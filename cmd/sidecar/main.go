package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pallotron/coredns-netbox/internal/config"
	"github.com/pallotron/coredns-netbox/internal/logging"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	slog.SetDefault(slog.New(logging.NewGCPHandler(os.Stderr, nil)))

	runOnce := flag.Bool("run-once", false, "Run once and exit (for init container mode)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	cfg.RunOnce = *runOnce

	client, err := netboxclient.New(cfg.NetboxURL, cfg.NetboxToken, cfg.PageSize, cfg.MaxConcurrency)
	if err != nil {
		slog.Error("failed to create netbox client", "err", err)
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
	if err := run(ctx, cfg, client, forwardDisc, reverseDisc, mgr, m, markReady); err != nil {
		slog.Error("fatal error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config, client *netboxclient.Client, forwardDisc, reverseDisc zonediscovery.Discoverer, mgr *zonemanager.Manager, m *metrics.Sidecar, markReady func()) error {
	firstSuccess := false
	for {
		if err := poll(ctx, cfg, client, forwardDisc, reverseDisc, mgr, m); err != nil {
			slog.Warn("poll error", "err", err)
		} else if !firstSuccess && markReady != nil {
			firstSuccess = true
			markReady()
		}

		if cfg.RunOnce {
			slog.Info("run-once mode, exiting")
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.PollInterval):
		}
	}
}

func poll(ctx context.Context, _ *config.Config, client *netboxclient.Client, forwardDisc, reverseDisc zonediscovery.Discoverer, mgr *zonemanager.Manager, m *metrics.Sidecar) error {
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

	// Discover forward zones
	forwardZones, err := forwardDisc.Discover(records)
	if err != nil {
		m.PollTotal.WithLabelValues("error").Inc()
		m.PollDurationSeconds.Observe(time.Since(pollStart).Seconds())
		return fmt.Errorf("discover forward zones: %w", err)
	}

	slog.Info("discovered forward zones", "count", len(forwardZones))

	// Discover reverse zones if enabled
	combinedZones := forwardZones
	if reverseDisc != nil {
		reverseZones, err := reverseDisc.Discover(records)
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
