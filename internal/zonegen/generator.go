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
	Origin     string // e.g., "example.org." or "1.10.in-addr.arpa"
	PrimaryNS  string // e.g., "ns1.example.org."
	AdminEmail string // e.g., "admin.example.org."
	TTL        uint32
	Type       ZoneType // forward or reverse
	SOA        SOATimers
	// PrimaryNSIP, when set, is the address served for the PrimaryNS name in
	// the forward zone that contains it — without it the name referenced by
	// every SOA/NS record never resolves (it has no NetBox object).
	PrimaryNSIP string
}

// Generator produces DNS zone files from Netbox records.
type Generator struct {
	config   ZoneConfig
	lastHash string
	serial   uint32
	// hashFromFile marks lastHash as read from a zone file rather than
	// written by this process, which is the only case where the file's
	// integrity has to be taken on trust.
	hashFromFile bool
}

// NewGenerator creates a new zone file generator.
// If zonePath is provided and exists, it reads the current SOA serial AND
// the persisted content hash from the file: the serial for continuity, the
// hash so an unchanged record set is recognized across restarts (G5 — a
// fresh process used to start with an empty hash, rewrite every zone with
// serial+1, and force a full AXFR re-transfer to every secondary).
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
		// Trust the persisted record fingerprint only if the file still
		// hashes to its own recorded body fingerprint. A damaged file
		// (truncated write, bad restore, hand edit) must be rewritten, not
		// skipped — otherwise the damage survives every restart.
		if hash, err := readVerifiedContentHash(zonePath); err == nil && hash != "" {
			g.lastHash = hash
			g.hashFromFile = true
		}
	}

	return g
}

// safeZoneNameRE accepts names that can be emitted into a zone file
// without altering its syntax: dot-separated labels of letters, digits,
// underscore and interior hyphens, optionally dot-terminated. Anything
// else — whitespace, quotes, semicolons, parentheses, control characters,
// and above all newlines — is zone-file metacharacter territory. The
// case-insensitive sibling of nameformat's renderedNameRE.
var safeZoneNameRE = regexp.MustCompile(`^[a-zA-Z0-9_]([a-zA-Z0-9_-]*[a-zA-Z0-9_])?(\.[a-zA-Z0-9_]([a-zA-Z0-9_-]*[a-zA-Z0-9_])?)*\.?$`)

// safeZoneName reports whether name is safe to write into a zone file and
// parseable by the file plugin (label and total length limits included —
// an over-long label is rejected by the zone parser just like a syntax
// error, with the same blast radius).
func safeZoneName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	if !safeZoneNameRE.MatchString(name) {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) > 63 {
			return false
		}
	}
	return true
}

// dropUnsafeNames filters out records whose DNS name (or CNAME target)
// cannot be emitted safely — G8 in the acceptance matrix: NetBox's API
// validates dns_name, but ORM writes (plugins, custom scripts, imports)
// bypass it, and before this guard a single hostile value reached the
// rendered zone, CoreDNS refused to parse the file, and EVERY zone update
// froze until the row was deleted (live finding: a dns_name with an
// embedded space wedged db.10.in-addr.arpa via its PTR target). Offenders
// are dropped loudly; the rest of the zone proceeds. Filtering happens
// before hashing, so a dropped record never causes a spurious rewrite.
func dropUnsafeNames(records []netboxclient.IPRecord, zone string) []netboxclient.IPRecord {
	kept := records
	copied := false
	for i, r := range records {
		bad := !safeZoneName(r.DNSName)
		if !bad && r.Type == netboxclient.RecordTypeCNAME && !safeZoneName(r.CNAMETarget) {
			bad = true
		}
		if !bad {
			if copied {
				kept = append(kept, r)
			}
			continue
		}
		slog.Error("dropping record: dns_name unsafe for zone file emission",
			"zone", zone, "dns_name", r.DNSName, "address", r.Address, "cname_target", r.CNAMETarget)
		if !copied {
			kept = append([]netboxclient.IPRecord(nil), records[:i]...)
			copied = true
		}
	}
	return kept
}

// Generate creates a zone file string from the given records.
// Returns the zone content, whether it changed from the last generation, the
// number of records in the zone after collision filtering, and any error.
func (g *Generator) Generate(records []netboxclient.IPRecord) (string, bool, int, error) {
	records = dropUnsafeNames(records, g.config.Origin)
	if g.config.Type == ZoneTypeForward {
		records = resolveCNAMECollisions(records, g.config.Origin)
	}
	hash := hashRecords(records)
	if hash == g.lastHash {
		slog.Debug("zone unchanged (hash match)", "zone", g.config.Origin, "hash", hash[:8], "records", len(records))
		return "", false, len(records), nil
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

	// G3: PrimaryNS is named in every zone's SOA/NS but lives in exactly one
	// zone — serve its address there so the name actually resolves. A
	// NetBox-managed record for the same name takes precedence.
	if g.config.Type != ZoneTypeReverse && g.config.PrimaryNSIP != "" {
		g.writePrimaryNSAddress(&b, records, origin)
	}

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
		// A, AAAA and CNAME records for forward zones
		for _, r := range sorted {
			name := shortName(r.DNSName, origin)
			var rrType, value string
			if r.Type == netboxclient.RecordTypeCNAME {
				rrType = "CNAME"
				value = ensureTrailingDot(r.CNAMETarget)
			} else {
				rrType = "A"
				if r.Family == 6 {
					rrType = "AAAA"
				}
				value = r.Address
			}
			if r.TTL > 0 {
				fmt.Fprintf(&b, "%s %d IN %s %s\n", name, r.TTL, rrType, value)
			} else {
				fmt.Fprintf(&b, "%s IN %s %s\n", name, rrType, value)
			}
		}
	}

	g.lastHash = hash
	g.hashFromFile = false
	return withFingerprints(b.String(), hash), true, len(records), nil
}

// withFingerprints inserts the two fingerprint comments after the $TTL line:
//
//	; content-hash: <hash of the record set this file represents>
//	; body-hash:    <hash of this file's own bytes, minus these two lines>
//
// content-hash answers "have the records changed?" across a restart (G5 — a
// fresh process used to rewrite every zone with serial+1 and force a full
// AXFR re-transfer to every secondary). body-hash answers "is this file
// still what we wrote?", which content-hash alone cannot: a tail-truncated
// file keeps its header, so without this the restarted process would trust
// the stale content-hash, skip the repair, and leave CoreDNS serving a
// silently incomplete zone (it parses — there is simply less of it).
// Comments are ignored by every zone parser.
func withFingerprints(content, contentHash string) string {
	lines := strings.SplitAfter(content, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "$TTL ") {
			continue
		}
		plain := strings.Join(lines[:i+1], "") + strings.Join(lines[i+1:], "")
		stamped := strings.Join(lines[:i+1], "") +
			fmt.Sprintf("; content-hash: %s\n; body-hash: %s\n", contentHash, hashBody(plain)) +
			strings.Join(lines[i+1:], "")
		return stamped
	}
	return content
}

// hashBody fingerprints zone content with the fingerprint comments removed,
// so the value is stable across writing and reading the same file back.
func hashBody(content string) string {
	h := sha256.New()
	for _, line := range strings.SplitAfter(content, "\n") {
		if contentHashRE.MatchString(strings.TrimSuffix(line, "\n")) ||
			bodyHashRE.MatchString(strings.TrimSuffix(line, "\n")) {
			continue
		}
		_, _ = h.Write([]byte(line))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
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

// writePrimaryNSAddress emits an address record for the PrimaryNS name when
// it belongs to this zone. Skipped when the name is out of zone or when a
// NetBox record already defines it (the operator-managed address wins).
func (g *Generator) writePrimaryNSAddress(b *strings.Builder, records []netboxclient.IPRecord, origin string) {
	nsName := strings.TrimSuffix(g.config.PrimaryNS, ".")
	short := shortName(nsName, origin)
	if strings.HasSuffix(short, ".") {
		return // out of zone: shortName returns the FQDN unchanged, dot-terminated
	}
	for _, r := range records {
		if strings.TrimSuffix(r.DNSName, ".") == nsName {
			return
		}
	}
	rtype := "A"
	if strings.Contains(g.config.PrimaryNSIP, ":") {
		rtype = "AAAA"
	}
	fmt.Fprintf(b, "%s IN %s %s\n\n", short, rtype, g.config.PrimaryNSIP)
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

// hashRecords fingerprints the record set for change detection. The hashed
// fields must stay in sync with recordLess: a field that affects zone output
// but not the hash would change zones without bumping the SOA serial.
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

// contentHashRE and bodyHashRE match the fingerprint comments that
// withFingerprints writes near the top of every generated zone file.
var (
	contentHashRE = regexp.MustCompile(`^; content-hash: ([0-9a-f]{64})$`)
	bodyHashRE    = regexp.MustCompile(`^; body-hash: ([0-9a-f]{64})$`)
)

// readVerifiedContentHash returns the persisted record-set fingerprint of a
// zone file, but only when the file still matches its own body fingerprint.
// Returns "" for a file with no fingerprints (pre-fix files, hand-written
// zones) and for a file whose body no longer matches — in both cases the
// caller must regenerate rather than trust what is on disk.
func readVerifiedContentHash(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(raw)

	var contentHash, bodyHash string
	for _, line := range strings.Split(content, "\n") {
		if m := contentHashRE.FindStringSubmatch(line); len(m) == 2 {
			contentHash = m[1]
		}
		if m := bodyHashRE.FindStringSubmatch(line); len(m) == 2 {
			bodyHash = m[1]
		}
		if contentHash != "" && bodyHash != "" {
			break
		}
	}
	if contentHash == "" || bodyHash == "" {
		return "", nil
	}
	if got := hashBody(content); got != bodyHash {
		slog.Warn("zone file does not match its recorded fingerprint; regenerating",
			"path", path, "recorded", bodyHash[:8], "actual", got[:8])
		return "", nil
	}
	return contentHash, nil
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
