package merge_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/merge"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/pallotron/coredns-netbox/internal/zonegen"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newComponents(t *testing.T) (string, dynamicstore.DynamicStore, *zonemanager.Manager, *metrics.Sidecar) {
	t.Helper()
	dir := t.TempDir()
	store, err := dynamicstore.NewFileStore(filepath.Join(dir, "dynamic.json"))
	require.NoError(t, err)
	mgr := zonemanager.New(dir, "ns1.example.org.", "", "admin.example.org.", 300, zonegen.SOATimers{})
	m := metrics.NewSidecar(prometheus.NewRegistry())
	return dir, store, mgr, m
}

func TestWrite_DynamicRecordGetsPTR(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	require.NoError(t, store.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "pod.example.org", Address: "10.0.1.5", Family: 4},
	}))

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	netboxZones := zonediscovery.ZoneMap{"example.org": nil}

	_, err := merge.Write(netboxZones, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "db.10.in-addr.arpa"))
	require.NoError(t, err, "reverse zone file should be created for dynamic record")
	assert.Contains(t, string(content), "5.1.0")
	assert.Contains(t, string(content), "pod.example.org")
}

func TestWrite_DynamicPTRShadowsNetboxPTR(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	// Dynamic record overrides the Netbox-sourced PTR for the same IP.
	require.NoError(t, store.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "dynamic-pod.example.org", Address: "10.0.1.5", Family: 4},
	}))

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	netboxZones := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "netbox-host.example.org", Address: "10.0.1.5", Family: 4},
		},
		"10.in-addr.arpa": {
			{DNSName: "netbox-host.example.org", Address: "5.1.0", Family: 4},
		},
	}

	_, err := merge.Write(netboxZones, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "db.10.in-addr.arpa"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "dynamic-pod.example.org", "dynamic PTR should be present")
	assert.NotContains(t, string(content), "netbox-host.example.org", "Netbox PTR should be shadowed")
}

func TestWrite_NilReverseDiscProducesNoPTR(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	require.NoError(t, store.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "pod.example.org", Address: "10.0.1.5", Family: 4},
	}))

	netboxZones := zonediscovery.ZoneMap{"example.org": nil}

	_, err := merge.Write(netboxZones, store, mgr, m, nil)
	require.NoError(t, err)

	_, err = os.ReadFile(filepath.Join(dir, "db.10.in-addr.arpa"))
	assert.True(t, os.IsNotExist(err), "no reverse zone file when reverseDisc is nil")
}

func TestWrite_DynamicRecordOutsideReverseZoneIsSkipped(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	// IP is in 192.168.x.x but only 10.in-addr.arpa is configured.
	require.NoError(t, store.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "pod.example.org", Address: "192.168.1.5", Family: 4},
	}))

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	netboxZones := zonediscovery.ZoneMap{"example.org": nil}

	_, err := merge.Write(netboxZones, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	_, err = os.ReadFile(filepath.Join(dir, "db.10.in-addr.arpa"))
	assert.True(t, os.IsNotExist(err), "no reverse zone for IP outside configured zones")
}

func TestWrite_NetboxPTRsPreservedWhenNoDynamicConflict(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	// Netbox has a PTR for 10.0.1.1; dynamic record uses a different IP.
	require.NoError(t, store.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "dynamic-pod.example.org", Address: "10.0.1.5", Family: 4},
	}))

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	netboxZones := zonediscovery.ZoneMap{
		"example.org": nil,
		"10.in-addr.arpa": {
			{DNSName: "netbox-host.example.org", Address: "1.1.0", Family: 4},
		},
	}

	_, err := merge.Write(netboxZones, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "db.10.in-addr.arpa"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "netbox-host.example.org", "non-conflicting Netbox PTR should be preserved")
	assert.Contains(t, string(content), "dynamic-pod.example.org", "dynamic PTR should also be present")
}

func TestWrite_ZoneRecordsGauge(t *testing.T) {
	_, store, mgr, m := newComponents(t)

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	netboxZones := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
			{DNSName: "a2.example.org", Address: "10.0.0.2", Family: 4},
			{DNSName: "v6.example.org", Address: "fd00::1", Family: 6},
			{DNSName: "alias.example.org", Type: netboxclient.RecordTypeCNAME, CNAMETarget: "a1.example.org"},
		},
		"10.in-addr.arpa": {
			{DNSName: "a1.example.org", Address: "1.0.0", Family: 4},
			{DNSName: "a2.example.org", Address: "2.0.0", Family: 4},
		},
	}

	_, err := merge.Write(netboxZones, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	assert.Equal(t, 2.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "AAAA")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "CNAME")))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("10.in-addr.arpa", "PTR")))
	// Only observed (zone, type) pairs are emitted: 4 series total.
	// (The WithLabelValues reads above create the series they touch, so
	// count BEFORE asserting absence in any new assertions you add.)
}

func TestWrite_ZoneRecordsGaugeResetsRemovedZones(t *testing.T) {
	_, store, mgr, m := newComponents(t)

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	withReverse := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"10.in-addr.arpa": {
			{DNSName: "a1.example.org", Address: "1.0.0", Family: 4},
		},
	}
	_, err := merge.Write(withReverse, store, mgr, m, reverseDisc)
	require.NoError(t, err)
	require.Equal(t, 2, testutil.CollectAndCount(m.ZoneRecords), "expected A + PTR series after first write")

	// Second write without the reverse zone: its PTR series must disappear,
	// not flatline at the old value.
	forwardOnly := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	_, err = merge.Write(forwardOnly, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	assert.Equal(t, 1, testutil.CollectAndCount(m.ZoneRecords), "stale zone series must be reset")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))
}

func TestWrite_ZoneRecordsGaugeKeptOnFailedUpdate(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	netboxZones := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	_, err := merge.Write(netboxZones, store, mgr, m, nil)
	require.NoError(t, err)
	require.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))

	// Make the zone dir unwritable so the next write (a new zone file) fails.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	grown := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"other.example.net": {
			{DNSName: "b1.other.example.net", Address: "10.0.0.9", Family: 4},
		},
	}
	_, err = merge.Write(grown, store, mgr, m, nil)
	require.Error(t, err, "write into a read-only zone dir must fail")

	// The gauge still describes the zones actually on disk.
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("other.example.net", "A")))
}
