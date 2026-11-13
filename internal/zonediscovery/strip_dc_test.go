package zonediscovery

import (
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripDCLabel_NormalCase(t *testing.T) {
	records := []netboxclient.IPRecord{
		{DNSName: "dc-m-rack166-hv-01.dc-m.example.org", Address: "10.0.0.1", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "dc-m-rack166-hv-01.example.org", got[0].DNSName)
}

func TestStripDCLabel_MultipleRecordsDifferentDCs(t *testing.T) {
	records := []netboxclient.IPRecord{
		{DNSName: "host01.nyc.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "host02.lon.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "host03.sfo.example.org", Address: "10.0.0.3", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "host01.example.org", got[0].DNSName)
	assert.Equal(t, "host02.example.org", got[1].DNSName)
	assert.Equal(t, "host03.example.org", got[2].DNSName)
}

func TestStripDCLabel_AlreadyAtDomainDepth(t *testing.T) {
	// hostname.domain — no DC label, should be unchanged
	records := []netboxclient.IPRecord{
		{DNSName: "hostname.example.org", Address: "10.0.0.1", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "hostname.example.org", got[0].DNSName)
}

func TestStripDCLabel_MultiLabelHostname(t *testing.T) {
	// host.role.dc.example.org — strip the label immediately before domain suffix
	records := []netboxclient.IPRecord{
		{DNSName: "host.role.dc.example.org", Address: "10.0.0.1", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "host.role.example.org", got[0].DNSName)
}

func TestStripDCLabel_TrailingDot(t *testing.T) {
	// DNS names sometimes have a trailing dot
	records := []netboxclient.IPRecord{
		{DNSName: "host.dc.example.org.", Address: "10.0.0.1", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "host.example.org", got[0].DNSName)
}

func TestStripDCLabel_ThreeLabelDomainSuffix(t *testing.T) {
	// Domain suffix itself has 3 labels: sub.example.org
	records := []netboxclient.IPRecord{
		{DNSName: "host.dc.sub.example.org", Address: "10.0.0.1", Family: 4},
	}
	got := StripDCLabel(records, "sub.example.org")
	assert.Equal(t, "host.sub.example.org", got[0].DNSName)
}

func TestStripDCLabel_EmptyRecords(t *testing.T) {
	got := StripDCLabel(nil, "example.org")
	assert.Empty(t, got)
}

func TestStripDCLabel_PreservesOtherFields(t *testing.T) {
	records := []netboxclient.IPRecord{
		{DNSName: "host.dc.example.org", Address: "192.168.1.1", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "192.168.1.1", got[0].Address)
	assert.Equal(t, 4, got[0].Family)
}

func TestStripDCLabel_RecordNotUnderDomainSuffix(t *testing.T) {
	// Record belongs to a different domain — leave unchanged
	records := []netboxclient.IPRecord{
		{DNSName: "host.dc.other.com", Address: "10.0.0.1", Family: 4},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "host.dc.other.com", got[0].DNSName)
}

// A generated record's own device name must never be discarded as if it
// were a DC label, even when it lands exactly at the position a true DC
// label would occupy. An interface record ("<iface>.<device>.<domain>")
// hits this: positionally it is indistinguishable from "host.dc.domain",
// but the candidate label here is the device's own identity.
func TestStripDCLabel_PreservesDeviceNameAtDCLabelPosition(t *testing.T) {
	records := []netboxclient.IPRecord{
		{
			DNSName:    "mgmt0.dc1-h1a-r101-prod-hv-01.example.org",
			Address:    "10.9.0.1",
			Family:     4,
			DeviceName: "dc1-h1a-r101-prod-hv-01",
			Kind:       netboxclient.KindInterface,
		},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "mgmt0.dc1-h1a-r101-prod-hv-01.example.org", got[0].DNSName,
		"the device's own name must not be stripped even though it occupies the DC-label position")
}

// A real DC label at that same position, on a record that DOES carry a
// DeviceName (the legacy <device>.<zone> canonical, whose zone component is
// a synthetic tag unrelated to the device name), still gets stripped.
func TestStripDCLabel_StillStripsRealDCLabelWithDeviceNameSet(t *testing.T) {
	records := []netboxclient.IPRecord{
		{
			DNSName:    "oob-sw-01.oob-sw.example.org",
			Address:    "10.9.24.1",
			Family:     4,
			DeviceName: "oob-sw-01",
			Kind:       netboxclient.KindDevice,
		},
	}
	got := StripDCLabel(records, "example.org")
	assert.Equal(t, "oob-sw-01.example.org", got[0].DNSName,
		"a genuine zone-discriminator label distinct from the device name must still be stripped")
}

// A stripped alias must point at the stripped canonical, not the pre-strip name.
func TestStripDCLabelRewritesCNAMETarget(t *testing.T) {
	records := []netboxclient.IPRecord{
		{
			DNSName:     "alias1.nyc.example.org",
			Type:        netboxclient.RecordTypeCNAME,
			CNAMETarget: "host1.nyc.example.org",
		},
	}
	got := StripDCLabel(records, "example.org")
	require.Len(t, got, 1)
	assert.Equal(t, "alias1.example.org", got[0].DNSName)
	assert.Equal(t, "host1.example.org", got[0].CNAMETarget)
}
