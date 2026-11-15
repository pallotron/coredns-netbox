package ipcategorizer

import (
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/nameformat"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// DNS limits (RFC 1035): 63 octets per label, 253 for a full name in
// presentation form without the trailing dot.
const (
	maxDNSLabelLen = 63
	maxDNSNameLen  = 253
)

// maxSkippedExamples bounds the examples quoted in the per-call warning.
const maxSkippedExamples = 5

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
	var skipped []string
	for _, r := range records {
		if r.DeviceName == "" || r.InterfaceName == "" || r.DNSName != "" || isLinkLocal(r.Address) {
			continue
		}
		label := NormalizeInterfaceName(r.InterfaceName)
		if label == "" {
			continue
		}
		canonical := canonicalFQDN(r.DeviceName, c.extractZone(r.DeviceName), formatter)
		name := label + "." + canonical
		if len(label) > maxDNSLabelLen || len(strings.TrimSuffix(name, ".")) > maxDNSNameLen {
			// Zone generation would drop it as unsafe, after it had already
			// displaced the device's valid PTR in reverse discovery.
			slog.Debug("skipping interface record: name too long for DNS",
				"device", r.DeviceName, "interface", r.InterfaceName, "address", r.Address,
				"label_len", len(label), "name_len", len(name))
			skipped = append(skipped, r.DeviceName+"/"+r.InterfaceName)
			continue
		}
		out = append(out, netboxclient.IPRecord{
			DNSName:       name,
			Address:       r.Address,
			Family:        r.Family,
			DeviceName:    r.DeviceName,
			InterfaceName: r.InterfaceName,
			VRF:           r.VRF,
			ID:            r.ID,
			Kind:          netboxclient.KindInterface,
		})
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		examples := skipped
		if len(examples) > maxSkippedExamples {
			examples = examples[:maxSkippedExamples]
		}
		slog.Warn("skipping interface records: name too long for DNS (label over 63 or name over 253 characters); set LOG_LEVEL=DEBUG to list every skipped interface",
			"count", len(skipped), "examples", strings.Join(examples, ", "))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DNSName != out[j].DNSName {
			return out[i].DNSName < out[j].DNSName
		}
		return out[i].Address < out[j].Address
	})
	return out
}
