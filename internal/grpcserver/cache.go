package grpcserver

import (
	"sync"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// NetboxCache holds the last known Netbox records, keyed by zone name.
// Updated after each successful Netbox fetch; read by handlers to check conflicts.
type NetboxCache struct {
	mu      sync.RWMutex
	records map[string][]netboxclient.IPRecord
}

// Update replaces the cache with a fresh zone map.
func (c *NetboxCache) Update(zones map[string][]netboxclient.IPRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = zones
}

// HasRecord returns true if fqdn exists in any Netbox zone.
func (c *NetboxCache) HasRecord(fqdn string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, recs := range c.records {
		for _, r := range recs {
			if r.DNSName == fqdn {
				return true
			}
		}
	}
	return false
}
