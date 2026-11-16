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
		"(?i)mgmt|Management|fxp0|^eth[01]|mgt|^net[0-9]|^me[0-9]|^ens[0-9]|^eno[0-9]|^nic[0-9]",
		"example.com",
	)
	require.NoError(t, err, "categorizer construction should succeed")
	return c
}

func TestHybridPrimaryIPWins(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-site13a-r101-prod-eth-tor-01"
	records := []netboxclient.IPRecord{
		// The heuristic's pick: a management-pattern interface.
		{ID: 1, Address: "172.26.41.73", Family: 4, DeviceName: dev, InterfaceName: "Management1"},
		// NetBox's primary_ip4: an interface the heuristic would never pick.
		{ID: 2, Address: "192.168.26.139", Family: 4, DeviceName: dev, InterfaceName: "Vlan100"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: dev, Status: "active", PrimaryIP4ID: 2},
	}

	// Guard: without the device list the heuristic picks Management1, so the
	// assertion below can only pass if primary_ip4 really overrides it.
	heuristic := c.SelectDeviceIPs(records)
	require.Contains(t, heuristic, dev)
	require.Equal(t, "172.26.41.73", heuristic[dev].PrimaryIP.Address, "heuristic baseline must pick Management1")

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, dev, "device should be selected")
	dns := result[dev]
	require.NotNil(t, dns.PrimaryIP, "primary IP should be selected")
	assert.Equal(t, "192.168.26.139", dns.PrimaryIP.Address, "primary_ip4 must override the heuristic's pick")
	assert.Equal(t, 1, stats.PrimaryIP, "one device via primary_ip4")
	assert.Equal(t, 0, stats.Fallback, "no fallback used")
}

// primary_ip4 and oob_ip are the source of truth even when the IP object
// carries an explicit dns_name: the id join must see every published IP,
// not just the ones the heuristic generates names for.
func TestHybridPrimaryAndOOBWithDNSNameStillResolve(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-site13a-r101-prod-hv-01"
	records := []netboxclient.IPRecord{
		{ID: 50, Address: "10.0.0.50", Family: 4, DeviceName: dev, InterfaceName: "eno1", DNSName: "hv01.example.com"},
		{ID: 51, Address: "10.0.0.51", Family: 4, DeviceName: dev, InterfaceName: "eno2"},
		{ID: 60, Address: "10.9.0.60", Family: 4, DeviceName: dev, InterfaceName: "idrac", DNSName: "hv01-oob.example.com"},
		{ID: 61, Address: "10.9.0.61", Family: 4, DeviceName: dev, InterfaceName: "idrac-shared"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: dev, Status: "active", PrimaryIP4ID: 50, OOBIPID: 60},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, dev)
	require.NotNil(t, result[dev].PrimaryIP, "primary IP should be selected")
	require.NotNil(t, result[dev].BMCIP, "BMC IP should be selected")
	assert.Equal(t, "10.0.0.50", result[dev].PrimaryIP.Address, "primary_ip4 with a dns_name must still win")
	assert.Equal(t, "10.9.0.60", result[dev].BMCIP.Address, "oob_ip with a dns_name must still win the BMC record")
	assert.Equal(t, 1, stats.PrimaryIP, "resolved via primary_ip4")
	assert.Equal(t, 0, stats.Fallback, "no heuristic fallback")
}

// A device whose only IP is its dns_name'd primary_ip4 still gets its
// generated device record.
func TestHybridPrimaryWithDNSNameOnlyIP(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-site13a-r101-prod-hv-02"
	records := []netboxclient.IPRecord{
		{ID: 70, Address: "10.0.0.70", Family: 4, DeviceName: dev, InterfaceName: "eno1", DNSName: "hv02.example.com"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: dev, Status: "active", PrimaryIP4ID: 70},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, dev)
	require.NotNil(t, result[dev].PrimaryIP, "primary IP should be selected")
	assert.Equal(t, "10.0.0.70", result[dev].PrimaryIP.Address)
	assert.Equal(t, 1, stats.PrimaryIP)
	assert.Equal(t, 0, stats.None)
}

// The regex heuristic still only considers IPs without a dns_name, exactly
// as before: a dns_name'd IP is never a fallback pick.
func TestHybridHeuristicIgnoresDNSNameIPs(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-site13a-r101-prod-hv-03"
	records := []netboxclient.IPRecord{
		{ID: 80, Address: "10.0.0.80", Family: 4, DeviceName: dev, InterfaceName: "eno1", DNSName: "hv03.example.com"},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, nil)
	assert.NotContains(t, result, dev, "dns_name'd IPs are not heuristic candidates")
	assert.Equal(t, 0, stats.Fallback)
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
		{ID: 22, Address: "10.9.0.22", Family: 4, DeviceName: "dc1-site13a-r110-prod-hv-08", InterfaceName: "oob-eth0"},        // published oob_ip target; not bmc-pattern-named
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

// A device and a VM sharing a name: the device (first) has only an oob_ip,
// the VM has a resolvable primary_ip4. The VM wins the primary, but the
// device's published oob_ip must still produce the BMC record.
func TestHybridDuplicateNameLaterPrimaryKeepsEarlierOOB(t *testing.T) {
	c := newTestCategorizer(t)
	const name = "dc1-site13a-r101-prod-hv-07"
	records := []netboxclient.IPRecord{
		{ID: 6, Address: "10.9.20.6", Family: 4, DeviceName: name, InterfaceName: "oob-eth0"},
		{ID: 5, Address: "172.16.20.5", Family: 4, DeviceName: name, InterfaceName: "eth0"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: name, Status: "active", OOBIPID: 6},      // dcim device: oob_ip only
		{Name: name, Status: "active", PrimaryIP4ID: 5}, // VM: primary_ip4 only
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, result, name)
	require.NotNil(t, result[name].PrimaryIP, "primary IP should be selected")
	require.NotNil(t, result[name].BMCIP, "the device's oob_ip must not be dropped")
	assert.Equal(t, "172.16.20.5", result[name].PrimaryIP.Address, "the VM's resolvable primary wins")
	assert.Equal(t, "10.9.20.6", result[name].BMCIP.Address, "the device's oob_ip still drives the BMC record")
	assert.Equal(t, 1, stats.PrimaryIP, "counted once")
}

// Stats describe the device's own record: fallback means the heuristic found
// a primary address. A device with only a BMC address (heuristic or oob_ip)
// has no device record and counts as none, the same either way.
func TestHybridStatsCountDevicesWithoutPrimaryAsNone(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		// bmcOnly: the heuristic finds only a BMC interface.
		{ID: 1, Address: "10.9.0.1", Family: 4, DeviceName: "bmc-only", InterfaceName: "idrac"},
		// oobOnly: NetBox oob_ip, no primary and no heuristic match.
		{ID: 2, Address: "10.9.0.2", Family: 4, DeviceName: "oob-only", InterfaceName: "console0"},
		// fallback: the heuristic finds a primary address.
		{ID: 3, Address: "10.0.0.3", Family: 4, DeviceName: "fallback", InterfaceName: "mgmt0"},
		// notListed: in IP data but not in the device list, BMC address only.
		{ID: 4, Address: "10.9.0.4", Family: 4, DeviceName: "not-listed", InterfaceName: "ipmi"},
	}
	devices := []netboxclient.DeviceRecord{
		{Name: "bmc-only", Status: "active"},
		{Name: "oob-only", Status: "active", OOBIPID: 2},
		{Name: "fallback", Status: "active"},
	}

	result, stats := c.SelectDeviceIPsHybrid(records, devices)
	assert.NotNil(t, result["bmc-only"].BMCIP, "bmc-only still gets its -bmc record")
	assert.NotNil(t, result["oob-only"].BMCIP, "oob-only still gets its -bmc record")
	assert.Equal(t, SelectionStats{PrimaryIP: 0, Fallback: 1, None: 3}, stats,
		"only the device whose heuristic found a primary counts as fallback")
}
