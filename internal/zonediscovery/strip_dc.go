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

	result := make([]netboxclient.IPRecord, len(records))
	for i, r := range records {
		r.DNSName = stripDCFromName(r.DNSName, domainSuffix)
		if r.CNAMETarget != "" {
			r.CNAMETarget = stripDCFromName(r.CNAMETarget, domainSuffix)
		}
		result[i] = r
	}
	return result
}

// stripDCFromName removes the DC label — the label immediately before the
// base domain suffix — from a single name. Names not under domainSuffix, or
// already at hostname.domain depth, are returned unchanged.
func stripDCFromName(name, domainSuffix string) string {
	trimmed := strings.TrimSuffix(name, ".")
	labels := strings.Split(trimmed, ".")
	domainDepth := len(strings.Split(domainSuffix, "."))

	dcIdx := len(labels) - domainDepth - 1
	if dcIdx < 1 {
		return name
	}
	suffix := strings.Join(labels[len(labels)-domainDepth:], ".")
	if !strings.EqualFold(suffix, domainSuffix) {
		return name
	}
	stripped := append(labels[:dcIdx:dcIdx], labels[dcIdx+1:]...)
	return strings.Join(stripped, ".")
}
