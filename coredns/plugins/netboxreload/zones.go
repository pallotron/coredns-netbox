package netboxreload

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miekg/dns"
)

// zone is immutable after construction by loadZoneFile. ServeDNS reads zone
// fields without holding a lock, relying on reloadZones performing an atomic
// pointer swap rather than in-place mutation.
//
// zone holds all DNS records for a single zone, keyed by lowercased owner name.
type zone struct {
	origin  string              // e.g. "mycompany.com."
	records map[string][]dns.RR // lowercased FQDN -> RRs
}

// parseZoneContent parses zone content from raw bytes. filename must start
// with "db." so the origin can be derived (e.g. "db.mycompany.com" → "mycompany.com.").
func parseZoneContent(filename string, data []byte) (*zone, error) {
	if !strings.HasPrefix(filename, "db.") {
		return nil, fmt.Errorf("unexpected zone filename %q: must start with db.*", filename)
	}
	origin := dns.Fqdn(strings.TrimPrefix(filename, "db."))
	z := &zone{origin: origin, records: make(map[string][]dns.RR)}
	zp := dns.NewZoneParser(bytes.NewReader(data), origin, filename)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		name := strings.ToLower(rr.Header().Name)
		z.records[name] = append(z.records[name], rr)
	}
	if err := zp.Err(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}
	return z, nil
}

// loadZoneFile parses a zone file at path. The zone origin is derived from the
// filename: "db.mycompany.com" → origin "mycompany.com.".
func loadZoneFile(path string) (*zone, error) {
	base := filepath.Base(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseZoneContent(base, data)
}

// loadZoneDir scans dir for files named db.* and loads each as a zone.
// Returns a map from zone origin (e.g. "mycompany.com.") to *zone.
func loadZoneDir(dir string) (map[string]*zone, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read zone dir %s: %w", dir, err)
	}

	zones := make(map[string]*zone)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "db.") {
			continue
		}
		z, err := loadZoneFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		zones[z.origin] = z
	}
	return zones, nil
}
