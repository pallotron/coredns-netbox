package zonediscovery

import (
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// StripDCLabel removes the DC label — the label immediately before the base
// domain suffix — from each record's DNS name. Records not under domainSuffix,
// or already at hostname.domain depth, are left unchanged.
//
// Example: "host.nyc.example.org" with domainSuffix "example.org" → "host.example.org"
func StripDCLabel(records []netboxclient.IPRecord, domainSuffix string) []netboxclient.IPRecord {
	if len(records) == 0 {
		return records
	}

	domainLabels := strings.Split(domainSuffix, ".")
	domainDepth := len(domainLabels)

	result := make([]netboxclient.IPRecord, len(records))
	for i, r := range records {
		name := strings.TrimSuffix(r.DNSName, ".")
		labels := strings.Split(name, ".")

		// Only transform records that are under domainSuffix and have a DC label to strip.
		// Need at least domainDepth+2 labels: hostname + dc + domain...
		dcIdx := len(labels) - domainDepth - 1
		if dcIdx < 1 {
			result[i] = r
			continue
		}

		// Verify the record ends with the expected domain suffix.
		suffix := strings.Join(labels[len(labels)-domainDepth:], ".")
		if !strings.EqualFold(suffix, domainSuffix) {
			result[i] = r
			continue
		}

		stripped := append(labels[:dcIdx:dcIdx], labels[dcIdx+1:]...)
		r.DNSName = strings.Join(stripped, ".")
		result[i] = r
	}
	return result
}
