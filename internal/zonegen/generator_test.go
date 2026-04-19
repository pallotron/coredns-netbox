package zonegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateZone(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
	}, "")

	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "host3.example.org", Address: "2001:db8::1", Family: 6},
	}

	content, changed, err := gen.Generate(records)
	require.NoError(t, err, "Generate() error")
	if !changed {
		t.Error("expected changed=true on first generation")
	}

	// Check SOA
	if !strings.Contains(content, "$ORIGIN example.org.") {
		t.Error("missing $ORIGIN")
	}
	if !strings.Contains(content, "IN SOA ns1.example.org. admin.example.org.") {
		t.Error("missing SOA record")
	}

	// Check records
	if !strings.Contains(content, "host1 IN A 10.0.0.1") {
		t.Error("missing host1 A record")
	}
	if !strings.Contains(content, "host2 IN A 10.0.0.2") {
		t.Error("missing host2 A record")
	}
	if !strings.Contains(content, "host3 IN AAAA 2001:db8::1") {
		t.Error("missing host3 AAAA record")
	}

	// Same records should not trigger a change
	_, changed, err = gen.Generate(records)
	require.NoError(t, err, "Generate() error")
	if changed {
		t.Error("expected changed=false for identical records")
	}

	// Different records should trigger a change
	records = append(records, netboxclient.IPRecord{
		DNSName: "host4.example.org", Address: "10.0.0.4", Family: 4,
	})
	content, changed, err = gen.Generate(records)
	require.NoError(t, err, "Generate() error")
	if !changed {
		t.Error("expected changed=true for new records")
	}
	if !strings.Contains(content, "host4 IN A 10.0.0.4") {
		t.Error("missing host4 A record")
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zones", "db.test")

	content := "test zone content\n"
	require.NoError(t, WriteFile(path, content), "WriteFile() error")

	got, err := os.ReadFile(path)
	require.NoError(t, err, "ReadFile() error")
	if string(got) != content {
		t.Errorf("content mismatch: got %q, want %q", string(got), content)
	}

	// Verify temp file was cleaned up
	tmp := path + ".tmp"
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("temp file was not cleaned up")
	}
}

func TestShortName(t *testing.T) {
	tests := []struct {
		fqdn, origin, want string
	}{
		{"host1.example.org", "example.org.", "host1"},
		{"sub.host1.example.org", "example.org.", "sub.host1"},
		{"example.org", "example.org.", "@"},
		{"other.com", "example.org.", "other.com."},
	}

	for _, tt := range tests {
		got := shortName(tt.fqdn, tt.origin)
		if got != tt.want {
			t.Errorf("shortName(%q, %q) = %q, want %q", tt.fqdn, tt.origin, got, tt.want)
		}
	}
}

func TestNextSerial(t *testing.T) {
	// From zero, should get today-based serial
	s := NextSerial(0)
	if s == 0 {
		t.Error("serial should not be 0")
	}
	// String length should be 10 (YYYYMMDDNN)
	// Just verify it increments
	s2 := NextSerial(s)
	if s2 != s+1 {
		t.Errorf("expected %d, got %d", s+1, s2)
	}
}

func TestGenerateReverseZone(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "1.10.in-addr.arpa",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
		Type:       ZoneTypeReverse,
	}, "")

	// For reverse zones, Address contains the PTR name and DNSName contains the target
	records := []netboxclient.IPRecord{
		{Address: "3.2", DNSName: "server1.example.com", Family: 4},
		{Address: "4.2", DNSName: "server2.example.com", Family: 4},
	}

	content, changed, err := gen.Generate(records)
	require.NoError(t, err, "Generate() error")
	if !changed {
		t.Error("expected changed=true on first generation")
	}

	// Check SOA
	if !strings.Contains(content, "$ORIGIN 1.10.in-addr.arpa.") {
		t.Error("missing $ORIGIN")
	}
	if !strings.Contains(content, "IN SOA ns1.example.org. admin.example.org.") {
		t.Error("missing SOA record")
	}

	// Check PTR records
	if !strings.Contains(content, "3.2 IN PTR server1.example.com.") {
		t.Error("missing 3.2 PTR record")
	}
	if !strings.Contains(content, "4.2 IN PTR server2.example.com.") {
		t.Error("missing 4.2 PTR record")
	}

	// Verify no A records are present
	if strings.Contains(content, " IN A ") {
		t.Error("should not contain A records in reverse zone")
	}
}

func TestGenerateForwardZone(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
		Type:       ZoneTypeForward,
	}, "")

	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
	}

	content, _, err := gen.Generate(records)
	require.NoError(t, err, "Generate() error")

	// Verify A record is present
	if !strings.Contains(content, "host1 IN A 10.0.0.1") {
		t.Error("missing A record in forward zone")
	}

	// Verify no PTR records are present
	if strings.Contains(content, " IN PTR ") {
		t.Error("should not contain PTR records in forward zone")
	}
}

func TestSerialPersistence(t *testing.T) {
	// Create a temporary zone file with a known serial
	tmpDir := t.TempDir()
	zonePath := filepath.Join(tmpDir, "db.example.org")

	existingZone := `$ORIGIN example.org.
$TTL 300
@ IN SOA ns1.example.org. admin.example.org. (
    2026030542   ; serial
    3600      ; refresh
    900       ; retry
    604800    ; expire
    86400     ; minimum
)

@ IN NS ns1.example.org.

host1 IN A 10.0.0.1
`
	err := os.WriteFile(zonePath, []byte(existingZone), 0644)
	require.NoError(t, err)

	// Create generator with existing zone file
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
	}, zonePath)

	// Generator should have read the serial from the file
	assert.Equal(t, uint32(2026030542), gen.serial, "should have loaded serial from existing zone file")

	// Generate with same records - should increment serial
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.2", Family: 4}, // Different IP
	}

	content, changed, err := gen.Generate(records)
	require.NoError(t, err)
	assert.True(t, changed, "should detect change when IP differs")

	// Serial should have advanced past the loaded value (jumps to today's date when loaded serial is stale)
	assert.Greater(t, gen.serial, uint32(2026030542), "serial should advance past loaded value")
	now := time.Now().UTC()
	expectedBase := uint32(now.Year()*1000000 + int(now.Month())*10000 + now.Day()*100)
	assert.GreaterOrEqual(t, gen.serial, expectedBase+1, "serial should be at least today's base+01")
	assert.Contains(t, content, fmt.Sprintf("%d   ; serial", gen.serial), "zone file should contain the new serial")
}

func TestSerialPersistenceNoFile(t *testing.T) {
	// Create generator with non-existent zone file path
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
	}, "/nonexistent/path/db.example.org")

	// Generator should start with serial 0 (file doesn't exist)
	assert.Equal(t, uint32(0), gen.serial, "should start with serial 0 when file doesn't exist")

	// Generate records - should create new serial based on today's date
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
	}

	content, changed, err := gen.Generate(records)
	require.NoError(t, err)
	assert.True(t, changed, "should detect change on first generation")

	// Serial should be in YYYYMMDD01 format
	now := time.Now().UTC()
	expectedBase := uint32(now.Year()*1000000 + int(now.Month())*10000 + now.Day()*100)
	assert.Equal(t, expectedBase+1, gen.serial, "serial should be today's date + 01")
	assert.Contains(t, content, "host1 IN A 10.0.0.1", "zone file should contain the record")
}

func TestGeneratePerRecordTTL(t *testing.T) {
	cfg := ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
		Type:       ZoneTypeForward,
	}
	g := NewGenerator(cfg, "")

	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4, TTL: 0},  // use zone TTL
		{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4, TTL: 60}, // per-record TTL
	}

	content, changed, err := g.Generate(records)
	require.NoError(t, err)
	require.True(t, changed)

	// host1: no inline TTL (uses $TTL 300)
	assert.Contains(t, content, "host1 IN A 10.0.0.1")
	// host2: inline TTL 60
	assert.Contains(t, content, "host2 60 IN A 10.0.0.2")
}

func TestGenerateIdempotency(t *testing.T) {
	// Test that generating the same records multiple times produces stable hashes
	// This catches hash oscillation bugs caused by non-deterministic record ordering
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
	}, "")

	// Create records with fields that could cause sort instability
	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4, DeviceName: "device1", InterfaceName: "eth0", VRF: "mgmt"},
		{DNSName: "host1.example.org", Address: "2001:db8::1", Family: 6, DeviceName: "device1", InterfaceName: "eth0", VRF: "mgmt"}, // Same DNS, different address/family
		{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4, DeviceName: "device2", InterfaceName: "eth1", VRF: "prod"},
		{DNSName: "host3.example.org", Address: "10.0.0.3", Family: 4, DeviceName: "device3", InterfaceName: "eth0", VRF: "mgmt"},
		{DNSName: "host4.example.org", Address: "10.0.0.4", Family: 4, DeviceName: "device4", InterfaceName: "eth2", VRF: "dev"},
	}

	// First generation
	content1, changed, err := gen.Generate(records)
	require.NoError(t, err)
	assert.True(t, changed, "should detect change on first generation")
	assert.NotEmpty(t, content1, "should generate content")

	// Second generation with same records (should be unchanged)
	content2, changed, err := gen.Generate(records)
	require.NoError(t, err)
	assert.False(t, changed, "should not detect change when records identical")
	assert.Empty(t, content2, "should return empty content when unchanged")

	// Third generation with same records (verify stability)
	content3, changed, err := gen.Generate(records)
	require.NoError(t, err)
	assert.False(t, changed, "should remain unchanged on third generation")
	assert.Empty(t, content3, "should return empty content when unchanged")

	// Test with shuffled records (same data, different input order)
	shuffledRecords := []netboxclient.IPRecord{
		records[4], // host4
		records[1], // host1 (IPv6)
		records[0], // host1 (IPv4)
		records[3], // host3
		records[2], // host2
	}

	content4, changed, err := gen.Generate(shuffledRecords)
	require.NoError(t, err)
	assert.False(t, changed, "should not detect change when records shuffled")
	assert.Empty(t, content4, "should return empty content when records just shuffled")
}
