package netboxreload

import (
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

// loadZoneFile parses a zone file at path. The zone origin is derived from the
// filename: "db.mycompany.com" → origin "mycompany.com.".
func loadZoneFile(path string) (*zone, error) {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "db.") {
		return nil, fmt.Errorf("unexpected zone filename %q: must start with db.*", base)
	}
	origin := dns.Fqdn(strings.TrimPrefix(base, "db."))

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	z := &zone{
		origin:  origin,
		records: make(map[string][]dns.RR),
	}

	zp := dns.NewZoneParser(f, origin, path)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		name := strings.ToLower(rr.Header().Name)
		z.records[name] = append(z.records[name], rr)
	}
	if err := zp.Err(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return z, nil
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
