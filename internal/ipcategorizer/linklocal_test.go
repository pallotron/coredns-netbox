package ipcategorizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// Dell's OS-to-iDRAC passthrough gives every server an "idrac" USB NIC at
// 169.254.0.2. It matches the BMC pattern, but the heuristic must never
// publish it as the -bmc record: it is the same unreachable address on
// every server.
func TestSelectDeviceIPsSkipsLinkLocalBMC(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-r101-prod-hv-01"

	onlyPassthrough := []netboxclient.IPRecord{
		{ID: 1, Address: "10.0.0.5", Family: 4, DeviceName: dev, InterfaceName: "eno1"},
		{ID: 2, Address: "169.254.0.2", Family: 4, DeviceName: dev, InterfaceName: "idrac"},
	}
	got := c.SelectDeviceIPs(onlyPassthrough)
	require.Contains(t, got, dev)
	assert.Nil(t, got[dev].BMCIP, "no -bmc record rather than a link-local one")

	withRealBMC := append(onlyPassthrough,
		netboxclient.IPRecord{ID: 3, Address: "10.9.0.5", Family: 4, DeviceName: dev, InterfaceName: "idrac-dedicated"})
	got = c.SelectDeviceIPs(withRealBMC)
	require.NotNil(t, got[dev].BMCIP)
	assert.Equal(t, "10.9.0.5", got[dev].BMCIP.Address, "the real BMC address is picked instead")
}

// The heuristic never picks a link-local address as a device's primary
// either (fe80:: on a management port).
func TestSelectDeviceIPsSkipsLinkLocalPrimary(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-r101-leaf-01"
	got := c.SelectDeviceIPs([]netboxclient.IPRecord{
		{ID: 1, Address: "fe80::1", Family: 6, DeviceName: dev, InterfaceName: "Management1"},
		{ID: 2, Address: "10.0.0.9", Family: 4, DeviceName: dev, InterfaceName: "Management1"},
	})
	require.Contains(t, got, dev)
	require.NotNil(t, got[dev].PrimaryIP)
	assert.Equal(t, "10.0.0.9", got[dev].PrimaryIP.Address)
}

// Interface records skip link-local addresses: IPv6 fe80:: on every port,
// and Kubernetes NodeLocal DNSCache's 169.254.20.10 on every node.
func TestInterfaceRecordsSkipLinkLocal(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 1, Address: "fe80::1", Family: 6, DeviceName: "dc1-r101-leaf-01", InterfaceName: "Vlan100"},
		{ID: 2, Address: "fe80::3eec:efff:fe12:3456", Family: 6, DeviceName: "dc1-r101-prod-hv-01", InterfaceName: "eno1"},
		{ID: 3, Address: "169.254.20.10", Family: 4, DeviceName: "k8s-node-001", InterfaceName: "nodelocaldns"},
		{ID: 4, Address: "10.0.0.5", Family: 4, DeviceName: "dc1-r101-prod-hv-01", InterfaceName: "eno1"},
		{ID: 5, Address: "2001:db8::5", Family: 6, DeviceName: "dc1-r101-prod-hv-01", InterfaceName: "eno1"},
	}
	got := c.InterfaceRecords(records, nil)
	addrs := make([]string, 0, len(got))
	for _, r := range got {
		addrs = append(addrs, r.Address)
	}
	assert.ElementsMatch(t, []string{"10.0.0.5", "2001:db8::5"}, addrs,
		"only routable addresses get interface records")
}

// An operator's explicit choice is respected: a primary_ip4 pointing at a
// link-local address is still published.
func TestHybridExplicitLinkLocalPrimaryKept(t *testing.T) {
	c := newTestCategorizer(t)
	const dev = "dc1-r101-prod-hv-01"
	records := []netboxclient.IPRecord{
		{ID: 7, Address: "169.254.10.5", Family: 4, DeviceName: dev, InterfaceName: "eno1"},
	}
	devices := []netboxclient.DeviceRecord{{Name: dev, Status: "active", PrimaryIP4ID: 7}}
	got, _ := c.SelectDeviceIPsHybrid(records, devices)
	require.Contains(t, got, dev)
	require.NotNil(t, got[dev].PrimaryIP)
	assert.Equal(t, "169.254.10.5", got[dev].PrimaryIP.Address)
}
