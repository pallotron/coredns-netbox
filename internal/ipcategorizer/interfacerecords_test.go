package ipcategorizer

import (
	"bytes"
	"log/slog"
	"strings"
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

// An interface name whose label would exceed 63 characters (NetBox allows
// 64) gets no interface record: it would be dropped as unsafe later anyway,
// after first displacing the device's valid PTR. Normal names on the same
// device still get theirs. One WARN per call reports the count and how to
// get the full list; each skipped interface is logged at DEBUG.
func TestInterfaceRecordsSkipsNamesTooLongForDNS(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := newTestCategorizer(t)
	const dev = "dc1-h1a-r101-prod-vm-07"
	longIface := "port-3f6c2a1e-9b7d-4c5a-8e2f-1d0b9a7c6e54-provider-net-vlan-1001" // 64 chars
	okIface := "port-3f6c2a1e-9b7d-4c5a-8e2f-1d0b9a7c6e54-provider-net-vlan1001"    // 63 chars
	require.Len(t, longIface, 64)
	require.Len(t, okIface, 63)
	records := []netboxclient.IPRecord{
		{ID: 1, Address: "10.20.0.15", Family: 4, DeviceName: dev, InterfaceName: longIface},
		{ID: 2, Address: "10.20.0.16", Family: 4, DeviceName: dev, InterfaceName: okIface},
		{ID: 3, Address: "10.20.0.17", Family: 4, DeviceName: dev, InterfaceName: "mgmt0"},
	}

	got := c.InterfaceRecords(records, nil)
	addrs := make([]string, 0, len(got))
	for _, r := range got {
		addrs = append(addrs, r.Address)
		for _, label := range strings.Split(r.DNSName, ".") {
			assert.LessOrEqual(t, len(label), 63, "every label must fit DNS: %s", r.DNSName)
		}
	}
	assert.ElementsMatch(t, []string{"10.20.0.16", "10.20.0.17"}, addrs,
		"the 64-char interface is skipped; the 63-char one and mgmt0 are kept")

	logs := buf.String()
	assert.Equal(t, 1, strings.Count(logs, "level=WARN"), "exactly one warning per call")
	assert.Contains(t, logs, "count=1")
	assert.Contains(t, logs, "LOG_LEVEL=DEBUG", "the warning says how to get the full list")
	assert.Contains(t, logs, "level=DEBUG", "each skipped interface is logged at debug")
	assert.Contains(t, logs, longIface)
}

// A full name over 253 characters is skipped the same way, even when each
// label fits.
func TestInterfaceRecordsSkipsFullNameTooLong(t *testing.T) {
	c := newTestCategorizer(t)
	// Three valid 60-char labels: the device name fits, but a 63-char
	// interface label in front pushes the full name past 253.
	dev := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60)
	iface := strings.Repeat("d", 63)
	records := []netboxclient.IPRecord{
		{ID: 1, Address: "10.20.0.20", Family: 4, DeviceName: dev, InterfaceName: iface},
		{ID: 2, Address: "10.20.0.21", Family: 4, DeviceName: dev, InterfaceName: "mgmt0"},
	}
	got := c.InterfaceRecords(records, nil)
	addrs := make([]string, 0, len(got))
	for _, r := range got {
		addrs = append(addrs, r.Address)
		assert.LessOrEqual(t, len(r.DNSName), 253, "full name must fit DNS: %s", r.DNSName)
	}
	assert.NotContains(t, addrs, "10.20.0.20", "the over-253 name is skipped")
	assert.Contains(t, addrs, "10.20.0.21", "mgmt0 on the same device still fits and is kept")
}
