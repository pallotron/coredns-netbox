package netboxreload

import (
	"log/slog"
	"sort"

	"github.com/miekg/dns"
)

// Sending NOTIFY (RFC 1996) is what turns a secondary's propagation delay
// from "up to one SOA refresh" into "about as long as a UDP round trip plus
// an AXFR". Upstream CoreDNS does this in the zone-owning plugin —
// plugin/file/reload.go calls transfer.Notify after a zone file changes —
// and this plugin owns the zones instead, so it has to do the same.
//
// Two deployment facts decide whether a NOTIFY actually lands, and neither
// is visible from this code (see docs/deployment.md):
//
//   - The transfer plugin skips wildcard targets, so `transfer.to: ['*']`
//     sends no NOTIFY at all. Concrete peer addresses are required.
//   - A secondary accepts a NOTIFY only from an address in its own
//     `transfer from` list, compared against the packet's source IP. A
//     primary behind a Service sends from its pod IP, not the Service VIP,
//     so the secondary must list the address the primary actually sends
//     from or it will log "Dropping notify" and wait for its refresh timer.
//
// Neither failure loses data: NOTIFY is a latency optimisation, and SOA
// refresh remains the guarantee. That is why every failure here is logged
// and counted rather than retried or escalated.

// serial returns the zone's SOA serial. A zone with no SOA at its apex
// cannot be transferred at all (Transfer bails the same way), so it reports
// not-ok and is left out of notifications.
func (z *zone) serial() (uint32, bool) {
	for _, rr := range z.records[z.origin] {
		if soa, ok := rr.(*dns.SOA); ok {
			return soa.Serial, true
		}
	}
	return 0, false
}

// serialChangedZones returns the origins whose SOA serial differs between
// the previously served zones and the newly loaded ones, including zones
// that appeared. Sorted for deterministic logs and tests.
//
// The serial is the right trigger rather than "the file changed": a reload
// can swap in re-rendered bytes with the same serial (a restart replays the
// same zone), and a NOTIFY for an unchanged serial just makes every
// secondary do a pointless SOA check.
func serialChangedZones(old, current map[string]*zone) []string {
	var changed []string
	for origin, z := range current {
		now, ok := z.serial()
		if !ok {
			continue
		}
		prev, existed := old[origin]
		if !existed {
			changed = append(changed, origin)
			continue
		}
		before, ok := prev.serial()
		if !ok || before != now {
			changed = append(changed, origin)
		}
	}
	sort.Strings(changed)
	return changed
}

// sendNotifies notifies the configured transfer targets about each origin.
// Called from a goroutine: a NOTIFY waits for each peer to answer, and a
// slow or unreachable secondary must not hold up the reload that triggered
// it.
func (p *Plugin) sendNotifies(origins []string) {
	if p.transfer == nil {
		// No transfer block in the Corefile: nothing is allowed to
		// transfer this zone, so there is nobody to notify.
		return
	}
	for _, origin := range origins {
		if err := p.transfer.Notify(origin); err != nil {
			slog.Warn("netboxreload: notify failed; secondaries will converge on their SOA refresh instead",
				"zone", origin, "err", err)
			notifyTotal.WithLabelValues("error").Inc()
			continue
		}
		notifyTotal.WithLabelValues("success").Inc()
	}
}
