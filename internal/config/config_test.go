package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allEnvKeys lists every environment variable that Load() reads.
var allEnvKeys = []string{
	"NETBOX_URL",
	"NETBOX_TOKEN",
	"DISCOVERY_MODE",
	"ZONE_DIR",
	"ZONE_DEPTH",
	"TTL",
	"POLL_INTERVAL",
	"NETBOX_PAGE_SIZE",
	"NETBOX_MAX_CONCURRENCY",
	"NETBOX_RETRY_COUNT",
	"NETBOX_RETRY_BASE_DELAY",
	"NETBOX_RETRY_MAX_DELAY",
	"HEALTH_ADDR",
	"PRIMARY_NS",
	"ADMIN_EMAIL",
	"BMC_INTERFACE_PATTERN",
	"LOOPBACK_PATTERN",
	"DATAPLANE_PATTERN",
	"MGMT_VRF_PATTERN",
	"MGMT_INTERFACE_PATTERN",
	"ENABLE_REVERSE_ZONES",
	"REVERSE_ZONES_IPV4",
	"REVERSE_ZONES_IPV6",
}

// clearEnv unsets all config-related env vars to ensure test isolation.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allEnvKeys {
		_ = os.Unsetenv(k)
	}
}

func TestLoad_RequiresNetboxToken(t *testing.T) {
	clearEnv(t)

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NETBOX_TOKEN is required")
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("NETBOX_TOKEN", "test-token")

	cfg, err := Load()
	require.NoError(t, err)

	// Core fields
	assert.Equal(t, "http://netbox.netbox.svc.cluster.local", cfg.NetboxURL)
	assert.Equal(t, "test-token", cfg.NetboxToken)
	assert.Equal(t, "zone-depth", cfg.DiscoveryMode)
	assert.Equal(t, "/zones", cfg.ZoneDir)
	assert.Equal(t, 2, cfg.ZoneDepth)
	assert.Equal(t, uint32(300), cfg.TTL)
	assert.Equal(t, 1000, cfg.PageSize)
	assert.Equal(t, 10, cfg.MaxConcurrency)
	assert.Equal(t, 60*time.Second, cfg.PollInterval)

	// Retry defaults
	assert.Equal(t, 3, cfg.NetboxRetryCount)
	assert.Equal(t, 1*time.Second, cfg.NetboxRetryBaseDelay)
	assert.Equal(t, 30*time.Second, cfg.NetboxRetryMaxDelay)

	// Health / SOA
	assert.Equal(t, ":8082", cfg.HealthAddr)
	assert.Equal(t, "ns1.example.org.", cfg.PrimaryNS)
	assert.Equal(t, "admin.example.org.", cfg.AdminEmail)

	// Interface patterns
	assert.Equal(t, "(?i)bmc|ipmi|ilo|idrac", cfg.BMCInterfacePattern)
	assert.Equal(t, "^lo$|^lo0|^Loopback", cfg.LoopbackInterfacePattern)
	assert.Equal(t, "(?i)storage|vtep|vsan", cfg.DataplaneInterfacePattern)
	assert.Equal(t, "(?i)mgmt|oob", cfg.MgmtVRFPattern)
	assert.Equal(t, "(?i)mgmt|Management|fxp0|eth[01]|mgt|NET", cfg.MgmtInterfacePattern)

	// Reverse zones
	assert.True(t, cfg.EnableReverseZones)
	assert.Equal(t, []string{"10.in-addr.arpa", "172.16.in-addr.arpa"}, cfg.ReverseZonesIPv4)
	assert.Nil(t, cfg.ReverseZonesIPv6)
}

func TestLoad_NumericAndDurationOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("NETBOX_TOKEN", "tok")
	t.Setenv("ZONE_DEPTH", "5")
	t.Setenv("TTL", "600")
	t.Setenv("POLL_INTERVAL", "120s")
	t.Setenv("NETBOX_PAGE_SIZE", "500")
	t.Setenv("NETBOX_MAX_CONCURRENCY", "20")
	t.Setenv("NETBOX_RETRY_COUNT", "5")
	t.Setenv("NETBOX_RETRY_BASE_DELAY", "2s")
	t.Setenv("NETBOX_RETRY_MAX_DELAY", "1m")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 5, cfg.ZoneDepth)
	assert.Equal(t, uint32(600), cfg.TTL)
	assert.Equal(t, 120*time.Second, cfg.PollInterval)
	assert.Equal(t, 500, cfg.PageSize)
	assert.Equal(t, 20, cfg.MaxConcurrency)
	assert.Equal(t, 5, cfg.NetboxRetryCount)
	assert.Equal(t, 2*time.Second, cfg.NetboxRetryBaseDelay)
	assert.Equal(t, 1*time.Minute, cfg.NetboxRetryMaxDelay)
}

func TestLoad_InvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		envKey  string
		envVal  string
		wantErr string
	}{
		{"bad ZONE_DEPTH", "ZONE_DEPTH", "abc", "invalid ZONE_DEPTH"},
		{"bad TTL", "TTL", "-1", "invalid TTL"},
		{"bad TTL float", "TTL", "3.5", "invalid TTL"},
		{"bad POLL_INTERVAL", "POLL_INTERVAL", "notduration", "invalid POLL_INTERVAL"},
		{"bad NETBOX_PAGE_SIZE", "NETBOX_PAGE_SIZE", "xyz", "invalid NETBOX_PAGE_SIZE"},
		{"bad NETBOX_MAX_CONCURRENCY", "NETBOX_MAX_CONCURRENCY", "!!", "invalid NETBOX_MAX_CONCURRENCY"},
		{"bad NETBOX_RETRY_COUNT", "NETBOX_RETRY_COUNT", "no", "invalid NETBOX_RETRY_COUNT"},
		{"bad NETBOX_RETRY_BASE_DELAY", "NETBOX_RETRY_BASE_DELAY", "bad", "invalid NETBOX_RETRY_BASE_DELAY"},
		{"bad NETBOX_RETRY_MAX_DELAY", "NETBOX_RETRY_MAX_DELAY", "bad", "invalid NETBOX_RETRY_MAX_DELAY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NETBOX_TOKEN", "tok")
			t.Setenv(tt.envKey, tt.envVal)

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoad_ReverseZones(t *testing.T) {
	t.Run("disable reverse zones", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NETBOX_TOKEN", "tok")
		t.Setenv("ENABLE_REVERSE_ZONES", "false")

		cfg, err := Load()
		require.NoError(t, err)
		assert.False(t, cfg.EnableReverseZones)
	})

	t.Run("custom IPv4 zone list", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NETBOX_TOKEN", "tok")
		t.Setenv("REVERSE_ZONES_IPV4", "192.168.in-addr.arpa,10.in-addr.arpa")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"192.168.in-addr.arpa", "10.in-addr.arpa"}, cfg.ReverseZonesIPv4)
	})

	t.Run("custom IPv6 zone list", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NETBOX_TOKEN", "tok")
		t.Setenv("REVERSE_ZONES_IPV6", "8.b.d.0.1.0.0.2.ip6.arpa")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"8.b.d.0.1.0.0.2.ip6.arpa"}, cfg.ReverseZonesIPv6)
	})

	t.Run("whitespace trimming in zone lists", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NETBOX_TOKEN", "tok")
		t.Setenv("REVERSE_ZONES_IPV4", "  10.in-addr.arpa , 172.16.in-addr.arpa  ")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"10.in-addr.arpa", "172.16.in-addr.arpa"}, cfg.ReverseZonesIPv4)
	})

	t.Run("empty env var falls back to default", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NETBOX_TOKEN", "tok")
		// Setting to empty string triggers envOrDefault fallback to the default value.
		t.Setenv("REVERSE_ZONES_IPV4", "")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, []string{"10.in-addr.arpa", "172.16.in-addr.arpa"}, cfg.ReverseZonesIPv4)
	})

	t.Run("unset IPv6 returns nil", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NETBOX_TOKEN", "tok")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Nil(t, cfg.ReverseZonesIPv6)
	})
}
