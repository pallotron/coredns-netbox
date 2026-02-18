package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds the sidecar configuration populated from environment variables.
type Config struct {
	NetboxURL      string
	NetboxToken    string
	DiscoveryMode  string
	ZoneDepth      int
	ZoneDir        string
	PollInterval   time.Duration
	TTL            uint32
	PageSize       int
	MaxConcurrency int
	RunOnce        bool
	HealthAddr     string
	PrimaryNS      string
	AdminEmail     string

	// Interface categorization patterns (regex)
	BMCInterfacePattern      string
	LoopbackInterfacePattern string
	DataplaneInterfacePattern string
	MgmtVRFPattern           string
	MgmtInterfacePattern     string
}

func Load() (*Config, error) {
	c := &Config{
		NetboxURL:      envOrDefault("NETBOX_URL", "http://netbox.netbox.svc.cluster.local"),
		NetboxToken:    os.Getenv("NETBOX_TOKEN"),
		DiscoveryMode:  envOrDefault("DISCOVERY_MODE", "zone-depth"),
		ZoneDir:        envOrDefault("ZONE_DIR", "/zones"),
		ZoneDepth:      2,
		TTL:            300,
		PageSize:       1000,
		MaxConcurrency: 10,
		HealthAddr:     envOrDefault("HEALTH_ADDR", ":8082"),
		PrimaryNS:      envOrDefault("PRIMARY_NS", "ns1.example.org."),
		AdminEmail:     envOrDefault("ADMIN_EMAIL", "admin.example.org."),

		// Interface categorization patterns
		BMCInterfacePattern:       envOrDefault("BMC_INTERFACE_PATTERN", "(?i)bmc|ipmi|ilo|idrac"),
		LoopbackInterfacePattern:  envOrDefault("LOOPBACK_PATTERN", "^lo$|^lo0|^Loopback"),
		DataplaneInterfacePattern: envOrDefault("DATAPLANE_PATTERN", "(?i)storage|vtep|vsan"),
		MgmtVRFPattern:            envOrDefault("MGMT_VRF_PATTERN", "(?i)mgmt|oob"),
		MgmtInterfacePattern:      envOrDefault("MGMT_INTERFACE_PATTERN", "(?i)mgmt|Management|fxp0|eth[01]|mgt|NET"),
	}

	if c.NetboxToken == "" {
		return nil, fmt.Errorf("NETBOX_TOKEN is required")
	}

	if v := os.Getenv("ZONE_DEPTH"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid ZONE_DEPTH: %w", err)
		}
		c.ZoneDepth = d
	}

	if v := os.Getenv("TTL"); v != "" {
		ttl, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid TTL: %w", err)
		}
		c.TTL = uint32(ttl)
	}

	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
		}
		c.PollInterval = d
	} else {
		c.PollInterval = 60 * time.Second
	}

	if v := os.Getenv("NETBOX_PAGE_SIZE"); v != "" {
		ps, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NETBOX_PAGE_SIZE: %w", err)
		}
		c.PageSize = ps
	}

	if v := os.Getenv("NETBOX_MAX_CONCURRENCY"); v != "" {
		mc, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NETBOX_MAX_CONCURRENCY: %w", err)
		}
		c.MaxConcurrency = mc
	}

	return c, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
