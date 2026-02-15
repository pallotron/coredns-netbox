package zonediscovery

import (
	"fmt"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// ZoneDepthDiscoverer groups records by taking the last N labels of each FQDN as the zone.
// For example, with Depth=2: "host1.example.org" -> zone "example.org"
// With Depth=3: "db.prod.example.org" -> zone "prod.example.org"
type ZoneDepthDiscoverer struct {
	Depth int
}

func (d *ZoneDepthDiscoverer) Discover(records []netboxclient.IPRecord) (ZoneMap, error) {
	if d.Depth < 2 {
		return nil, fmt.Errorf("zone depth must be >= 2, got %d", d.Depth)
	}

	zm := make(ZoneMap)
	for _, r := range records {
		name := strings.TrimSuffix(r.DNSName, ".")
		labels := strings.Split(name, ".")
		if len(labels) < d.Depth {
			continue // skip records that don't have enough labels
		}
		zone := strings.Join(labels[len(labels)-d.Depth:], ".")
		zm[zone] = append(zm[zone], r)
	}
	return zm, nil
}
