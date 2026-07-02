package zonemanager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonegen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_HasExistingZones_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})
	assert.False(t, mgr.HasExistingZones(), "expected false for empty zone dir")
}

func TestManager_HasExistingZones_WithZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.example.org"), []byte("data"), 0o644), "setup")

	assert.True(t, mgr.HasExistingZones(), "expected true when db.* files exist")
}

func TestManager_HasExistingZones_IgnoresNonZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

	require.NoError(t, os.WriteFile(filepath.Join(dir, "some-other-file"), []byte("data"), 0o644), "setup")

	assert.False(t, mgr.HasExistingZones(), "expected false when only non-zone files exist")
}

func TestManager_CreateZoneFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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

	assert.Contains(t, string(content), "host1", "zone file should contain host1")
	assert.Contains(t, string(content), "host2", "zone file should contain host2")
	assert.Contains(t, string(content), "$ORIGIN example.org.", "zone file should contain $ORIGIN")
}

func TestManager_MultipleZones(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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
		assert.FileExists(t, filepath.Join(dir, "db."+zone), "zone file for %s not created", zone)
	}

	zones := mgr.Zones()
	require.Len(t, zones, 2, "expected 2 zones")
}

func TestManager_RemoveOrphanedZones(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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

	assert.NoFileExists(t, filepath.Join(dir, "db.old.example.org"), "orphaned zone file should have been removed")
	assert.FileExists(t, filepath.Join(dir, "db.example.org"), "db.example.org should still exist")
}

func TestManager_NoChangeSkipsWrite(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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

	assert.Equal(t, info1.ModTime(), info2.ModTime(), "file should not have been rewritten when data is unchanged")
}

func TestManager_RemoveStaleFiles(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

	// Manually create a stale zone file
	_ = os.WriteFile(filepath.Join(dir, "db.stale.example.org"), []byte("stale"), 0o644)

	zm := zonediscovery.ZoneMap{
		"example.org": []netboxclient.IPRecord{
			{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}

	_, err := mgr.Update(zm)
	require.NoError(t, err, "unexpected error")

	assert.NoFileExists(t, filepath.Join(dir, "db.stale.example.org"), "stale zone file should have been removed")
}

func TestManager_UpdateStats_Created(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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
	assert.Equal(t, 2, stats.Created, "expected Created=2")
	assert.Equal(t, 0, stats.Updated, "expected Updated=0")
	assert.Equal(t, 0, stats.Deleted, "expected Deleted=0")
}

func TestManager_UpdateStats_Updated(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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
	assert.Equal(t, 0, stats.Created, "expected Created=0")
	assert.Equal(t, 1, stats.Updated, "expected Updated=1")
}

func TestManager_UpdateStats_NoChange(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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
	assert.Equal(t, 0, stats.Created, "expected Created=0")
	assert.Equal(t, 0, stats.Updated, "expected Updated=0")
}

func TestManager_UpdateStats_Deleted(t *testing.T) {
	dir := t.TempDir()
	mgr := New(dir, "ns1.example.org.", "admin.example.org.", 30, zonegen.SOATimers{})

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
	assert.Equal(t, 1, stats.Deleted, "expected Deleted=1")
}
