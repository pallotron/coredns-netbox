package zonemanager

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonegen"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

// Manager maintains one zonegen.Generator per discovered zone and writes
// zone files to a directory using the naming convention db.<zoneName>.
type Manager struct {
	zoneDir    string
	primaryNS  string
	adminEmail string
	ttl        uint32

	generators map[string]*zonegen.Generator
}

// UpdateStats holds counters for zone file operations performed during an Update call.
type UpdateStats struct {
	Created     int
	Updated     int
	Deleted     int
	WriteErrors int
}

// New creates a new zone Manager.
func New(zoneDir, primaryNS, adminEmail string, ttl uint32) *Manager {
	return &Manager{
		zoneDir:    zoneDir,
		primaryNS:  primaryNS,
		adminEmail: adminEmail,
		ttl:        ttl,
		generators: make(map[string]*zonegen.Generator),
	}
}

// Update takes a ZoneMap (zone name -> records), generates zone files for each
// zone, writes them to disk, and removes orphaned zone files.
func (m *Manager) Update(zoneMap zonediscovery.ZoneMap) (UpdateStats, error) {
	var stats UpdateStats
	activeZones := make(map[string]bool)

	// Sort zone names for deterministic output
	var zones []string
	for z := range zoneMap {
		zones = append(zones, z)
	}
	sort.Strings(zones)

	for _, zone := range zones {
		records := zoneMap[zone]
		activeZones[zone] = true

		gen, ok := m.generators[zone]
		isNew := !ok
		if !ok {
			// Determine zone type based on zone name
			zoneType := zonegen.ZoneTypeForward
			if isReverseZone(zone) {
				zoneType = zonegen.ZoneTypeReverse
			}

			gen = zonegen.NewGenerator(zonegen.ZoneConfig{
				Origin:     zone,
				PrimaryNS:  m.primaryNS,
				AdminEmail: m.adminEmail,
				TTL:        m.ttl,
				Type:       zoneType,
			})
			m.generators[zone] = gen
		}

		content, changed, err := gen.Generate(records)
		if err != nil {
			return stats, fmt.Errorf("generate zone %s: %w", zone, err)
		}

		if !changed {
			slog.Info("zone unchanged", "zone", zone)
			continue
		}

		path := m.zonePath(zone)
		if err := zonegen.WriteFile(path, content); err != nil {
			stats.WriteErrors++
			return stats, fmt.Errorf("write zone file %s: %w", path, err)
		}
		if isNew {
			stats.Created++
		} else {
			stats.Updated++
		}
		slog.Info("zone updated", "zone", zone, "records", len(records))
	}

	// Remove orphaned zone files and generators
	deleted, err := m.removeOrphans(activeZones)
	stats.Deleted = deleted
	if err != nil {
		return stats, fmt.Errorf("remove orphans: %w", err)
	}

	return stats, nil
}

// HasExistingZones returns true if at least one zone file (db.*) already
// exists on disk. Used by the init container to decide whether a cached copy
// is available when Netbox is unreachable on startup.
func (m *Manager) HasExistingZones() bool {
	entries, err := os.ReadDir(m.zoneDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "db.") {
			return true
		}
	}
	return false
}

// Zones returns the list of currently managed zone names.
func (m *Manager) Zones() []string {
	var zones []string
	for z := range m.generators {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	return zones
}

func (m *Manager) zonePath(zone string) string {
	return filepath.Join(m.zoneDir, "db."+zone)
}

func (m *Manager) removeOrphans(activeZones map[string]bool) (int, error) {
	var deleted int

	// Clean up generators for zones no longer active
	for zone := range m.generators {
		if !activeZones[zone] {
			path := m.zonePath(zone)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return deleted, fmt.Errorf("remove orphan zone file %s: %w", path, err)
			}
			deleted++
			slog.Info("zone removed (orphaned)", "zone", zone)
			delete(m.generators, zone)
		}
	}

	// Also scan the directory for zone files not tracked by any generator
	entries, err := os.ReadDir(m.zoneDir)
	if err != nil {
		if os.IsNotExist(err) {
			return deleted, nil
		}
		return deleted, fmt.Errorf("read zone dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "db.") {
			continue
		}
		zone := strings.TrimPrefix(name, "db.")
		if !activeZones[zone] {
			path := filepath.Join(m.zoneDir, name)
			if err := os.Remove(path); err != nil {
				return deleted, fmt.Errorf("remove stale zone file %s: %w", path, err)
			}
			deleted++
			slog.Info("zone removed (stale file)", "zone", zone)
		}
	}

	return deleted, nil
}

// RecordsForZone is a convenience type alias.
type RecordsForZone = []netboxclient.IPRecord

// isReverseZone checks if a zone name is a reverse DNS zone.
func isReverseZone(zone string) bool {
	return strings.HasSuffix(zone, ".in-addr.arpa") || strings.HasSuffix(zone, ".ip6.arpa")
}
