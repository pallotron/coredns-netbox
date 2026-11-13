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
//
// A device's own name is never eligible to be discarded as a DC label (see
// stripDCFromName) even when it lands at that position, which happens
// whenever a generated record prepends an extra label onto an
// already-complete device canonical — an interface record
// ("mgmt0.dc1-h1a-r101-prod-hv-01.example.org") being the case that exposed
// it: positionally this looks identical to a true "host.dc.domain" name, but
// the candidate label here is the device's own identity, not a discardable
// DC tag, and stripping it collapses every device's interface records onto
// the same non-unique "<iface>.example.org" name.
func StripDCLabel(records []netboxclient.IPRecord, domainSuffix string) []netboxclient.IPRecord {
	if len(records) == 0 {
		return records
	}

	result := make([]netboxclient.IPRecord, len(records))
	for i, r := range records {
		r.DNSName = stripDCFromName(r.DNSName, domainSuffix, r.DeviceName)
		if r.CNAMETarget != "" {
			r.CNAMETarget = stripDCFromName(r.CNAMETarget, domainSuffix, r.DeviceName)
		}
		result[i] = r
	}
	return result
}

// stripDCFromName removes the DC label — the label immediately before the
// base domain suffix — from a single name. Names not under domainSuffix, or
// already at hostname.domain depth, are returned unchanged. deviceName (the
// record's own device, "" for explicit/operator-named records) guards
// against discarding a generated record's own device identity: if the
// candidate DC label is actually the device's name, the name is left alone.
func stripDCFromName(name, domainSuffix, deviceName string) string {
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
	if deviceName != "" && strings.EqualFold(labels[dcIdx], deviceName) {
		return name
	}
	stripped := append(labels[:dcIdx:dcIdx], labels[dcIdx+1:]...)
	return strings.Join(stripped, ".")
}
