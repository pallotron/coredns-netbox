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

// answer returns the RRs answering a qtype query at name, following CNAME
// chains within the zone (RFC 1034 §4.3.2): when the name owns a CNAME and
// the query is for another type, the CNAME is included and resolution
// restarts at its target. Chains ending outside the zone, or at names with
// no matching data, return the partial chain (the client follows up).
// A visited set guards against CNAME loops in hand-edited zone files.
func (z *zone) answer(name string, qtype uint16) []dns.RR {
	var out []dns.RR
	visited := make(map[string]bool)
	for cur := name; !visited[cur]; {
		visited[cur] = true
		var cname *dns.CNAME
		matched := false
		for _, rr := range z.records[cur] {
			if rr.Header().Rrtype == qtype || qtype == dns.TypeANY {
				out = append(out, dns.Copy(rr))
				matched = true
			} else if c, ok := rr.(*dns.CNAME); ok {
				cname = c
			}
		}
		if matched || cname == nil || qtype == dns.TypeCNAME || qtype == dns.TypeANY {
			break
		}
		out = append(out, dns.Copy(cname))
		cur = strings.ToLower(dns.Fqdn(cname.Target))
	}
	return out
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

// cachedZone pairs a parsed zone with the validator that identified the
// source content it was parsed from: the Last-Modified header for HTTP
// fetches, mtime+size for directory loads. When the validator still matches
// on the next reload, the parsed zone is reused instead of re-parsed —
// safe because zones are immutable after construction.
type cachedZone struct {
	validator string
	z         *zone
}

// loadZoneDir scans dir for files named db.* and loads each as a zone.
// Files whose mtime+size match an entry in prev (keyed by filename) are
// reused without re-reading or re-parsing. The sidecar only rewrites a zone
// file when its content hash changes, so mtime+size tracks content.
// Returns the zones keyed by origin (e.g. "mycompany.com.") plus the cache
// to pass as prev on the next load.
func loadZoneDir(dir string, prev map[string]cachedZone) (map[string]*zone, map[string]cachedZone, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read zone dir %s: %w", dir, err)
	}

	zones := make(map[string]*zone)
	cache := make(map[string]cachedZone)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "db.") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, nil, fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		validator := fmt.Sprintf("%d|%d", info.ModTime().UnixNano(), info.Size())
		if c, ok := prev[e.Name()]; ok && c.validator == validator {
			zones[c.z.origin] = c.z
			cache[e.Name()] = c
			continue
		}
		z, err := loadZoneFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		zones[z.origin] = z
		cache[e.Name()] = cachedZone{validator: validator, z: z}
	}
	return zones, cache, nil
}
