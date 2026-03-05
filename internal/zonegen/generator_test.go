package zonegen

import (
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

	// Serial should have incremented from the loaded value
	assert.Equal(t, uint32(2026030543), gen.serial, "serial should increment from loaded value")
	assert.Contains(t, content, "2026030543   ; serial", "zone file should contain incremented serial")
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
