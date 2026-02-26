package zonediscovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/require"
)

func TestZoneDepthDiscoverer_Depth2(t *testing.T) {
	d := &ZoneDepthDiscoverer{Depth: 2}
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "db.prod.example.org", Address: "10.0.0.3", Family: 4},
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")

	require.Len(t, zm, 1, "expected 1 zone, got %v", zoneNames(zm))
	recs, ok := zm["example.org"]
	require.True(t, ok, "expected zone example.org")
	require.Len(t, recs, 3, "expected 3 records in example.org")
}

func TestZoneDepthDiscoverer_Depth3(t *testing.T) {
	d := &ZoneDepthDiscoverer{Depth: 3}
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "db.prod.example.org", Address: "10.0.0.3", Family: 4},
		{DNSName: "web.prod.example.org", Address: "10.0.0.4", Family: 4},
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")

	// host1.example.org only has 3 labels, so depth=3 => zone "host1.example.org"
	// but that's the FQDN itself. The zone-depth approach doesn't special-case this.
	_, ok := zm["prod.example.org"]
	require.True(t, ok, "expected zone prod.example.org, got zones: %v", zoneNames(zm))
	require.Len(t, zm["prod.example.org"], 2, "expected 2 records in prod.example.org")
}

func TestZoneDepthDiscoverer_SkipShortNames(t *testing.T) {
	d := &ZoneDepthDiscoverer{Depth: 3}
	records := []netboxclient.IPRecord{
		{DNSName: "example.org", Address: "10.0.0.1", Family: 4}, // only 2 labels
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")
	require.Empty(t, zm, "expected 0 zones for short name with depth=3")
}

func TestZoneDepthDiscoverer_InvalidDepth(t *testing.T) {
	d := &ZoneDepthDiscoverer{Depth: 1}
	_, err := d.Discover(nil)
	require.Error(t, err, "expected error for depth < 2")
}

func TestCommonSuffixDiscoverer_SingleZone(t *testing.T) {
	d := &CommonSuffixDiscoverer{}
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "db.example.org", Address: "10.0.0.3", Family: 4},
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")

	require.Len(t, zm, 1, "expected 1 zone, got %v", zoneNames(zm))
	_, ok := zm["example.org"]
	require.True(t, ok, "expected zone example.org, got zones: %v", zoneNames(zm))
}

func TestCommonSuffixDiscoverer_DeepCommonSuffix(t *testing.T) {
	d := &CommonSuffixDiscoverer{}
	records := []netboxclient.IPRecord{
		{DNSName: "host1.prod.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host2.prod.example.org", Address: "10.0.0.2", Family: 4},
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")

	_, ok := zm["prod.example.org"]
	require.True(t, ok, "expected zone prod.example.org, got zones: %v", zoneNames(zm))
}

func TestCommonSuffixDiscoverer_MixedTLDs(t *testing.T) {
	d := &CommonSuffixDiscoverer{}
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host2.example.com", Address: "10.0.0.2", Family: 4},
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")

	require.Len(t, zm, 2, "expected 2 zones for mixed TLDs, got %v", zoneNames(zm))
}

// TestCommonSuffixDiscoverer_FallbackA verifies that when records within a TLD
// group share no common depth >=2, the discoverer falls back to the 2-label
// suffix of the first record.
func TestCommonSuffixDiscoverer_FallbackA(t *testing.T) {
	d := &CommonSuffixDiscoverer{}
	// Both records share TLD .org, but different second-level domains.
	// Within the .org group, commonLen will be < 2, triggering Fallback A.
	records := []netboxclient.IPRecord{
		{DNSName: "host.alpha.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host.beta.org", Address: "10.0.0.2", Family: 4},
	}
	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")
	require.Len(t, zm, 1, "expected 1 zone for fallback, got %v", zoneNames(zm))
	// Fallback A: uses 2-label suffix of first record -> "alpha.org"
	_, ok := zm["alpha.org"]
	require.True(t, ok, "expected fallback zone alpha.org, got zones: %v", zoneNames(zm))
}

// TestCommonSuffixDiscoverer_FallbackB verifies that when the computed zone name
// equals all record FQDNs in the group, the discoverer truncates to the last
// 2-label suffix rather than returning a zone that equals a hostname.
func TestCommonSuffixDiscoverer_FallbackB(t *testing.T) {
	d := &CommonSuffixDiscoverer{}
	// Both records have the same FQDN "host.example.org".
	// commonSuffix computes zone "host.example.org", but that equals the FQDN,
	// so Fallback B truncates to "example.org".
	records := []netboxclient.IPRecord{
		{DNSName: "host.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host.example.org", Address: "10.0.0.2", Family: 4},
	}
	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")
	require.Len(t, zm, 1, "expected 1 zone for fallback, got %v", zoneNames(zm))
	// Fallback B: truncates to last 2 labels → "example.org"
	_, ok := zm["example.org"]
	require.True(t, ok, "expected fallback zone example.org, got zones: %v", zoneNames(zm))
}

func TestCommonSuffixDiscoverer_Empty(t *testing.T) {
	d := &CommonSuffixDiscoverer{}
	zm, err := d.Discover(nil)
	require.NoError(t, err, "unexpected error")
	require.Empty(t, zm, "expected 0 zones")
}

func TestNetboxDNSDiscoverer(t *testing.T) {
	// Set up a mock Netbox DNS API
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/plugins/netbox-dns/zones/" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		resp := netboxDNSZoneList{
			Count: 2,
			Results: []netboxDNSZone{
				{Name: "example.org", Status: struct {
					Value string `json:"value"`
				}{Value: "active"}},
				{Name: "prod.example.org", Status: struct {
					Value string `json:"value"`
				}{Value: "active"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	d := NewNetboxDNSDiscoverer(srv.URL, "test-token")
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "db.prod.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "web.prod.example.org", Address: "10.0.0.3", Family: 4},
	}

	zm, err := d.Discover(records)
	require.NoError(t, err, "unexpected error")

	// host1.example.org matches example.org
	// db.prod.example.org matches prod.example.org (longest match)
	// web.prod.example.org matches prod.example.org (longest match)
	require.Len(t, zm, 2, "expected 2 zones, got %v", zoneNames(zm))
	require.Len(t, zm["example.org"], 1, "expected 1 record in example.org")
	require.Len(t, zm["prod.example.org"], 2, "expected 2 records in prod.example.org")
}

func TestLongestMatchingZone(t *testing.T) {
	zones := []string{"example.org", "prod.example.org"}

	tests := []struct {
		fqdn     string
		expected string
	}{
		{"host1.example.org", "example.org"},
		{"db.prod.example.org", "prod.example.org"},
		{"host.other.com", ""},
	}

	for _, tt := range tests {
		got := longestMatchingZone(tt.fqdn, zones)
		if got != tt.expected {
			t.Errorf("longestMatchingZone(%q) = %q, want %q", tt.fqdn, got, tt.expected)
		}
	}
}

func TestNewDiscoverer_ZoneDepth(t *testing.T) {
	d, err := NewDiscoverer(ModeZoneDepth, map[string]string{"depth": "3"})
	require.NoError(t, err, "unexpected error")
	zd, ok := d.(*ZoneDepthDiscoverer)
	require.True(t, ok, "expected *ZoneDepthDiscoverer")
	require.Equal(t, 3, zd.Depth, "expected depth 3")
}

func TestNewDiscoverer_UnknownMode(t *testing.T) {
	_, err := NewDiscoverer("unknown", nil)
	require.Error(t, err, "expected error for unknown mode")
}

func zoneNames(zm ZoneMap) []string {
	var names []string
	for k := range zm {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
