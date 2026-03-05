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

// ZoneConfig holds parameters for zone file generation.
type ZoneConfig struct {
	Origin     string   // e.g., "example.org." or "1.10.in-addr.arpa"
	PrimaryNS  string   // e.g., "ns1.example.org."
	AdminEmail string   // e.g., "admin.example.org."
	TTL        uint32
	Type       ZoneType // forward or reverse
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
		// Sort by all fields to ensure stable ordering
		if sorted[i].DNSName != sorted[j].DNSName {
			return sorted[i].DNSName < sorted[j].DNSName
		}
		if sorted[i].Address != sorted[j].Address {
			return sorted[i].Address < sorted[j].Address
		}
		if sorted[i].Family != sorted[j].Family {
			return sorted[i].Family < sorted[j].Family
		}
		if sorted[i].DeviceName != sorted[j].DeviceName {
			return sorted[i].DeviceName < sorted[j].DeviceName
		}
		if sorted[i].InterfaceName != sorted[j].InterfaceName {
			return sorted[i].InterfaceName < sorted[j].InterfaceName
		}
		return sorted[i].VRF < sorted[j].VRF
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
			fmt.Fprintf(&b, "%s IN %s %s\n", name, rrType, r.Address)
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

func hashRecords(records []netboxclient.IPRecord) string {
	sorted := make([]netboxclient.IPRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		// Sort by all fields to ensure stable ordering
		if sorted[i].DNSName != sorted[j].DNSName {
			return sorted[i].DNSName < sorted[j].DNSName
		}
		if sorted[i].Address != sorted[j].Address {
			return sorted[i].Address < sorted[j].Address
		}
		if sorted[i].Family != sorted[j].Family {
			return sorted[i].Family < sorted[j].Family
		}
		if sorted[i].DeviceName != sorted[j].DeviceName {
			return sorted[i].DeviceName < sorted[j].DeviceName
		}
		if sorted[i].InterfaceName != sorted[j].InterfaceName {
			return sorted[i].InterfaceName < sorted[j].InterfaceName
		}
		return sorted[i].VRF < sorted[j].VRF
	})

	h := sha256.New()
	for _, r := range sorted {
		_, _ = fmt.Fprintf(h, "%s|%s|%d\n", r.DNSName, r.Address, r.Family)
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
