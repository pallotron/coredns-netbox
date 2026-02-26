package zonemanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/stretchr/testify/require"
)

func TestManager_HasExistingZones_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)
	if mgr.HasExistingZones() {
		t.Error("expected false for empty zone dir")
	}
}

func TestManager_HasExistingZones_WithZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.example.org"), []byte("data"), 0o644), "setup")

	if !mgr.HasExistingZones() {
		t.Error("expected true when db.* files exist")
	}
}

func TestManager_HasExistingZones_IgnoresNonZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "some-other-file"), []byte("data"), 0o644), "setup")

	if mgr.HasExistingZones() {
		t.Error("expected false when only non-zone files exist")
	}
}

func TestManager_CreateZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
			{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		},
	}

	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")

	// Check that zone file was created
	content, err := os.ReadFile(filepath.Join(dir, "db.example.org"))
	require.NoError(t, err, "zone file not created")

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

	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")

	// Check both files exist
	for _, zone := range []string{"example.org", "prod.example.org"} {
		if _, err := os.Stat(filepath.Join(dir, "db."+zone)); err != nil {
			t.Errorf("zone file for %s not created: %v", zone, err)
		}
	}

	zones := mgr.Zones()
	require.Len(t, zones, 2, "expected 2 zones")
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
	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")

	// Now update with only one zone — old.example.org should be removed
	zm2 := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	_, err = mgr.Update(zm2)
	require.NoError(t, err, "unexpected error")

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
	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")
	info1, _ := os.Stat(filepath.Join(dir, "db.example.org"))

	// Second update with same data — should not rewrite
	_, err = mgr.Update(zm)
	require.NoError(t, err, "unexpected error")
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

	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")

	if _, err := os.Stat(filepath.Join(dir, "db.stale.example.org")); !os.IsNotExist(err) {
		t.Error("stale zone file should have been removed")
	}
}

func TestManager_UpdateStats_Created(t *testing.T) {
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

	stats, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")
	if stats.Created != 2 {
		t.Errorf("expected Created=2, got %d", stats.Created)
	}
	if stats.Updated != 0 {
		t.Errorf("expected Updated=0, got %d", stats.Updated)
	}
	if stats.Deleted != 0 {
		t.Errorf("expected Deleted=0, got %d", stats.Deleted)
	}
}

func TestManager_UpdateStats_Updated(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}

	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error on first update")

	// Change the records — this must produce a file rewrite (Updated=1)
	zm2 := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
			{DNSName: "host2.example.org", Address: "10.0.0.2", Family: 4},
		},
	}
	stats, err := mgr.Update(zm2)
	require.NoError(t, err, "unexpected error on second update")
	if stats.Created != 0 {
		t.Errorf("expected Created=0, got %d", stats.Created)
	}
	if stats.Updated != 1 {
		t.Errorf("expected Updated=1, got %d", stats.Updated)
	}
}

func TestManager_UpdateStats_NoChange(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}

	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error on first update")

	// Identical records — no write should happen
	stats, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error on second update")
	if stats.Created != 0 {
		t.Errorf("expected Created=0, got %d", stats.Created)
	}
	if stats.Updated != 0 {
		t.Errorf("expected Updated=0, got %d", stats.Updated)
	}
}

func TestManager_UpdateStats_Deleted(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"old.example.org": []netboxclient.IPRecord{
			{DNSName: "host2.old.example.org", Address: "10.0.0.2", Family: 4},
		},
	}
	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")

	// Drop old.example.org — Deleted should be 1
	zm2 := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	stats, err := mgr.Update(zm2)
	require.NoError(t, err, "unexpected error")
	if stats.Deleted != 1 {
		t.Errorf("expected Deleted=1, got %d", stats.Deleted)
	}
}
