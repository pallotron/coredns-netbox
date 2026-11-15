package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

func TestFilterByDevice(t *testing.T) {
	records := []netboxclient.IPRecord{
		{Address: "10.0.0.1", DeviceName: "dc1-r101-hv-01"},
		{Address: "10.0.0.2", DeviceName: "dc1-r102-hv-02"},
		{Address: "10.0.0.3"}, // no device
	}

	assert.Len(t, filterByDevice(records, ""), 3, "an empty filter keeps everything")

	got := filterByDevice(records, "r101")
	require.Len(t, got, 1, "only the matching device's records are kept")
	assert.Equal(t, "10.0.0.1", got[0].Address)
}

// An address with both an explicit dns_name and a generated interface record
// previews no PTR, because the sidecar publishes the explicit one and the
// preview leaves explicit PTRs out. Generated-only addresses are unchanged.
func TestPreviewReverseZonesExplicitWinsAndIsHidden(t *testing.T) {
	disc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	gen := []netboxclient.IPRecord{
		{DNSName: "ethernet1-1.dc1-r101-sw-01.example.com", Address: "10.0.1.9", Family: 4, Kind: netboxclient.KindInterface},
		{DNSName: "dc1-r101-sw-01.example.com", Address: "10.0.1.10", Family: 4, Kind: netboxclient.KindDevice},
	}
	explicit := []netboxclient.IPRecord{
		{DNSName: "vip.example.com", Address: "10.0.1.9", Family: 4},
	}

	zones, err := previewReverseZones(disc, gen, explicit)
	require.NoError(t, err)

	targets := map[string]string{}
	for _, r := range zones["10.in-addr.arpa"] {
		targets[r.Address] = r.DNSName
	}
	assert.NotContains(t, targets, "9.1.0", "the sidecar publishes the explicit PTR here, so no generated PTR is previewed")
	assert.Equal(t, "dc1-r101-sw-01.example.com", targets["10.1.0"], "generated-only addresses keep their PTR")
	assert.NotContains(t, targets, "vip.example.com", "explicit PTRs are not part of the preview")
}
