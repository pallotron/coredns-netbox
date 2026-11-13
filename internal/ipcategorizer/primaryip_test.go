package ipcategorizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

func newTestCategorizer(t *testing.T) *Categorizer {
	t.Helper()
	// Tightened defaults (NET removed, eth/mgt anchored, Juniper me0 added).
	c, err := NewCategorizer(
		"(?i)bmc|ipmi|ilo|idrac",
		"^lo$|^lo0|^Loopback",
		"(?i)storage|vtep|vsan",
		"(?i)mgmt|oob",
		"(?i)mgmt|Management|fxp0|^eth[01]|^mgt|^me[0-9]|^ens[0-9]|^eno[0-9]|^nic[0-9]",
		"example.com",
	)
	require.NoError(t, err, "categorizer construction should succeed")
	return c
}

func TestHybridPrimaryIPWins(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		// Alphabetically-first dataplane port — the old heuristic's wrong pick.
		{ID: 1, Address: "192.168.26.139", Family: 4, DeviceName: "dc1-site13a-r101-prod-eth-tor-01", InterfaceName: "Ethernet1/1"},
		{ID: 2, Address: "172.26.41.73", Family: 4, DeviceName: "dc1-site13a-r101-prod-eth-tor-01", InterfaceName: "Management1"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r101-prod-eth-tor-01", Status: "active", PrimaryIP4ID: 2},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, "dc1-site13a-r101-prod-eth-tor-01", "device should be selected")
	dns := result["dc1-site13a-r101-prod-eth-tor-01"]
	require.NotNil(t, dns.PrimaryIP, "primary IP should be selected")
	assert.Equal(t, "172.26.41.73", dns.PrimaryIP.Address, "primary_ip4 join must beat the heuristic")
	assert.Equal(t, 1, stats.PrimaryIP, "one device via primary_ip4")
	assert.Equal(t, 0, stats.Fallback, "no fallback used")
}

func TestHybridFallbackWhenNoPrimary(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 3, Address: "10.0.0.5", Family: 4, DeviceName: "dc1-site13a-r101-prod-hv-01", InterfaceName: "eno1"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r101-prod-hv-01", Status: "active", PrimaryIP4ID: 0},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, "dc1-site13a-r101-prod-hv-01", "fallback should select the device")
	assert.Equal(t, "10.0.0.5", result["dc1-site13a-r101-prod-hv-01"].PrimaryIP.Address, "heuristic fallback picks the mgmt-pattern interface")
	assert.Equal(t, 1, stats.Fallback, "one device via fallback")
}

func TestHybridPrimaryNotInPublishedSetFallsBack(t *testing.T) {
	c := newTestCategorizer(t)
	// primary_ip4 id 99 is NOT in the fetched set (e.g. reserved status).
	records := []netboxclient.IPRecord{
		{ID: 4, Address: "10.0.0.6", Family: 4, DeviceName: "dc1-site13a-r102-prod-hv-02", InterfaceName: "eno1"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r102-prod-hv-02", Status: "active", PrimaryIP4ID: 99},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, "dc1-site13a-r102-prod-hv-02")
	assert.Equal(t, "10.0.0.6", result["dc1-site13a-r102-prod-hv-02"].PrimaryIP.Address, "unpublished primary falls back to heuristic")
	assert.Equal(t, 1, stats.Fallback)
	assert.Equal(t, 0, stats.PrimaryIP)
}

func TestHybridOOBIPBecomesBMC(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 5, Address: "172.16.5.1", Family: 4, DeviceName: "dc1-site13a-r103-prod-hv-03", InterfaceName: "eno1"},
		{ID: 6, Address: "10.9.0.5", Family: 4, DeviceName: "dc1-site13a-r103-prod-hv-03", InterfaceName: "idrac-dedicated"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r103-prod-hv-03", Status: "active", PrimaryIP4ID: 5, OOBIPID: 6},
	}

	result, _ := c.SelectDeviceIPsHybrid(records, devices)
	dns := result["dc1-site13a-r103-prod-hv-03"]
	require.NotNil(t, dns.BMCIP, "oob_ip should produce a BMC record")
	assert.Equal(t, "10.9.0.5", dns.BMCIP.Address)
}

func TestHybridOOBIPOverridesBMCHeuristic(t *testing.T) {
	// Spec §2: a published oob_ip is the BMC record's source of truth and
	// always wins, even when the bmc-pattern heuristic also found a (different)
	// candidate interface.
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 20, Address: "172.16.9.1", Family: 4, DeviceName: "dc1-site13a-r110-prod-hv-08", InterfaceName: "eno1"},
		{ID: 21, Address: "10.9.0.21", Family: 4, DeviceName: "dc1-site13a-r110-prod-hv-08", InterfaceName: "idrac-dedicated"}, // bmc-pattern match
		{ID: 22, Address: "10.9.0.22", Family: 4, DeviceName: "dc1-site13a-r110-prod-hv-08", InterfaceName: "oob-eth0"},       // published oob_ip target; not bmc-pattern-named
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r110-prod-hv-08", Status: "active", PrimaryIP4ID: 20, OOBIPID: 22},
	}

	result, _ := c.SelectDeviceIPsHybrid(records, devices)
	dns := result["dc1-site13a-r110-prod-hv-08"]
	require.NotNil(t, dns.BMCIP, "BMC record should exist")
	assert.Equal(t, "10.9.0.22", dns.BMCIP.Address, "published oob_ip must win over the bmc-pattern heuristic")
}

func TestHybridFallbackPathOOBIPStillProducesBMC(t *testing.T) {
	// Spec §2: a fallback-path device (no resolvable primary_ip4) with a
	// published oob_ip and no bmc-pattern interface at all must still get a
	// BMC record from oob_ip — the unresolved winner{} placeholder must not
	// silently drop dev.OOBIPID.
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		// Neither bmc- nor mgmt-pattern named, so the heuristic finds nothing.
		{ID: 30, Address: "10.9.0.30", Family: 4, DeviceName: "dc1-site13a-r111-prod-hv-09", InterfaceName: "oob-eth0"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r111-prod-hv-09", Status: "active", PrimaryIP4ID: 0, OOBIPID: 30},
	}

	result, _ := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, "dc1-site13a-r111-prod-hv-09", "a published oob_ip must produce a BMC record even with no resolvable primary and no bmc-pattern interface")
	dns := result["dc1-site13a-r111-prod-hv-09"]
	require.NotNil(t, dns.BMCIP)
	assert.Equal(t, "10.9.0.30", dns.BMCIP.Address)
	assert.Nil(t, dns.PrimaryIP, "no primary IP should have been found")
}

func TestHybridDeviceMissingFromDeviceListKeepsFallback(t *testing.T) {
	// Safety net: a device that appears in IP assignments but not in the
	// devices fetch (name drift, partial API data) keeps its heuristic record.
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 7, Address: "10.0.0.7", Family: 4, DeviceName: "dc1-site13a-r104-prod-hv-04", InterfaceName: "eno1"},
	}
	result, stats := c.SelectDeviceIPsHybrid(records, nil)
	require.Contains(t, result, "dc1-site13a-r104-prod-hv-04", "fallback union must keep devices absent from the device list")
	assert.Equal(t, 1, stats.Fallback)
}

func TestHybridNoneCounted(t *testing.T) {
	c := newTestCategorizer(t)
	devices := []netboxclient.DeviceRecord{
		{Name: "dc1-site13a-r105-prod-sw-01", Status: "active"}, // no primary, no IPs at all
	}
	result, stats := c.SelectDeviceIPsHybrid(nil, devices)
	assert.NotContains(t, result, "dc1-site13a-r105-prod-sw-01")
	assert.Equal(t, 1, stats.None, "unresolvable device counted as none")
}

func TestHybridDuplicateDeviceNamesCountedOnce(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 10, Address: "172.16.7.1", Family: 4, DeviceName: "dc1-site13a-r107-prod-hv-06", InterfaceName: "eno1"},
	}
	devices := []netboxclient.DeviceRecord{
		// duplicate name: first entry has no primary, second (e.g. a VM) has a published one
		{Name: "dc1-site13a-r107-prod-hv-06", Status: "active", PrimaryIP4ID: 0},
		{Name: "dc1-site13a-r107-prod-hv-06", Status: "active", PrimaryIP4ID: 10},
	}
	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, "dc1-site13a-r107-prod-hv-06")
	assert.Equal(t, "172.16.7.1", result["dc1-site13a-r107-prod-hv-06"].PrimaryIP.Address)
	assert.Equal(t, 1, stats.PrimaryIP, "distinct name counted exactly once")
	assert.Equal(t, 0, stats.Fallback)
	assert.Equal(t, 0, stats.None, "name must not also be counted as none")
}

func TestHybridDuplicateDeviceNamesFirstResolvablePrimaryWins(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 11, Address: "172.16.8.1", Family: 4, DeviceName: "dc1-site13a-r108-prod-hv-07", InterfaceName: "eno1"},
		{ID: 12, Address: "172.16.8.2", Family: 4, DeviceName: "dc1-site13a-r108-prod-hv-07", InterfaceName: "eno2"},
	}
	devices := []netboxclient.DeviceRecord{
		// both entries have a resolvable primary; the FIRST entry's wins (devices before VMs).
		{Name: "dc1-site13a-r108-prod-hv-07", Status: "active", PrimaryIP4ID: 11},
		{Name: "dc1-site13a-r108-prod-hv-07", Status: "active", PrimaryIP4ID: 12},
	}
	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, "dc1-site13a-r108-prod-hv-07")
	assert.Equal(t, "172.16.8.1", result["dc1-site13a-r108-prod-hv-07"].PrimaryIP.Address, "first entry's resolvable primary wins")
	assert.Equal(t, 1, stats.PrimaryIP, "distinct name counted exactly once")
	assert.Equal(t, 0, stats.Fallback)
	assert.Equal(t, 0, stats.None)
}

func TestDeviceDNSToRecordsStampsKinds(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 8, Address: "172.16.5.9", Family: 4, DeviceName: "dc1-site13a-r106-prod-hv-05", InterfaceName: "eno1"},
		{ID: 9, Address: "10.9.0.9", Family: 4, DeviceName: "dc1-site13a-r106-prod-hv-05", InterfaceName: "bmc0"},
	}
	deviceDNS := c.SelectDeviceIPs(records)
	out := DeviceDNSToRecords(deviceDNS, nil)
	require.Len(t, out, 2, "primary + bmc records")
	kinds := map[string]string{}
	for _, r := range out {
		kinds[r.Address] = r.Kind
	}
	assert.Equal(t, netboxclient.KindDevice, kinds["172.16.5.9"], "primary record kind")
	assert.Equal(t, netboxclient.KindBMC, kinds["10.9.0.9"], "bmc record kind")
}
