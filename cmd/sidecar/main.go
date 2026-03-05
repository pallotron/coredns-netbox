package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pallotron/coredns-netbox/internal/config"
	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/logging"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	// Configure log level from environment
	logLevel := slog.LevelInfo
	if level := os.Getenv("LOG_LEVEL"); level != "" {
		switch level {
		case "DEBUG", "debug":
			logLevel = slog.LevelDebug
		case "INFO", "info":
			logLevel = slog.LevelInfo
		case "WARN", "warn", "WARNING", "warning":
			logLevel = slog.LevelWarn
		case "ERROR", "error":
			logLevel = slog.LevelError
		}
	}
	slog.SetDefault(slog.New(logging.NewGCPHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})))

	runOnce := flag.Bool("run-once", false, "Run once and exit (for init container mode)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	cfg.RunOnce = *runOnce

	client, err := netboxclient.New(cfg.NetboxURL, cfg.NetboxToken, cfg.PageSize, cfg.MaxConcurrency,
		cfg.NetboxRetryCount, cfg.NetboxRetryBaseDelay, cfg.NetboxRetryMaxDelay)
	if err != nil {
		slog.Error("failed to create netbox client", "err", err)
		os.Exit(1)
	}

	// Create IP categorizer for device-based DNS record generation
	categorizer, err := ipcategorizer.NewCategorizer(
		cfg.BMCInterfacePattern,
		cfg.LoopbackInterfacePattern,
		cfg.DataplaneInterfacePattern,
		cfg.MgmtVRFPattern,
		cfg.MgmtInterfacePattern,
		cfg.DomainSuffix,
	)
	if err != nil {
		slog.Error("failed to create IP categorizer", "err", err)
		os.Exit(1)
	}

	// Create forward zone discoverer
	opts := map[string]string{
		"depth":        strconv.Itoa(cfg.ZoneDepth),
		"netbox_url":   cfg.NetboxURL,
		"netbox_token": cfg.NetboxToken,
	}
	forwardDisc, err := zonediscovery.NewDiscoverer(zonediscovery.DiscoveryMode(cfg.DiscoveryMode), opts)
	if err != nil {
		slog.Error("failed to create forward zone discoverer", "err", err)
		os.Exit(1)
	}

	// Create reverse zone discoverer if enabled
	var reverseDisc zonediscovery.Discoverer
	if cfg.EnableReverseZones {
		reverseDisc = zonediscovery.NewReverseZoneDiscoverer(
			cfg.ReverseZonesIPv4,
			cfg.ReverseZonesIPv6,
		)
		slog.Info("reverse zones enabled", "ipv4_zones", cfg.ReverseZonesIPv4, "ipv6_zones", cfg.ReverseZonesIPv6)
	}

	mgr := zonemanager.New(cfg.ZoneDir, cfg.PrimaryNS, cfg.AdminEmail, cfg.TTL)

	slog.Info("starting sidecar", "discovery_mode", cfg.DiscoveryMode, "zone_dir", cfg.ZoneDir)

	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)
	client.RetryCounter = m.NetboxFetchRetriesTotal

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Start health check server (unless run-once mode)
	var markReady func()
	if !cfg.RunOnce {
		var healthy atomic.Bool
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			if healthy.Load() {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("not ok"))
			}
		})
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := &http.Server{Addr: cfg.HealthAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("health server error", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			if err := srv.Shutdown(context.Background()); err != nil {
				slog.Error("health server shutdown error", "err", err)
			}
		}()
		markReady = func() { healthy.Store(true) }
	}

	// Run the poll loop
	if err := run(ctx, cfg, client, categorizer, forwardDisc, reverseDisc, mgr, m, markReady); err != nil {
		slog.Error("fatal error", "err", err)
		os.Exit(1)
	}
}

// runOnceResult decides the outcome of --run-once mode.
// If the poll succeeded, return nil.
// If the poll failed but cached zone files exist on disk, warn and return nil
// so the init container exits 0 and CoreDNS serves the pre-existing cache.
// If the poll failed and there are no cached zone files, return the poll error
// so the init container exits non-zero and Kubernetes retries.
func runOnceResult(pollErr error, hasCachedZones bool) error {
	if pollErr == nil {
		return nil
	}
	if hasCachedZones {
		slog.Warn("Netbox unavailable on init, serving from cached zone files")
		return nil
	}
	return pollErr
}

func run(ctx context.Context, cfg *config.Config, client *netboxclient.Client, categorizer *ipcategorizer.Categorizer, forwardDisc, reverseDisc zonediscovery.Discoverer, mgr *zonemanager.Manager, m *metrics.Sidecar, markReady func()) error {
	firstSuccess := false
	lastSuccessTime := time.Now()
	for {
		pollErr := poll(ctx, cfg, client, categorizer, forwardDisc, reverseDisc, mgr, m)
		if pollErr != nil {
			slog.Warn("poll error", "err", pollErr)
			m.ZoneStalenessSeconds.Set(time.Since(lastSuccessTime).Seconds())
		} else {
			lastSuccessTime = time.Now()
			m.ZoneStalenessSeconds.Set(0)
			if !firstSuccess && markReady != nil {
				firstSuccess = true
				markReady()
			}
		}

		if cfg.RunOnce {
			slog.Info("run-once mode, exiting")
			return runOnceResult(pollErr, mgr.HasExistingZones())
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.PollInterval):
		}
	}
}

func poll(ctx context.Context, _ *config.Config, client *netboxclient.Client, categorizer *ipcategorizer.Categorizer, forwardDisc, reverseDisc zonediscovery.Discoverer, mgr *zonemanager.Manager, m *metrics.Sidecar) error {
	pollStart := time.Now()

	slog.Info("fetching IP addresses from netbox")

	fetchStart := time.Now()
	records, err := client.FetchIPAddresses(ctx)
	m.NetboxFetchDurationSeconds.Observe(time.Since(fetchStart).Seconds())
	if err != nil {
		m.PollTotal.WithLabelValues("error").Inc()
		m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())
		return fmt.Errorf("fetch IP addresses: %w", err)
	}

	slog.Info("fetched records from netbox", "count", len(records))
	m.NetboxRecordsFetched.Set(float64(len(records)))
	if len(records) == 0 {
		m.NetboxEmptyResponseTotal.Inc()
	}

	// Hybrid approach: combine records with existing dns_name and device-generated names
	enrichedRecords := enrichRecordsWithDeviceNames(records, categorizer)

	// Discover forward zones from enriched records
	forwardZones, err := forwardDisc.Discover(enrichedRecords)
	if err != nil {
		m.PollTotal.WithLabelValues("error").Inc()
		m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())
		return fmt.Errorf("discover forward zones: %w", err)
	}

	slog.Info("discovered forward zones", "count", len(forwardZones))

	// Discover reverse zones if enabled (use enriched records)
	combinedZones := forwardZones
	if reverseDisc != nil {
		reverseZones, err := reverseDisc.Discover(enrichedRecords)
		if err != nil {
			m.PollTotal.WithLabelValues("error").Inc()
			m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())
			return fmt.Errorf("discover reverse zones: %w", err)
		}
		slog.Info("discovered reverse zones", "count", len(reverseZones))

		// Merge forward and reverse zones
		for zone, recs := range reverseZones {
			combinedZones[zone] = recs
		}
	}

	stats, err := mgr.Update(combinedZones)
	m.ZoneWritesTotal.WithLabelValues("create").Add(float64(stats.Created))
	m.ZoneWritesTotal.WithLabelValues("update").Add(float64(stats.Updated))
	m.ZoneWritesTotal.WithLabelValues("delete").Add(float64(stats.Deleted))
	if stats.WriteErrors > 0 {
		m.ZoneWriteErrorsTotal.Add(float64(stats.WriteErrors))
	}
	if err != nil {
		m.PollTotal.WithLabelValues("error").Inc()
		m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())
		return fmt.Errorf("update zones: %w", err)
	}

	m.ZonesActive.Set(float64(len(mgr.Zones())))
	m.LastSuccessfulPollTimestamp.SetToCurrentTime()
	m.PollTotal.WithLabelValues("success").Inc()
	m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())

	slog.Info("zone update complete", "active_zones", mgr.Zones())
	return nil
}

// enrichRecordsWithDeviceNames implements a hybrid approach:
// - Keep records that already have dns_name populated
// - Generate DNS names from device names for records without dns_name
func enrichRecordsWithDeviceNames(records []netboxclient.IPRecord, categorizer *ipcategorizer.Categorizer) []netboxclient.IPRecord {
	var withDNSName []netboxclient.IPRecord
	var withoutDNSName []netboxclient.IPRecord

	// Split records based on whether dns_name is populated
	for _, rec := range records {
		if rec.DNSName != "" {
			withDNSName = append(withDNSName, rec)
		} else {
			withoutDNSName = append(withoutDNSName, rec)
		}
	}

	slog.Info("record split", "with_dns_name", len(withDNSName), "without_dns_name", len(withoutDNSName))

	// Generate device-based DNS records for those without dns_name
	deviceDNS := categorizer.SelectDeviceIPs(withoutDNSName)
	generatedRecords := deviceDNSToRecords(deviceDNS)

	slog.Info("generated device-based records", "devices", len(deviceDNS), "records", len(generatedRecords))

	// Merge: keep existing dns_name records and add generated ones
	result := make([]netboxclient.IPRecord, 0, len(withDNSName)+len(generatedRecords))
	result = append(result, withDNSName...)
	result = append(result, generatedRecords...)

	return result
}

// deviceDNSToRecords converts device DNS records to IPRecord format
func deviceDNSToRecords(deviceDNS map[string]*ipcategorizer.DeviceDNSRecords) []netboxclient.IPRecord {
	var records []netboxclient.IPRecord

	// Sort device names for deterministic output
	deviceNames := make([]string, 0, len(deviceDNS))
	for deviceName := range deviceDNS {
		deviceNames = append(deviceNames, deviceName)
	}
	sort.Strings(deviceNames)

	for _, deviceName := range deviceNames {
		dns := deviceDNS[deviceName]
		// Primary management IP: devicename.zone
		if dns.PrimaryIP != nil {
			fqdn := deviceName + "." + dns.Zone
			records = append(records, netboxclient.IPRecord{
				DNSName:       fqdn,
				Address:       dns.PrimaryIP.Address,
				Family:        dns.PrimaryIP.Family,
				DeviceName:    deviceName,
				InterfaceName: dns.PrimaryIP.InterfaceName,
				VRF:           dns.PrimaryIP.VRF,
			})
		}

		// BMC IP: devicename-bmc.zone
		if dns.BMCIP != nil {
			fqdn := deviceName + "-bmc." + dns.Zone
			records = append(records, netboxclient.IPRecord{
				DNSName:       fqdn,
				Address:       dns.BMCIP.Address,
				Family:        dns.BMCIP.Family,
				DeviceName:    deviceName,
				InterfaceName: dns.BMCIP.InterfaceName,
				VRF:           dns.BMCIP.VRF,
			})
		}
	}

	return records
}
