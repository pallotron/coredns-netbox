package ipcategorizer

import (
	"regexp"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/nameformat"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// nonDNSLabelChars matches every run of characters not allowed in the
// normalized interface label.
var nonDNSLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// NormalizeInterfaceName maps a vendor interface name to a valid DNS label:
// lowercase, with every run of characters outside [a-z0-9-] collapsed to a
// single dash, and leading/trailing dashes trimmed.
// "Ethernet49/1" → "ethernet49-1", "et-0/2/6" → "et-0-2-6", "lo0.0" → "lo0-0".
func NormalizeInterfaceName(name string) string {
	n := strings.ToLower(name)
	n = nonDNSLabelChars.ReplaceAllString(n, "-")
	return strings.Trim(n, "-")
}

// InterfaceRecords generates one address record per interface-assigned IP
// without an explicit dns_name, named <iface>.<device-canonical-fqdn> in the
// device's zone (spec §3). Explicit dns_name records are untouched elsewhere
// and win PTR priority, so they are skipped here. Output is sorted for
// deterministic zone generation.
func (c *Categorizer) InterfaceRecords(
	records []netboxclient.IPRecord,
	formatter *nameformat.Formatter,
) []netboxclient.IPRecord {
	var out []netboxclient.IPRecord
	for _, r := range records {
		if r.DeviceName == "" || r.InterfaceName == "" || r.DNSName != "" {
			continue
		}
		label := NormalizeInterfaceName(r.InterfaceName)
		if label == "" {
			continue
		}
		canonical := canonicalFQDN(r.DeviceName, c.extractZone(r.DeviceName), formatter)
		out = append(out, netboxclient.IPRecord{
			DNSName:       label + "." + canonical,
			Address:       r.Address,
			Family:        r.Family,
			DeviceName:    r.DeviceName,
			InterfaceName: r.InterfaceName,
			VRF:           r.VRF,
			ID:            r.ID,
			Kind:          netboxclient.KindInterface,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DNSName != out[j].DNSName {
			return out[i].DNSName < out[j].DNSName
		}
		return out[i].Address < out[j].Address
	})
	return out
}
