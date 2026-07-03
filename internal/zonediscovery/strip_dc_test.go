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
