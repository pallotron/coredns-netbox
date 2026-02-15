package zonemanager

import (
	"fmt"
	"log"
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
func (m *Manager) Update(zoneMap zonediscovery.ZoneMap) error {
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
		if !ok {
			gen = zonegen.NewGenerator(zonegen.ZoneConfig{
				Origin:     zone,
				PrimaryNS:  m.primaryNS,
				AdminEmail: m.adminEmail,
				TTL:        m.ttl,
			})
			m.generators[zone] = gen
		}

		content, changed, err := gen.Generate(records)
		if err != nil {
			return fmt.Errorf("generate zone %s: %w", zone, err)
		}

		if !changed {
			log.Printf("Zone %s: no changes", zone)
			continue
		}

		path := m.zonePath(zone)
		if err := zonegen.WriteFile(path, content); err != nil {
			return fmt.Errorf("write zone file %s: %w", path, err)
		}
		log.Printf("Zone %s: updated (%d records)", zone, len(records))
	}

	// Remove orphaned zone files and generators
	if err := m.removeOrphans(activeZones); err != nil {
		return fmt.Errorf("remove orphans: %w", err)
	}

	return nil
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

func (m *Manager) removeOrphans(activeZones map[string]bool) error {
	// Clean up generators for zones no longer active
	for zone := range m.generators {
		if !activeZones[zone] {
			path := m.zonePath(zone)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove orphan zone file %s: %w", path, err)
			}
			log.Printf("Zone %s: removed (orphaned)", zone)
			delete(m.generators, zone)
		}
	}

	// Also scan the directory for zone files not tracked by any generator
	entries, err := os.ReadDir(m.zoneDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read zone dir: %w", err)
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
				return fmt.Errorf("remove stale zone file %s: %w", path, err)
			}
			log.Printf("Zone %s: removed stale file", zone)
		}
	}

	return nil
}

// RecordsForZone is a convenience type alias.
type RecordsForZone = []netboxclient.IPRecord
