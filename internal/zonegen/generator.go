package zonegen

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// ZoneType distinguishes between forward and reverse DNS zones.
type ZoneType string

const (
	ZoneTypeForward ZoneType = "forward"
	ZoneTypeReverse ZoneType = "reverse"
)

// SOATimers holds the SOA refresh/retry/expire/minimum values in seconds.
// Refresh and retry control how often secondaries poll the primary for
// updates; expire is how long a secondary keeps serving a zone after the
// primary becomes unreachable.
type SOATimers struct {
	Refresh uint32
	Retry   uint32
	Expire  uint32
	Minimum uint32
}

// DefaultSOATimers returns the SOA timers used when none are configured.
func DefaultSOATimers() SOATimers {
	return SOATimers{
		Refresh: 3600,
		Retry:   900,
		Expire:  604800,
		Minimum: 86400,
	}
}

// ZoneConfig holds parameters for zone file generation.
type ZoneConfig struct {
	Origin     string   // e.g., "example.org." or "1.10.in-addr.arpa"
	PrimaryNS  string   // e.g., "ns1.example.org."
	AdminEmail string   // e.g., "admin.example.org."
	TTL        uint32
	Type       ZoneType // forward or reverse
	SOA        SOATimers
}

// Generator produces DNS zone files from Netbox records.
type Generator struct {
	config    ZoneConfig
	lastHash  string
	serial    uint32
}

// NewGenerator creates a new zone file generator.
// If zonePath is provided and exists, it reads the current SOA serial
// from the file to ensure serial continuity across restarts.
func NewGenerator(cfg ZoneConfig, zonePath string) *Generator {
	if cfg.SOA == (SOATimers{}) {
		cfg.SOA = DefaultSOATimers()
	}
	g := &Generator{config: cfg}

	// Try to read existing serial from zone file for continuity
	if zonePath != "" {
		if serial, err := readSerialFromZoneFile(zonePath); err == nil && serial > 0 {
			g.serial = serial
		}
	}

	return g
}

// Generate creates a zone file string from the given records.
// Returns the zone content, whether it changed from the last generation, and any error.
func (g *Generator) Generate(records []netboxclient.IPRecord) (string, bool, error) {
	hash := hashRecords(records)
	if hash == g.lastHash {
		slog.Debug("zone unchanged (hash match)", "zone", g.config.Origin, "hash", hash[:8], "records", len(records))
		return "", false, nil
	}

	if g.lastHash != "" {
		slog.Info("zone changed (hash mismatch)", "zone", g.config.Origin,
			"old_hash", g.lastHash[:8], "new_hash", hash[:8], "records", len(records))
	} else {
		slog.Info("zone initial generation", "zone", g.config.Origin, "hash", hash[:8], "records", len(records))
	}

	g.serial = NextSerial(g.serial)

	var b strings.Builder

	origin := ensureTrailingDot(g.config.Origin)

	// SOA record
	fmt.Fprintf(&b, "$ORIGIN %s\n", origin)
	fmt.Fprintf(&b, "$TTL %d\n", g.config.TTL)
	fmt.Fprintf(&b, "@ IN SOA %s %s (\n", g.config.PrimaryNS, g.config.AdminEmail)
	fmt.Fprintf(&b, "    %d   ; serial\n", g.serial)
	fmt.Fprintf(&b, "    %-9d ; refresh\n", g.config.SOA.Refresh)
	fmt.Fprintf(&b, "    %-9d ; retry\n", g.config.SOA.Retry)
	fmt.Fprintf(&b, "    %-9d ; expire\n", g.config.SOA.Expire)
	fmt.Fprintf(&b, "    %-9d ; minimum\n", g.config.SOA.Minimum)
	fmt.Fprintf(&b, ")\n\n")

	// NS record
	fmt.Fprintf(&b, "@ IN NS %s\n\n", g.config.PrimaryNS)

	// Sort records for deterministic output
	sorted := make([]netboxclient.IPRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return recordLess(sorted[i], sorted[j])
	})

	// Generate records based on zone type
	if g.config.Type == ZoneTypeReverse {
		// PTR records for reverse zones
		for _, r := range sorted {
			// For reverse zones:
			// - r.Address contains the PTR name (e.g., "3.2" or "3.2.1")
			// - r.DNSName contains the target FQDN
			ptrName := r.Address
			if ptrName == "" {
				ptrName = "@"
			}
			target := ensureTrailingDot(r.DNSName)
			fmt.Fprintf(&b, "%s IN PTR %s\n", ptrName, target)
		}
	} else {
		// A and AAAA records for forward zones
		for _, r := range sorted {
			name := shortName(r.DNSName, origin)
			rrType := "A"
			if r.Family == 6 {
				rrType = "AAAA"
			}
			if r.TTL > 0 {
				fmt.Fprintf(&b, "%s %d IN %s %s\n", name, r.TTL, rrType, r.Address)
			} else {
				fmt.Fprintf(&b, "%s IN %s %s\n", name, rrType, r.Address)
			}
		}
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
		_ = os.Remove(tmp)
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

// recordLess orders records by every field so both zone emission and hashing
// see the same deterministic order.
func recordLess(a, b netboxclient.IPRecord) bool {
	if a.DNSName != b.DNSName {
		return a.DNSName < b.DNSName
	}
	if a.Address != b.Address {
		return a.Address < b.Address
	}
	if a.Family != b.Family {
		return a.Family < b.Family
	}
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	if a.CNAMETarget != b.CNAMETarget {
		return a.CNAMETarget < b.CNAMETarget
	}
	if a.DeviceName != b.DeviceName {
		return a.DeviceName < b.DeviceName
	}
	if a.InterfaceName != b.InterfaceName {
		return a.InterfaceName < b.InterfaceName
	}
	if a.VRF != b.VRF {
		return a.VRF < b.VRF
	}
	return a.TTL < b.TTL
}

func hashRecords(records []netboxclient.IPRecord) string {
	sorted := make([]netboxclient.IPRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return recordLess(sorted[i], sorted[j])
	})

	h := sha256.New()
	for _, r := range sorted {
		_, _ = fmt.Fprintf(h, "%s|%s|%d|%d|%s|%s\n", r.DNSName, r.Address, r.Family, r.TTL, r.Type, r.CNAMETarget)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// readSerialFromZoneFile attempts to read the SOA serial from an existing zone file.
// Returns the serial number if found, otherwise returns 0.
// This enables serial continuity across pod restarts even with ephemeral storage.
func readSerialFromZoneFile(path string) (uint32, error) {
	file, err := os.Open(path)
	if err != nil {
		// File doesn't exist yet (first run) - not an error
		return 0, err
	}
	defer func() { _ = file.Close() }()

	// Parse the zone file looking for the SOA serial
	// Format: "@ IN SOA ... (\n    <serial>   ; serial\n"
	scanner := bufio.NewScanner(file)
	inSOA := false
	serialRegex := regexp.MustCompile(`^\s*(\d{10})\s*;\s*serial`)

	for scanner.Scan() {
		line := scanner.Text()

		// Detect SOA record start
		if strings.Contains(line, "IN SOA") {
			inSOA = true
			continue
		}

		// If we're in the SOA section, look for the serial line
		if inSOA {
			if matches := serialRegex.FindStringSubmatch(line); len(matches) == 2 {
				return ParseSerial(matches[1])
			}

			// End of SOA section (closing paren)
			if strings.Contains(line, ")") {
				break
			}
		}
	}

	// Serial not found or parse error
	return 0, fmt.Errorf("serial not found in zone file")
}
