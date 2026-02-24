package zonediscovery

import (
	"log/slog"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// CommonSuffixDiscoverer groups records by their longest common domain suffix.
// It finds the common suffix shared across all FQDNs and uses it as the zone.
// If records span multiple TLDs, they are grouped by second-level domain independently.
type CommonSuffixDiscoverer struct{}

func (d *CommonSuffixDiscoverer) Discover(records []netboxclient.IPRecord) (ZoneMap, error) {
	if len(records) == 0 {
		return ZoneMap{}, nil
	}

	// Group records by their TLD first to handle mixed TLDs
	tldGroups := make(map[string][]netboxclient.IPRecord)
	for _, r := range records {
		name := strings.TrimSuffix(r.DNSName, ".")
		labels := strings.Split(name, ".")
		if len(labels) < 2 {
			continue
		}
		tld := labels[len(labels)-1]
		tldGroups[tld] = append(tldGroups[tld], r)
	}

	zm := make(ZoneMap)
	for _, group := range tldGroups {
		zone := commonSuffix(group)
		if zone == "" {
			continue
		}
		zm[zone] = append(zm[zone], group...)
	}
	return zm, nil
}

// commonSuffix finds the longest common domain suffix among a group of records.
// Returns at least a 2-label suffix (e.g. "example.org").
func commonSuffix(records []netboxclient.IPRecord) string {
	if len(records) == 0 {
		return ""
	}

	// Reverse labels for each record
	var reversed [][]string
	for _, r := range records {
		name := strings.TrimSuffix(r.DNSName, ".")
		labels := strings.Split(name, ".")
		rev := make([]string, len(labels))
		for i, l := range labels {
			rev[len(labels)-1-i] = l
		}
		reversed = append(reversed, rev)
	}

	// Find common prefix of reversed labels
	ref := reversed[0]
	commonLen := len(ref)
	for _, rev := range reversed[1:] {
		maxCheck := commonLen
		if len(rev) < maxCheck {
			maxCheck = len(rev)
		}
		newCommon := 0
		for i := 0; i < maxCheck; i++ {
			if rev[i] != ref[i] {
				break
			}
			newCommon++
		}
		commonLen = newCommon
	}

	// Must be at least 2 labels (e.g. example.org)
	if commonLen < 2 {
		slog.Warn("no common depth >=2 in group, falling back to 2-label suffix of first record",
			"first_record", records[0].DNSName)
		// Fall back to second-level domain of the first record
		name := strings.TrimSuffix(records[0].DNSName, ".")
		labels := strings.Split(name, ".")
		if len(labels) >= 2 {
			commonLen = 2
			ref = make([]string, len(labels))
			for i, l := range labels {
				ref[len(labels)-1-i] = l
			}
		} else {
			return ""
		}
	}

	// Rebuild the zone from reversed common prefix
	common := make([]string, commonLen)
	for i := 0; i < commonLen; i++ {
		common[commonLen-1-i] = ref[i]
	}

	zone := strings.Join(common, ".")

	// The zone should not equal any full FQDN — it must be a proper suffix.
	// If all records have the same FQDN, use 2-label suffix.
	for _, r := range records {
		name := strings.TrimSuffix(r.DNSName, ".")
		if name != zone {
			return zone
		}
	}

	// All FQDNs are the same as the zone — use the last 2 labels
	slog.Warn("computed zone equals all FQDNs, truncating to 2-label suffix",
		"zone", zone)
	labels := strings.Split(zone, ".")
	if len(labels) > 2 {
		return strings.Join(labels[len(labels)-2:], ".")
	}
	return zone
}
