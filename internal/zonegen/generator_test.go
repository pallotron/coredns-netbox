package zonegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

func TestGenerateZone(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
	})

	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "host3.example.org", Address: "2001:db8::1", Family: 6},
	}

	content, changed, err := gen.Generate(records)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if changed {
		t.Error("expected changed=false for identical records")
	}

	// Different records should trigger a change
	records = append(records, netboxclient.IPRecord{
		DNSName: "host4.example.org", Address: "10.0.0.4", Family: 4,
	})
	content, changed, err = gen.Generate(records)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
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
	if err := WriteFile(path, content); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
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
	})

	// For reverse zones, Address contains the PTR name and DNSName contains the target
	records := []netboxclient.IPRecord{
		{Address: "3.2", DNSName: "server1.example.com", Family: 4},
		{Address: "4.2", DNSName: "server2.example.com", Family: 4},
	}

	content, changed, err := gen.Generate(records)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
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
	})

	records := []netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
	}

	content, _, err := gen.Generate(records)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// Verify A record is present
	if !strings.Contains(content, "host1 IN A 10.0.0.1") {
		t.Error("missing A record in forward zone")
	}

	// Verify no PTR records are present
	if strings.Contains(content, " IN PTR ") {
		t.Error("should not contain PTR records in forward zone")
	}
}
