package zonegen

import (
	"log/slog"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// resolveCNAMECollisions enforces RFC 1034 §3.6.2 for a zone's record set:
// a name may hold at most one CNAME, and a CNAME may not coexist with other
// data. The zone parser does not validate this, so it must happen here.
//
// Policy:
//   - CNAME with an empty target, or at the zone apex (where SOA/NS live):
//     dropped — both would corrupt the zone.
//   - CNAME colliding with address data: drop the CNAME, keep the addresses.
//   - Multiple CNAMEs at one name with different targets: drop ALL of them —
//     picking a winner by sort order would silently alias to an arbitrary
//     device.
//   - Exact duplicates (same name, same target): dedupe to one.
//
// Every drop is logged at ERROR. Input order is preserved. zone (the origin)
// is used for apex detection and logging.
func resolveCNAMECollisions(records []netboxclient.IPRecord, zone string) []netboxclient.IPRecord {
	// key normalizes owner names for comparison; dns_name pass-throughs may
	// carry mixed case while generated names are lowercase.
	key := func(s string) string {
		return strings.ToLower(strings.TrimSuffix(s, "."))
	}

	hasData := make(map[string]bool)
	targets := make(map[string]map[string]bool) // owner -> set of distinct targets
	devices := make(map[string][]string)        // owner -> devices claiming a CNAME there
	for _, r := range records {
		n := key(r.DNSName)
		if r.Type == netboxclient.RecordTypeCNAME {
			if targets[n] == nil {
				targets[n] = make(map[string]bool)
			}
			targets[n][key(r.CNAMETarget)] = true
			devices[n] = append(devices[n], r.DeviceName)
		} else {
			hasData[n] = true
		}
	}

	var out []netboxclient.IPRecord
	emitted := make(map[string]bool)
	for _, r := range records {
		if r.Type != netboxclient.RecordTypeCNAME {
			out = append(out, r)
			continue
		}
		n := key(r.DNSName)
		switch {
		case r.CNAMETarget == "":
			slog.Error("dropping CNAME with empty target",
				"zone", zone, "name", r.DNSName, "device", r.DeviceName)
		case n == key(zone):
			slog.Error("dropping CNAME at zone apex: it would coexist with SOA/NS (RFC 1034 §3.6.2)",
				"zone", zone, "name", r.DNSName, "target", r.CNAMETarget, "device", r.DeviceName)
		case hasData[n]:
			slog.Error("dropping CNAME: name also holds address data (RFC 1034 §3.6.2)",
				"zone", zone, "name", r.DNSName, "target", r.CNAMETarget, "device", r.DeviceName)
		case len(targets[n]) > 1:
			slog.Error("dropping ambiguous CNAMEs: multiple targets render the same alias",
				"zone", zone, "name", r.DNSName, "target", r.CNAMETarget, "devices", devices[n])
		case emitted[n]:
			// exact duplicate — already emitted
		default:
			emitted[n] = true
			out = append(out, r)
		}
	}
	return out
}
