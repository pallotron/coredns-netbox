package zonegen

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// ZoneConfig holds parameters for zone file generation.
type ZoneConfig struct {
	Origin     string // e.g., "example.org."
	PrimaryNS  string // e.g., "ns1.example.org."
	AdminEmail string // e.g., "admin.example.org."
	TTL        uint32
}

// Generator produces DNS zone files from Netbox records.
type Generator struct {
	config    ZoneConfig
	lastHash  string
	serial    uint32
}

// NewGenerator creates a new zone file generator.
func NewGenerator(cfg ZoneConfig) *Generator {
	return &Generator{config: cfg}
}

// Generate creates a zone file string from the given records.
// Returns the zone content, whether it changed from the last generation, and any error.
func (g *Generator) Generate(records []netboxclient.IPRecord) (string, bool, error) {
	hash := hashRecords(records)
	if hash == g.lastHash {
		return "", false, nil
	}

	g.serial = NextSerial(g.serial)

	var b strings.Builder

	origin := ensureTrailingDot(g.config.Origin)

	// SOA record
	fmt.Fprintf(&b, "$ORIGIN %s\n", origin)
	fmt.Fprintf(&b, "$TTL %d\n", g.config.TTL)
	fmt.Fprintf(&b, "@ IN SOA %s %s (\n", g.config.PrimaryNS, g.config.AdminEmail)
	fmt.Fprintf(&b, "    %d   ; serial\n", g.serial)
	fmt.Fprintf(&b, "    3600      ; refresh\n")
	fmt.Fprintf(&b, "    900       ; retry\n")
	fmt.Fprintf(&b, "    604800    ; expire\n")
	fmt.Fprintf(&b, "    86400     ; minimum\n")
	fmt.Fprintf(&b, ")\n\n")

	// NS record
	fmt.Fprintf(&b, "@ IN NS %s\n\n", g.config.PrimaryNS)

	// Sort records for deterministic output
	sorted := make([]netboxclient.IPRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].DNSName != sorted[j].DNSName {
			return sorted[i].DNSName < sorted[j].DNSName
		}
		return sorted[i].Address < sorted[j].Address
	})

	// A and AAAA records
	for _, r := range sorted {
		name := shortName(r.DNSName, origin)
		rrType := "A"
		if r.Family == 6 {
			rrType = "AAAA"
		}
		fmt.Fprintf(&b, "%s IN %s %s\n", name, rrType, r.Address)
	}

	g.lastHash = hash
	return b.String(), true, nil
}

// WriteFile atomically writes the zone content to the specified path.
func WriteFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// shortName extracts the relative name from an FQDN given the zone origin.
// e.g., "host1.example.org" with origin "example.org." returns "host1".
func shortName(fqdn, origin string) string {
	fqdn = ensureTrailingDot(fqdn)
	if fqdn == origin {
		return "@"
	}
	suffix := "." + origin
	if strings.HasSuffix(fqdn, suffix) {
		return strings.TrimSuffix(fqdn, suffix)
	}
	// If the name doesn't belong to this zone, use the FQDN
	return fqdn
}

func ensureTrailingDot(s string) string {
	if !strings.HasSuffix(s, ".") {
		return s + "."
	}
	return s
}

func hashRecords(records []netboxclient.IPRecord) string {
	sorted := make([]netboxclient.IPRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].DNSName != sorted[j].DNSName {
			return sorted[i].DNSName < sorted[j].DNSName
		}
		return sorted[i].Address < sorted[j].Address
	})

	h := sha256.New()
	for _, r := range sorted {
		fmt.Fprintf(h, "%s|%s|%d\n", r.DNSName, r.Address, r.Family)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
