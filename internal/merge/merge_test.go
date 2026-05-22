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
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newComponents(t *testing.T) (string, dynamicstore.DynamicStore, *zonemanager.Manager, *metrics.Sidecar) {
	t.Helper()
	dir := t.TempDir()
	store, err := dynamicstore.NewFileStore(filepath.Join(dir, "dynamic.json"))
	require.NoError(t, err)
	mgr := zonemanager.New(dir, "ns1.example.org.", "admin.example.org.", 300)
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
