package zonediscovery

import (
	"net"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReverseZoneDiscoverer_IPv4(t *testing.T) {
	tests := []struct {
		name       string
		ipv4Zones  []string
		records    []netboxclient.IPRecord
		wantZones  map[string]int        // zone -> record count
		wantPTRs   map[string]string     // zone -> first PTR name
	}{
		{
			name:      "Single parent zone covers all",
			ipv4Zones: []string{"10.in-addr.arpa"},
			records: []netboxclient.IPRecord{
				{DNSName: "server1.example.com", Address: "10.1.2.3", Family: 4},
				{DNSName: "server2.example.com", Address: "10.1.2.4", Family: 4},
				{DNSName: "server3.example.com", Address: "10.2.3.4", Family: 4},
			},
			wantZones: map[string]int{
				"10.in-addr.arpa": 3,
			},
			wantPTRs: map[string]string{
				"10.in-addr.arpa": "3.2.1",
			},
		},
		{
			name:      "Multiple /16 zones",
			ipv4Zones: []string{"1.10.in-addr.arpa", "2.10.in-addr.arpa"},
			records: []netboxclient.IPRecord{
				{DNSName: "server1.example.com", Address: "10.1.2.3", Family: 4},
				{DNSName: "server2.example.com", Address: "10.1.2.4", Family: 4},
				{DNSName: "server3.example.com", Address: "10.2.3.4", Family: 4},
			},
			wantZones: map[string]int{
				"1.10.in-addr.arpa": 2,
				"2.10.in-addr.arpa": 1,
			},
			wantPTRs: map[string]string{
				"1.10.in-addr.arpa": "3.2",
				"2.10.in-addr.arpa": "4.3",
			},
		},
		{
			name:      "Skip records without DNS names",
			ipv4Zones: []string{"10.in-addr.arpa"},
			records: []netboxclient.IPRecord{
				{DNSName: "", Address: "10.1.2.3", Family: 4},
				{DNSName: "server1.example.com", Address: "10.1.2.4", Family: 4},
			},
			wantZones: map[string]int{
				"10.in-addr.arpa": 1,
			},
			wantPTRs: map[string]string{
				"10.in-addr.arpa": "4.2.1",
			},
		},
		{
			name:      "IP not in configured zones",
			ipv4Zones: []string{"10.in-addr.arpa"},
			records: []netboxclient.IPRecord{
				{DNSName: "server1.example.com", Address: "172.16.1.1", Family: 4},
			},
			wantZones: map[string]int{},
			wantPTRs:  map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disc := NewReverseZoneDiscoverer(tt.ipv4Zones, nil)
			zones, err := disc.Discover(tt.records)
			require.NoError(t, err)

			// Check zone counts
			if len(zones) != len(tt.wantZones) {
				t.Errorf("got %d zones, want %d", len(zones), len(tt.wantZones))
			}

			for zone, wantCount := range tt.wantZones {
				records, ok := zones[zone]
				if !ok {
					t.Errorf("zone %q not found", zone)
					continue
				}
				if len(records) != wantCount {
					t.Errorf("zone %q has %d records, want %d", zone, len(records), wantCount)
				}

				// Check PTR name format
				if wantPTR, ok := tt.wantPTRs[zone]; ok && len(records) > 0 {
					gotPTR := records[0].Address
					if gotPTR != wantPTR {
						t.Errorf("zone %q first PTR = %q, want %q", zone, gotPTR, wantPTR)
					}
				}
			}
		})
	}
}

func TestReverseZoneDiscoverer_IPv6(t *testing.T) {
	tests := []struct {
		name          string
		ipv6Zones     []string
		records       []netboxclient.IPRecord
		wantZoneCount int
	}{
		{
			name:      "IPv6 single parent zone (2001:db8::/32)",
			ipv6Zones: []string{"b.8.0.d.0.1.2.0.ip6.arpa"},
			records: []netboxclient.IPRecord{
				{DNSName: "server1.example.com", Address: "2001:db8::1", Family: 6},
				{DNSName: "server2.example.com", Address: "2001:db8::2", Family: 6},
			},
			wantZoneCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disc := NewReverseZoneDiscoverer(nil, tt.ipv6Zones)
			zones, err := disc.Discover(tt.records)
			require.NoError(t, err)

			if len(zones) != tt.wantZoneCount {
				t.Errorf("got %d zones, want %d", len(zones), tt.wantZoneCount)
			}

			// Verify all zones end with .ip6.arpa
			for zone := range zones {
				if !hasIP6ArpaSuffix(zone) {
					t.Errorf("zone %q doesn't end with .ip6.arpa", zone)
				}
			}
		})
	}
}

func TestReverseIPv4(t *testing.T) {
	tests := []struct {
		name      string
		zones     []string
		ip        string
		wantZone  string
		wantPTR   string
	}{
		{
			name:     "10.1.2.3 with /8 parent zone",
			zones:    []string{"10.in-addr.arpa"},
			ip:       "10.1.2.3",
			wantZone: "10.in-addr.arpa",
			wantPTR:  "3.2.1",
		},
		{
			name:     "10.1.2.3 with /16 parent zone",
			zones:    []string{"1.10.in-addr.arpa"},
			ip:       "10.1.2.3",
			wantZone: "1.10.in-addr.arpa",
			wantPTR:  "3.2",
		},
		{
			name:     "10.1.2.3 with /24 parent zone",
			zones:    []string{"2.1.10.in-addr.arpa"},
			ip:       "10.1.2.3",
			wantZone: "2.1.10.in-addr.arpa",
			wantPTR:  "3",
		},
		{
			name:     "192.168.1.100 with /16",
			zones:    []string{"168.192.in-addr.arpa"},
			ip:       "192.168.1.100",
			wantZone: "168.192.in-addr.arpa",
			wantPTR:  "100.1",
		},
		{
			name:     "Choose most specific zone",
			zones:    []string{"10.in-addr.arpa", "1.10.in-addr.arpa"},
			ip:       "10.1.2.3",
			wantZone: "1.10.in-addr.arpa",
			wantPTR:  "3.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disc := NewReverseZoneDiscoverer(tt.zones, nil)
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "failed to parse IP %q", tt.ip)
			zone, ptr, err := disc.reverseIPv4(ip)
			require.NoError(t, err)
			if zone != tt.wantZone {
				t.Errorf("zone = %q, want %q", zone, tt.wantZone)
			}
			if ptr != tt.wantPTR {
				t.Errorf("ptr = %q, want %q", ptr, tt.wantPTR)
			}
		})
	}
}

func hasIP6ArpaSuffix(zone string) bool {
	return len(zone) > 9 && zone[len(zone)-9:] == ".ip6.arpa"
}

// CNAME records must never produce PTRs — PTRs target canonical names only.
func TestReverseDiscoverSkipsCNAMEs(t *testing.T) {
	d := NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "alias1.example.org", Type: netboxclient.RecordTypeCNAME, CNAMETarget: "host1.example.org"},
	}
	zones, err := d.Discover(records)
	require.NoError(t, err)
	require.Len(t, zones["10.in-addr.arpa"], 1, "only the address record produces a PTR")
	assert.Equal(t, "host1.example.org", zones["10.in-addr.arpa"][0].DNSName)
}
