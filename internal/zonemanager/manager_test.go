package zonemanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

func TestManager_CreateZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
			{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		},
	}

	if err := mgr.Update(zm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that zone file was created
	content, err := os.ReadFile(filepath.Join(dir, "db.example.org"))
	if err != nil {
		t.Fatalf("zone file not created: %v", err)
	}

	if !strings.Contains(string(content), "host1") {
		t.Error("zone file should contain host1")
	}
	if !strings.Contains(string(content), "host2") {
		t.Error("zone file should contain host2")
	}
	if !strings.Contains(string(content), "$ORIGIN example.org.") {
		t.Error("zone file should contain $ORIGIN")
	}
}

func TestManager_MultipleZones(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"prod.example.org": []netboxclient.IPRecord{
			{DNSName: "db.prod.example.org", Address: "10.0.0.2", Family: 4},
		},
	}

	if err := mgr.Update(zm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check both files exist
	for _, zone := range []string{"example.org", "prod.example.org"} {
		if _, err := os.Stat(filepath.Join(dir, "db."+zone)); err != nil {
			t.Errorf("zone file for %s not created: %v", zone, err)
		}
	}

	zones := mgr.Zones()
	if len(zones) != 2 {
		t.Fatalf("expected 2 zones, got %d", len(zones))
	}
}

func TestManager_RemoveOrphanedZones(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	// Create initial zones
	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"old.example.org": []netboxclient.IPRecord{
			{DNSName: "host2.old.example.org", Address: "10.0.0.2", Family: 4},
		},
	}
	if err := mgr.Update(zm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Now update with only one zone — old.example.org should be removed
	zm2 := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	if err := mgr.Update(zm2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "db.old.example.org")); !os.IsNotExist(err) {
		t.Error("orphaned zone file db.old.example.org should have been removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "db.example.org")); err != nil {
		t.Error("db.example.org should still exist")
	}
}

func TestManager_NoChangeSkipsWrite(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}

	// First update
	if err := mgr.Update(zm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info1, _ := os.Stat(filepath.Join(dir, "db.example.org"))

	// Second update with same data — should not rewrite
	if err := mgr.Update(zm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info2, _ := os.Stat(filepath.Join(dir, "db.example.org"))

	if info1.ModTime() != info2.ModTime() {
		t.Error("file should not have been rewritten when data is unchanged")
	}
}

func TestManager_RemoveStaleFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	// Manually create a stale zone file
	_ = os.WriteFile(filepath.Join(dir, "db.stale.example.org"), []byte("stale"), 0o644)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}

	if err := mgr.Update(zm); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "db.stale.example.org")); !os.IsNotExist(err) {
		t.Error("stale zone file should have been removed")
	}
}
