package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/pallotron/coredns-netbox/internal/config"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
)

func main() {
	runOnce := flag.Bool("run-once", false, "Run once and exit (for init container mode)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.RunOnce = *runOnce

	client, err := netboxclient.New(cfg.NetboxURL, cfg.NetboxToken, cfg.PageSize, cfg.MaxConcurrency)
	if err != nil {
		log.Fatalf("Failed to create Netbox client: %v", err)
	}

	// Create forward zone discoverer
	opts := map[string]string{
		"depth":        strconv.Itoa(cfg.ZoneDepth),
		"netbox_url":   cfg.NetboxURL,
		"netbox_token": cfg.NetboxToken,
	}
	forwardDisc, err := zonediscovery.NewDiscoverer(zonediscovery.DiscoveryMode(cfg.DiscoveryMode), opts)
	if err != nil {
		log.Fatalf("Failed to create forward zone discoverer: %v", err)
	}

	// Create reverse zone discoverer if enabled
	var reverseDisc zonediscovery.Discoverer
	if cfg.EnableReverseZones {
		reverseDisc = zonediscovery.NewReverseZoneDiscoverer(
			cfg.ReverseZonesIPv4,
			cfg.ReverseZonesIPv6,
		)
		log.Printf("Reverse zones enabled: IPv4 zones=%v, IPv6 zones=%v",
			cfg.ReverseZonesIPv4, cfg.ReverseZonesIPv6)
	}

	mgr := zonemanager.New(cfg.ZoneDir, cfg.PrimaryNS, cfg.AdminEmail, cfg.TTL)

	log.Printf("Zone discovery mode: %s, zone dir: %s", cfg.DiscoveryMode, cfg.ZoneDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Start health check server (unless run-once mode)
	if !cfg.RunOnce {
		healthy := true
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			if healthy {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("not ok"))
			}
		})
		srv := &http.Server{Addr: cfg.HealthAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Health server error: %v", err)
			}
		}()
		go func() {
			<-ctx.Done()
			srv.Shutdown(context.Background())
		}()
		_ = healthy // used by closure
	}

	// Run the poll loop
	if err := run(ctx, cfg, client, forwardDisc, reverseDisc, mgr); err != nil {
		log.Fatalf("Fatal error: %v", err)
	}
}

func run(ctx context.Context, cfg *config.Config, client *netboxclient.Client, forwardDisc, reverseDisc zonediscovery.Discoverer, mgr *zonemanager.Manager) error {
	for {
		if err := poll(ctx, cfg, client, forwardDisc, reverseDisc, mgr); err != nil {
			log.Printf("Poll error: %v", err)
		}

		if cfg.RunOnce {
			log.Println("Run-once mode, exiting.")
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.PollInterval):
		}
	}
}

func poll(ctx context.Context, _ *config.Config, client *netboxclient.Client, forwardDisc, reverseDisc zonediscovery.Discoverer, mgr *zonemanager.Manager) error {
	log.Println("Fetching IP addresses from Netbox...")

	records, err := client.FetchIPAddresses(ctx)
	if err != nil {
		return fmt.Errorf("fetch IP addresses: %w", err)
	}

	log.Printf("Fetched %d records from Netbox", len(records))

	// Discover forward zones
	forwardZones, err := forwardDisc.Discover(records)
	if err != nil {
		return fmt.Errorf("discover forward zones: %w", err)
	}

	log.Printf("Discovered %d forward zones", len(forwardZones))

	// Discover reverse zones if enabled
	combinedZones := forwardZones
	if reverseDisc != nil {
		reverseZones, err := reverseDisc.Discover(records)
		if err != nil {
			return fmt.Errorf("discover reverse zones: %w", err)
		}
		log.Printf("Discovered %d reverse zones", len(reverseZones))

		// Merge forward and reverse zones
		for zone, recs := range reverseZones {
			combinedZones[zone] = recs
		}
	}

	if err := mgr.Update(combinedZones); err != nil {
		return fmt.Errorf("update zones: %w", err)
	}

	log.Printf("Zone update complete, active zones: %v", mgr.Zones())
	return nil
}
