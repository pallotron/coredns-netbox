package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"maps"

	"github.com/pallotron/coredns-netbox/internal/config"
	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/grpcserver"
	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/logging"
	"github.com/pallotron/coredns-netbox/internal/merge"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/reloader"
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

	// Dynamic store
	store, err := dynamicstore.NewFileStore(filepath.Join(cfg.ZoneDir, "dynamic.json"))
	if err != nil {
		slog.Error("failed to create dynamic store", "err", err)
		os.Exit(1)
	}

	// Shared state for gRPC ↔ poll loop coordination.
	//
	// mergeSignal and netboxSignal are buffered channels of size 1. This is an
	// intentional debounce: if a second signal arrives while one is already
	// pending in the buffer, the non-blocking send in the gRPC handler is
	// dropped. Rapid-fire ForceNetboxPoll or ForceMergeWrite calls therefore
	// coalesce into a single poll/merge cycle rather than queuing up redundant
	// work. The poll loop drains each channel before re-blocking on select, so
	// no signal is ever lost when the loop is idle.
	netboxCache := &grpcserver.NetboxCache{}
	statusTracker := &grpcserver.StatusTracker{}
	mergeSignal := make(chan struct{}, 1)
	netboxSignal := make(chan struct{}, 1)

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

		grpcSrv := grpcserver.New(cfg.GRPCAuthToken, store, netboxCache, statusTracker, mergeSignal, netboxSignal, mgr)
		lis, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			slog.Error("failed to listen for gRPC", "addr", cfg.GRPCAddr, "err", err)
			os.Exit(1)
		}
		slog.Info("gRPC server listening", "addr", cfg.GRPCAddr)
		go grpcSrv.Serve(lis)
		go func() {
			<-ctx.Done()
			grpcSrv.Stop()
		}()
	}

	rl := reloader.New(cfg.CoreDNSReloadAddrs, cfg.CoreDNSReloadToken)

	// Run the poll loop
	if err := run(ctx, cfg, client, categorizer, forwardDisc, reverseDisc, mgr, m, markReady,
		store, netboxCache, statusTracker, mergeSignal, netboxSignal, rl); err != nil {
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

func run(ctx context.Context, cfg *config.Config, client *netboxclient.Client,
	categorizer *ipcategorizer.Categorizer, forwardDisc, reverseDisc zonediscovery.Discoverer,
	mgr *zonemanager.Manager, m *metrics.Sidecar, markReady func(),
	store dynamicstore.DynamicStore, netboxCache *grpcserver.NetboxCache,
	statusTracker *grpcserver.StatusTracker,
	mergeSignal, netboxSignal <-chan struct{},
	rl *reloader.Reloader,
) error {
	lastSuccessTime := time.Now()
	firstSuccess := false

	doFetchNetbox := func() (zonediscovery.ZoneMap, error) {
		zm, err := fetchNetbox(ctx, client, categorizer, forwardDisc, reverseDisc, m)
		if err != nil || !cfg.StripDCLabel {
			return zm, err
		}
		for zone, recs := range zm {
			zm[zone] = zonediscovery.StripDCLabel(recs, cfg.DomainSuffix)
		}
		return zm, nil
	}

	doMergeAndWrite := func(netboxZones zonediscovery.ZoneMap) error {
		return merge.Write(netboxZones, store, mgr, m, reverseDisc)
	}

	// In run-once (zone-init) mode, skip the Netbox fetch if zone files already
	// exist on disk. This allows fast pod restarts when zoneStorage.persistent
	// is enabled: CoreDNS starts immediately from the existing PVC content while
	// the sidecar refreshes zones in the background on the next poll cycle.
	if cfg.RunOnce && mgr.HasExistingZones() {
		slog.Info("zone-init: zones already exist, skipping Netbox fetch")
		return nil
	}

	// Initial full poll
	netboxZones, fetchErr := doFetchNetbox()
	if fetchErr != nil {
		slog.Warn("initial netbox fetch failed", "err", fetchErr)
	} else {
		netboxCache.Update(netboxZones)
		statusTracker.SetNetboxPoll(time.Now())
	}
	if mergeErr := doMergeAndWrite(netboxZones); mergeErr != nil {
		slog.Warn("initial merge failed", "err", mergeErr)
	} else {
		if !cfg.RunOnce {
			rl.Reload(ctx)
		}
		lastSuccessTime = time.Now()
		statusTracker.SetMergeWrite(time.Now())
		if !firstSuccess && markReady != nil {
			firstSuccess = true
			markReady()
		}
	}

	if cfg.RunOnce {
		slog.Info("run-once mode, exiting")
		return runOnceResult(fetchErr, mgr.HasExistingZones())
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	lastNetboxZones := netboxZones

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			zones, err := doFetchNetbox()
			if err != nil {
				slog.Warn("poll error", "err", err)
				m.ZoneStalenessSeconds.Set(time.Since(lastSuccessTime).Seconds())
				statusTracker.SetStaleness(time.Since(lastSuccessTime).Seconds())
			} else {
				lastNetboxZones = zones
				netboxCache.Update(zones)
				statusTracker.SetNetboxPoll(time.Now())
			}
			if mergeErr := doMergeAndWrite(lastNetboxZones); mergeErr == nil {
				rl.Reload(ctx)
				lastSuccessTime = time.Now()
				m.ZoneStalenessSeconds.Set(0)
				statusTracker.SetMergeWrite(time.Now())
				statusTracker.SetStaleness(0)
				if !firstSuccess && markReady != nil {
					firstSuccess = true
					markReady()
				}
			}

		case <-netboxSignal:
			zones, err := doFetchNetbox()
			if err == nil {
				lastNetboxZones = zones
				netboxCache.Update(zones)
				statusTracker.SetNetboxPoll(time.Now())
			}
			if mergeErr := doMergeAndWrite(lastNetboxZones); mergeErr == nil {
				rl.Reload(ctx)
				statusTracker.SetMergeWrite(time.Now())
			}

		case <-mergeSignal:
			if mergeErr := doMergeAndWrite(lastNetboxZones); mergeErr == nil {
				rl.Reload(ctx)
				statusTracker.SetMergeWrite(time.Now())
			}
		}
	}
}

func fetchNetbox(ctx context.Context, client *netboxclient.Client,
	categorizer *ipcategorizer.Categorizer, forwardDisc, reverseDisc zonediscovery.Discoverer,
	m *metrics.Sidecar,
) (zonediscovery.ZoneMap, error) {
	slog.Info("fetching IP addresses from netbox")
	fetchStart := time.Now()
	records, err := client.FetchIPAddresses(ctx)
	m.NetboxFetchDurationSeconds.Observe(time.Since(fetchStart).Seconds())
	if err != nil {
		m.PollTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("fetch IP addresses: %w", err)
	}

	slog.Info("fetched records from netbox", "count", len(records))
	m.NetboxRecordsFetched.Set(float64(len(records)))
	if len(records) == 0 {
		m.NetboxEmptyResponseTotal.Inc()
	}

	enriched := enrichRecordsWithDeviceNames(records, categorizer)

	forwardZones, err := forwardDisc.Discover(enriched)
	if err != nil {
		return nil, fmt.Errorf("discover forward zones: %w", err)
	}
	slog.Info("discovered forward zones", "count", len(forwardZones))

	combined := forwardZones
	if reverseDisc != nil {
		reverseZones, err := reverseDisc.Discover(enriched)
		if err != nil {
			return nil, fmt.Errorf("discover reverse zones: %w", err)
		}
		slog.Info("discovered reverse zones", "count", len(reverseZones))
		maps.Copy(combined, reverseZones)
	}
	return combined, nil
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
