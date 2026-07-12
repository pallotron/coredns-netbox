package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pallotron/coredns-netbox/internal/nameformat"
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
	MaxConcurrency      int
	NetboxRetryCount     int
	NetboxRetryBaseDelay time.Duration
	NetboxRetryMaxDelay  time.Duration
	RunOnce             bool
	HealthAddr     string
	PrimaryNS      string
	AdminEmail     string

	// Interface categorization patterns (regex)
	BMCInterfacePattern       string
	LoopbackInterfacePattern  string
	DataplaneInterfacePattern string
	MgmtVRFPattern            string
	MgmtInterfacePattern      string

	// SOA timers (seconds) for generated zones. Refresh/retry control how
	// often secondaries poll for updates; expire is how long they keep
	// serving after the primary becomes unreachable.
	SOARefresh uint32
	SOARetry   uint32
	SOAExpire  uint32
	SOAMinimum uint32

	// DNS domain configuration
	DomainSuffix  string
	StripDCLabel  bool

	// Device name parsing and templating (issue #60). Parsers is an ordered
	// list of RE2 regexes with named capture groups (first match wins);
	// formats are text/template strings rendering complete FQDNs. See
	// internal/nameformat.
	DeviceNameParsers   []string
	NameFormatCanonical string
	NameFormatAliases   []string
	NameFormatZone      string

	// Reverse zone configuration
	EnableReverseZones bool
	ReverseZonesIPv4   []string // Static IPv4 reverse zones (e.g., ["10.in-addr.arpa"])
	ReverseZonesIPv6   []string // Static IPv6 reverse zones (e.g., ["b.8.0.d.0.1.2.0.ip6.arpa"])

	// gRPC server configuration
	GRPCAddr      string
	GRPCAuthToken string

	// Netbox webhook trigger: HMAC secret for the /webhook/netbox route.
	// Empty disables the route entirely (see internal/netboxwebhook.Register).
	NetboxWebhookSecret string

	// CoreDNS reload notification
	CoreDNSReloadAddrs []string // host:port addresses to call Reload() on after zone write
	CoreDNSReloadToken string   // bearer token for CoreDNS gRPC reload (usually empty)
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
		MaxConcurrency:      10,
		NetboxRetryCount:     3,
		NetboxRetryBaseDelay: 1 * time.Second,
		NetboxRetryMaxDelay:  30 * time.Second,
		HealthAddr:          envOrDefault("HEALTH_ADDR", ":8082"),
		PrimaryNS:      envOrDefault("PRIMARY_NS", "ns1.example.org."),
		AdminEmail:     envOrDefault("ADMIN_EMAIL", "admin.example.org."),
		SOARefresh:     3600,
		SOARetry:       900,
		SOAExpire:      604800,
		SOAMinimum:     86400,

		// Interface categorization patterns
		BMCInterfacePattern:       envOrDefault("BMC_INTERFACE_PATTERN", "(?i)bmc|ipmi|ilo|idrac"),
		LoopbackInterfacePattern:  envOrDefault("LOOPBACK_PATTERN", "^lo$|^lo0|^Loopback"),
		DataplaneInterfacePattern: envOrDefault("DATAPLANE_PATTERN", "(?i)storage|vtep|vsan"),
		MgmtVRFPattern:            envOrDefault("MGMT_VRF_PATTERN", "(?i)mgmt|oob"),
		MgmtInterfacePattern:      envOrDefault("MGMT_INTERFACE_PATTERN", "(?i)mgmt|Management|fxp0|eth[01]|mgt|NET|^ens[0-9]|^eno[0-9]|^nic[0-9]"),

		// DNS domain configuration
		DomainSuffix: envOrDefault("DOMAIN_SUFFIX", "example.org"),
		StripDCLabel: envOrDefault("STRIP_DC_LABEL", "false") == "true",

		// Device name parsing and templating (issue #60)
		DeviceNameParsers:   nameformat.SplitLines(os.Getenv("DEVICE_NAME_PARSERS")),
		NameFormatCanonical: os.Getenv("NAME_FORMAT_CANONICAL"),
		NameFormatAliases:   nameformat.SplitLines(os.Getenv("NAME_FORMAT_ALIASES")),
		NameFormatZone:      os.Getenv("NAME_FORMAT_ZONE"),

		// Reverse zone defaults
		EnableReverseZones: envOrDefault("ENABLE_REVERSE_ZONES", "true") == "true",
		ReverseZonesIPv4:   parseZoneList(envOrDefault("REVERSE_ZONES_IPV4", "10.in-addr.arpa,172.16.in-addr.arpa")),
		ReverseZonesIPv6:   parseZoneList(envOrDefault("REVERSE_ZONES_IPV6", "")),

		// gRPC server configuration
		GRPCAddr:      envOrDefault("GRPC_ADDR", ":8083"),
		GRPCAuthToken: os.Getenv("GRPC_AUTH_TOKEN"),

		NetboxWebhookSecret: os.Getenv("NETBOX_WEBHOOK_SECRET"),

		CoreDNSReloadAddrs: parseZoneList(os.Getenv("COREDNS_RELOAD_ADDRS")),
		CoreDNSReloadToken: os.Getenv("COREDNS_RELOAD_TOKEN"),
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

	soaTimers := map[string]*uint32{
		"SOA_REFRESH": &c.SOARefresh,
		"SOA_RETRY":   &c.SOARetry,
		"SOA_EXPIRE":  &c.SOAExpire,
		"SOA_MINIMUM": &c.SOAMinimum,
	}
	for name, dst := range soaTimers {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid %s: %w", name, err)
			}
			*dst = uint32(n)
		}
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

	if v := os.Getenv("NETBOX_RETRY_COUNT"); v != "" {
		rc, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NETBOX_RETRY_COUNT: %w", err)
		}
		c.NetboxRetryCount = rc
	}

	if v := os.Getenv("NETBOX_RETRY_BASE_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NETBOX_RETRY_BASE_DELAY: %w", err)
		}
		c.NetboxRetryBaseDelay = d
	}

	if v := os.Getenv("NETBOX_RETRY_MAX_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NETBOX_RETRY_MAX_DELAY: %w", err)
		}
		c.NetboxRetryMaxDelay = d
	}

	return c, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseZoneList(s string) []string {
	if s == "" {
		return nil
	}
	var zones []string
	for _, z := range strings.Split(s, ",") {
		z = strings.TrimSpace(z)
		if z != "" {
			zones = append(zones, z)
		}
	}
	return zones
}
