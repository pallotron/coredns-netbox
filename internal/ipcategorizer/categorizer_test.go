package ipcategorizer

import (
	"testing"

	"github.com/pallotron/coredns-netbox/internal/nameformat"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Default regex patterns matching config defaults
const (
	defaultBMC       = `(?i)bmc|ipmi|ilo|idrac`
	defaultLoopback  = `^lo$|^lo0|^Loopback`
	defaultDataplane = `(?i)storage|vtep|vsan`
	defaultMgmtVRF   = `(?i)mgmt|oob`
	defaultMgmtIface = `(?i)mgmt|Management|fxp0|eth[01]|mgt|NET`
	defaultDomain    = "example.com"
)

func newDefaultCategorizer(t *testing.T) *Categorizer {
	t.Helper()
	c, err := NewCategorizer(defaultBMC, defaultLoopback, defaultDataplane, defaultMgmtVRF, defaultMgmtIface, defaultDomain)
	require.NoError(t, err, "NewCategorizer with default patterns should succeed")
	return c
}

// ---------- NewCategorizer ----------

func TestNewCategorizer_Valid(t *testing.T) {
	c, err := NewCategorizer(defaultBMC, defaultLoopback, defaultDataplane, defaultMgmtVRF, defaultMgmtIface, defaultDomain)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestNewCategorizer_InvalidRegex(t *testing.T) {
	tests := []struct {
		name string
		bmc, loopback, dataplane, mgmtVRF, mgmtIface string
	}{
		{"bad BMC", "[invalid", defaultLoopback, defaultDataplane, defaultMgmtVRF, defaultMgmtIface},
		{"bad loopback", defaultBMC, "[invalid", defaultDataplane, defaultMgmtVRF, defaultMgmtIface},
		{"bad dataplane", defaultBMC, defaultLoopback, "[invalid", defaultMgmtVRF, defaultMgmtIface},
		{"bad mgmtVRF", defaultBMC, defaultLoopback, defaultDataplane, "[invalid", defaultMgmtIface},
		{"bad mgmtIface", defaultBMC, defaultLoopback, defaultDataplane, defaultMgmtVRF, "[invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCategorizer(tt.bmc, tt.loopback, tt.dataplane, tt.mgmtVRF, tt.mgmtIface, defaultDomain)
			assert.Error(t, err)
			assert.Nil(t, c)
		})
	}
}

// ---------- Categorize ----------

func TestCategorize(t *testing.T) {
	c := newDefaultCategorizer(t)

	tests := []struct {
		name     string
		record   netboxclient.IPRecord
		expected InterfaceCategory
	}{
		// BMC interfaces
		{"bmc lowercase", netboxclient.IPRecord{InterfaceName: "bmc0"}, CategoryBMC},
		{"ipmi", netboxclient.IPRecord{InterfaceName: "ipmi"}, CategoryBMC},
		{"ilo", netboxclient.IPRecord{InterfaceName: "iLO"}, CategoryBMC},
		{"idrac", netboxclient.IPRecord{InterfaceName: "iDRAC"}, CategoryBMC},

		// Loopback interfaces
		{"lo exact", netboxclient.IPRecord{InterfaceName: "lo"}, CategoryLoopback},
		{"lo0", netboxclient.IPRecord{InterfaceName: "lo0"}, CategoryLoopback},
		{"Loopback0", netboxclient.IPRecord{InterfaceName: "Loopback0"}, CategoryLoopback},

		// Dataplane interfaces
		{"storage", netboxclient.IPRecord{InterfaceName: "storage0"}, CategoryDataplane},
		{"vtep", netboxclient.IPRecord{InterfaceName: "vtep1"}, CategoryDataplane},
		{"vsan", netboxclient.IPRecord{InterfaceName: "VSAN"}, CategoryDataplane},

		// Mgmt VRF (matched on VRF field, not interface name)
		{"mgmt vrf", netboxclient.IPRecord{InterfaceName: "eth2", VRF: "mgmt"}, CategoryMgmtVRF},
		{"oob vrf", netboxclient.IPRecord{InterfaceName: "ge-0/0/0", VRF: "OOB"}, CategoryMgmtVRF},

		// Mgmt interface (name-based)
		{"fxp0", netboxclient.IPRecord{InterfaceName: "fxp0"}, CategoryMgmtInterface},
		{"eth0", netboxclient.IPRecord{InterfaceName: "eth0"}, CategoryMgmtInterface},
		{"eth1", netboxclient.IPRecord{InterfaceName: "eth1"}, CategoryMgmtInterface},
		{"Management1", netboxclient.IPRecord{InterfaceName: "Management1"}, CategoryMgmtInterface},
		{"mgt0", netboxclient.IPRecord{InterfaceName: "mgt0"}, CategoryMgmtInterface},
		{"NET0", netboxclient.IPRecord{InterfaceName: "NET0"}, CategoryMgmtInterface},

		// Unknown
		{"unknown ge", netboxclient.IPRecord{InterfaceName: "ge-0/0/1"}, CategoryUnknown},
		{"Ethernet matches NET pattern", netboxclient.IPRecord{InterfaceName: "Ethernet1/1"}, CategoryMgmtInterface}, // matches "NET" case-insensitive
		{"unknown swp", netboxclient.IPRecord{InterfaceName: "swp1"}, CategoryUnknown},
		{"empty interface", netboxclient.IPRecord{InterfaceName: ""}, CategoryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.Categorize(tt.record)
			assert.Equal(t, tt.expected, got, "interface=%q vrf=%q", tt.record.InterfaceName, tt.record.VRF)
		})
	}
}

func TestCategorize_BMCBeatsMgmtVRF(t *testing.T) {
	c := newDefaultCategorizer(t)

	// Interface name matches BMC, VRF matches mgmt -- BMC should win (higher priority)
	record := netboxclient.IPRecord{
		InterfaceName: "ipmi",
		VRF:           "mgmt",
	}
	got := c.Categorize(record)
	assert.Equal(t, CategoryBMC, got, "BMC should take priority over mgmt VRF")
}

// ---------- InterfaceCategory.String ----------

func TestInterfaceCategoryString(t *testing.T) {
	tests := []struct {
		cat  InterfaceCategory
		want string
	}{
		{CategoryUnknown, "unknown"},
		{CategoryLoopback, "loopback"},
		{CategoryDataplane, "dataplane"},
		{CategoryBMC, "bmc"},
		{CategoryMgmtVRF, "mgmt-vrf"},
		{CategoryMgmtInterface, "mgmt-interface"},
		{InterfaceCategory(99), "unknown"}, // out-of-range falls to default
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.cat.String())
		})
	}
}

// ---------- SelectDeviceIPs ----------

func TestSelectDeviceIPs_GroupsByDevice_PicksPrimary(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "dc1-site1-r101-prod-hv-01", InterfaceName: "eth0", Address: "10.0.0.1", Family: 4, VRF: ""},
		{DeviceName: "dc1-site1-r101-prod-hv-01", InterfaceName: "ipmi", Address: "10.0.1.1", Family: 4, VRF: ""},
	}

	result := c.SelectDeviceIPs(records)
	require.Contains(t, result, "dc1-site1-r101-prod-hv-01")

	dev := result["dc1-site1-r101-prod-hv-01"]
	require.NotNil(t, dev.PrimaryIP, "should have a primary IP from mgmt interface")
	assert.Equal(t, "10.0.0.1", dev.PrimaryIP.Address)
	require.NotNil(t, dev.BMCIP, "should have a BMC IP")
	assert.Equal(t, "10.0.1.1", dev.BMCIP.Address)
}

func TestSelectDeviceIPs_VRFPreferredOverIfaceName(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		// mgmt interface match (eth0)
		{DeviceName: "dev1", InterfaceName: "eth0", Address: "10.0.0.1", Family: 4, VRF: ""},
		// mgmt VRF match (ge-0/0/0 in mgmt VRF)
		{DeviceName: "dev1", InterfaceName: "ge-0/0/0", Address: "10.0.0.2", Family: 4, VRF: "mgmt"},
	}

	result := c.SelectDeviceIPs(records)
	require.Contains(t, result, "dev1")

	dev := result["dev1"]
	require.NotNil(t, dev.PrimaryIP)
	assert.Equal(t, "10.0.0.2", dev.PrimaryIP.Address, "VRF-based IP should be preferred over interface-name-based")
}

func TestSelectDeviceIPs_SkipsEmptyDeviceName(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "", InterfaceName: "eth0", Address: "10.0.0.1", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	assert.Empty(t, result, "records with empty device name should be skipped")
}

func TestSelectDeviceIPs_ExcludesDevicesWithOnlyUnknownInterfaces(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		// Only unknown interfaces for this device (swp* doesn't match any pattern)
		{DeviceName: "dev-unknown", InterfaceName: "swp1", Address: "10.0.0.1", Family: 4},
		{DeviceName: "dev-unknown", InterfaceName: "swp2", Address: "10.0.0.2", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	assert.NotContains(t, result, "dev-unknown", "device with only unknown interfaces should be excluded")
}

func TestSelectDeviceIPs_MultipleDevices(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "dev-a", InterfaceName: "eth0", Address: "10.0.0.1", Family: 4},
		{DeviceName: "dev-b", InterfaceName: "fxp0", Address: "10.0.1.1", Family: 4},
		{DeviceName: "dev-b", InterfaceName: "bmc0", Address: "10.0.1.2", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	assert.Len(t, result, 2)
	require.Contains(t, result, "dev-a")
	require.Contains(t, result, "dev-b")
	assert.Equal(t, "10.0.0.1", result["dev-a"].PrimaryIP.Address)
	assert.Equal(t, "10.0.1.1", result["dev-b"].PrimaryIP.Address)
	require.NotNil(t, result["dev-b"].BMCIP)
	assert.Equal(t, "10.0.1.2", result["dev-b"].BMCIP.Address)
}

func TestSelectDeviceIPs_SortsDeterministically(t *testing.T) {
	c := newDefaultCategorizer(t)

	// Two mgmt-VRF IPs for same device -- should pick the one with the
	// lexicographically smallest interface name.
	records := []netboxclient.IPRecord{
		{DeviceName: "dev1", InterfaceName: "ge-0/0/1", Address: "10.0.0.2", Family: 4, VRF: "mgmt"},
		{DeviceName: "dev1", InterfaceName: "ge-0/0/0", Address: "10.0.0.1", Family: 4, VRF: "mgmt"},
	}

	result := c.SelectDeviceIPs(records)
	require.Contains(t, result, "dev1")
	assert.Equal(t, "10.0.0.1", result["dev1"].PrimaryIP.Address, "should pick ge-0/0/0 (sorted first)")
}

func TestSelectDeviceIPs_BMCOnly(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "dev1", InterfaceName: "ipmi", Address: "10.0.0.1", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	require.Contains(t, result, "dev1", "device with only BMC should still be included")
	assert.Nil(t, result["dev1"].PrimaryIP)
	require.NotNil(t, result["dev1"].BMCIP)
	assert.Equal(t, "10.0.0.1", result["dev1"].BMCIP.Address)
}

func TestSelectDeviceIPs_SetsZone(t *testing.T) {
	c := newDefaultCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "dc1-site13a-r101-prod-hv-01", InterfaceName: "eth0", Address: "10.0.0.1", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	require.Contains(t, result, "dc1-site13a-r101-prod-hv-01")
	assert.Equal(t, "dc1-site.example.com", result["dc1-site13a-r101-prod-hv-01"].Zone)
}

// ---------- extractZone ----------

func TestExtractZone(t *testing.T) {
	c := newDefaultCategorizer(t)

	tests := []struct {
		name       string
		deviceName string
		want       string
	}{
		{"site with digits and letter", "dc1-site13a-r101-prod-hv-01", "dc1-site.example.com"},
		{"short site", "dc2-m21-r101-prod-hv-01", "dc2-m.example.com"},
		{"site alpha only", "dc1-core-sw-01", "dc1-core.example.com"},
		{"single part (no dash)", "hostname", "example.com"},
		{"two parts", "dc1-sw01", "dc1-sw.example.com"},
		{"site with digits suffix", "dc3-cu2a-leaf-01", "dc3-cu.example.com"},
		{"single-site rack pattern", "dc1-r101-pdu-left-01", "dc1.example.com"},
		{"lab device pattern", "loc1-site2a-r301-lab-dev-hv-01", "loc1-lab-dev.example.com"},
		{"lab device with staging", "loc2-site3b-r401-lab-staging-srv-01", "loc2-lab-staging.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.extractZone(tt.deviceName)
			assert.Equal(t, tt.want, got)
		})
	}
}

// newTestFormatter returns the same formatter configuration used in nameformat
// package tests, suitable for exercising DeviceDNSToRecords with real-world
// patterns.
func newTestFormatter(t *testing.T) *nameformat.Formatter {
	t.Helper()
	f, err := nameformat.New(
		[]string{
			// standard device with env: dc1-h2a-r101-prod-hv-01
			`^(?P<dc>[a-z0-9]+)-(?P<hall>[a-z]+[0-9][a-z0-9]*)-r(?P<rack>[0-9]+)-(?P<env>prod|mgmt|staging)-(?P<role>[a-z][a-z0-9-]*?)-(?P<idx>[0-9]+)$`,
			// no hall: site01-r101-pdu-left-01
			`^(?P<dc>[a-z0-9]+)-r(?P<rack>[0-9]+)-(?P<role>[a-z][a-z0-9-]*?)-(?P<idx>[0-9]+)$`,
		},
		`{{.name}}.{{template "zone" .}}`,
		[]string{`{{.role}}{{.idx}}-{{.dc}}{{if .hall}}-{{.hall}}{{end}}-r{{.rack}}.{{template "zone" .}}`},
		`{{.dc}}{{if .hall}}-{{alphaPrefix .hall}}{{end}}.{{.domain}}`,
		"example.org",
	)
	require.NoError(t, err)
	return f
}

func TestDeviceDNSToRecordsWithFormatter(t *testing.T) {
	formatter := newTestFormatter(t)

	deviceDNS := map[string]*DeviceDNSRecords{
		"dc1-h2a-r101-prod-hv-01": {
			DeviceName: "dc1-h2a-r101-prod-hv-01",
			Zone:       "dc1-h.example.org", // legacy zone; ignored when the parser matches
			PrimaryIP:  &netboxclient.IPRecord{Address: "10.0.0.1", Family: 4, InterfaceName: "mgmt0"},
			BMCIP:      &netboxclient.IPRecord{Address: "10.0.8.1", Family: 4, InterfaceName: "bmc0"},
		},
		// does not match the parser -> legacy naming, no aliases
		"core-router-wan": {
			DeviceName: "core-router-wan",
			Zone:       "core.example.org",
			PrimaryIP:  &netboxclient.IPRecord{Address: "10.0.0.9", Family: 4},
		},
	}

	records := DeviceDNSToRecords(deviceDNS, formatter)

	byName := map[string]netboxclient.IPRecord{}
	for _, r := range records {
		byName[r.DNSName] = r
	}
	require.Len(t, records, 5)

	canonical := byName["dc1-h2a-r101-prod-hv-01.dc1-h.example.org"]
	assert.Equal(t, "10.0.0.1", canonical.Address, "primary A at canonical name")
	assert.Empty(t, canonical.Type)

	bmc := byName["dc1-h2a-r101-prod-hv-01-bmc.dc1-h.example.org"]
	assert.Equal(t, "10.0.8.1", bmc.Address, "BMC A at canonical-bmc name")

	alias := byName["hv01-dc1-h2a-r101.dc1-h.example.org"]
	assert.Equal(t, netboxclient.RecordTypeCNAME, alias.Type)
	assert.Equal(t, "dc1-h2a-r101-prod-hv-01.dc1-h.example.org", alias.CNAMETarget)
	assert.Equal(t, "dc1-h2a-r101-prod-hv-01", alias.DeviceName)

	bmcAlias := byName["hv01-dc1-h2a-r101-bmc.dc1-h.example.org"]
	assert.Equal(t, netboxclient.RecordTypeCNAME, bmcAlias.Type)
	assert.Equal(t, "dc1-h2a-r101-prod-hv-01-bmc.dc1-h.example.org", bmcAlias.CNAMETarget)

	legacy := byName["core-router-wan.core.example.org"]
	assert.Equal(t, "10.0.0.9", legacy.Address, "non-matching device keeps legacy name")
}

func TestDeviceDNSToRecordsNilFormatterIsLegacy(t *testing.T) {
	deviceDNS := map[string]*DeviceDNSRecords{
		"dev1": {
			DeviceName: "dev1",
			Zone:       "example.org",
			PrimaryIP:  &netboxclient.IPRecord{Address: "10.0.0.1", Family: 4},
			BMCIP:      &netboxclient.IPRecord{Address: "10.0.8.1", Family: 4},
		},
	}
	records := DeviceDNSToRecords(deviceDNS, nil)
	require.Len(t, records, 2)
	assert.Equal(t, "dev1.example.org", records[0].DNSName)
	assert.Equal(t, "dev1-bmc.example.org", records[1].DNSName)
}
