package zonediscovery

import (
	"sort"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// CNAMEOnlyZones returns the names of zones whose records are exclusively
// CNAMEs. Such a zone was created solely by alias templates; statically
// configured secondaries must enumerate it explicitly or they will silently
// never transfer it. The result is sorted for deterministic logging.
func CNAMEOnlyZones(zm ZoneMap) []string {
	var zones []string
	for zone, recs := range zm {
		if len(recs) == 0 {
			continue
		}
		allCNAME := true
		for _, r := range recs {
			if r.Type != netboxclient.RecordTypeCNAME {
				allCNAME = false
				break
			}
		}
		if allCNAME {
			zones = append(zones, zone)
		}
	}
	sort.Strings(zones)
	return zones
}
