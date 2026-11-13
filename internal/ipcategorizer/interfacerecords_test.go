package ipcategorizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

func TestNormalizeInterfaceName(t *testing.T) {
	cases := map[string]string{
		"Ethernet49/1": "ethernet49-1",
		"et-0/2/6":     "et-0-2-6",
		"lo0.0":        "lo0-0",
		"Management1":  "management1",
		"ae100.0":      "ae100-0",
		"Vlan801":      "vlan801",
		"irb vlan3":    "irb-vlan3",
		"me0":          "me0",
	}
	for in, want := range cases {
		assert.Equal(t, want, NormalizeInterfaceName(in), "normalize %q", in)
	}
}

func TestInterfaceRecords(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 1, Address: "192.168.26.139", Family: 4, DeviceName: "dc1-site13a-r101-prod-eth-tor-01", InterfaceName: "Ethernet1/1"},
		{ID: 2, Address: "172.26.41.73", Family: 4, DeviceName: "dc1-site13a-r101-prod-eth-tor-01", InterfaceName: "Management1"},
		// explicit dns_name → skipped (the explicit record already exists)
		{ID: 3, Address: "10.1.1.1", Family: 4, DeviceName: "dc1-site13a-r101-prod-eth-tor-01", InterfaceName: "Vlan801", DNSName: "svc-vip.example.com"},
		// no interface assignment → skipped
		{ID: 4, Address: "10.1.1.2", Family: 4},
	}

	out := c.InterfaceRecords(records, nil)
	require.Len(t, out, 2, "only interface-assigned, dns_name-less IPs")

	names := map[string]string{}
	for _, r := range out {
		assert.Equal(t, netboxclient.KindInterface, r.Kind, "interface records carry KindInterface")
		names[r.Address] = r.DNSName
	}
	// No formatter → legacy <device>.<zone> canonical; zone from extractZone
	// ("dc1-site13a-..." → "dc1-site.example.com").
	assert.Equal(t, "ethernet1-1.dc1-site13a-r101-prod-eth-tor-01.dc1-site.example.com", names["192.168.26.139"])
	assert.Equal(t, "management1.dc1-site13a-r101-prod-eth-tor-01.dc1-site.example.com", names["172.26.41.73"])
}

func TestInterfaceRecordsDeterministicOrder(t *testing.T) {
	c := newTestCategorizer(t)
	records := []netboxclient.IPRecord{
		{ID: 2, Address: "10.0.0.2", Family: 4, DeviceName: "dc1-site13a-r101-prod-hv-01", InterfaceName: "eno2"},
		{ID: 1, Address: "10.0.0.1", Family: 4, DeviceName: "dc1-site13a-r101-prod-hv-01", InterfaceName: "eno1"},
	}
	a := c.InterfaceRecords(records, nil)
	b := c.InterfaceRecords([]netboxclient.IPRecord{records[1], records[0]}, nil)
	assert.Equal(t, a, b, "output must not depend on input order")
}
