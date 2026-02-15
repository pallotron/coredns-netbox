package zonediscovery

import (
	"fmt"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// DiscoveryMode defines the strategy for grouping records into DNS zones.
type DiscoveryMode string

const (
	ModeCommonSuffix DiscoveryMode = "common-suffix"
	ModeZoneDepth    DiscoveryMode = "zone-depth"
	ModeNetboxDNS    DiscoveryMode = "netbox-dns"
)

// ZoneMap maps zone names (without trailing dot) to the records belonging to that zone.
type ZoneMap map[string][]netboxclient.IPRecord

// Discoverer groups IP records into DNS zones.
type Discoverer interface {
	Discover(records []netboxclient.IPRecord) (ZoneMap, error)
}

// NewDiscoverer creates a Discoverer for the given mode.
// opts is mode-specific:
//   - zone-depth: depth int
//   - netbox-dns: netboxURL, netboxToken string
//   - common-suffix: (no extra opts)
func NewDiscoverer(mode DiscoveryMode, opts map[string]string) (Discoverer, error) {
	switch mode {
	case ModeZoneDepth:
		depth := 2
		if v, ok := opts["depth"]; ok {
			var d int
			if _, err := fmt.Sscanf(v, "%d", &d); err != nil {
				return nil, fmt.Errorf("invalid zone depth %q: %w", v, err)
			}
			depth = d
		}
		return &ZoneDepthDiscoverer{Depth: depth}, nil

	case ModeCommonSuffix:
		return &CommonSuffixDiscoverer{}, nil

	case ModeNetboxDNS:
		url := opts["netbox_url"]
		token := opts["netbox_token"]
		if url == "" || token == "" {
			return nil, fmt.Errorf("netbox-dns mode requires netbox_url and netbox_token")
		}
		return NewNetboxDNSDiscoverer(url, token), nil

	default:
		return nil, fmt.Errorf("unknown discovery mode: %q", mode)
	}
}
