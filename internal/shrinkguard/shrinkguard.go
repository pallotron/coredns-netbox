// Package shrinkguard decides whether a successful NetBox fetch may replace
// the currently published record set, or whether the loss it represents is
// too large to be believed without an operator saying so (matrix row N21,
// product gap G1).
//
// What this guard does NOT have to catch: a truncated or half-failed fetch.
// netboxclient.fetchIPAddressesOnce aborts the whole fetch when any page
// errors, so a partial result never reaches zone generation — the serve-stale
// path handles it. What remains are the cases where NetBox successfully
// returns fewer records: an empty-but-HTTP-200 response, a token or filter
// whose scope narrowed, a bulk status change, a bad import that wiped a
// tenant's addresses. Those are large and coherent, which is why a
// percentage is the trigger here.
//
// Thresholds are a percentage with an absolute floor, and the floor is a
// noise gate rather than the trigger: churn scales with the fleet, so a fixed
// record count tuned for today starts firing on routine churn as the fleet
// grows and gets switched off, while a percentage needs no maintenance. The
// floor exists because percentages are meaningless on a handful of records
// (a 4-record dev zone loses half of itself when one host goes away).
package shrinkguard

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// Scope names which rule refused a publish.
const (
	// ScopeEmpty is an empty result replacing a published set. It is refused
	// unconditionally — the minimal G1 policy predates the thresholds below
	// and does not depend on them being enabled.
	ScopeEmpty = "empty"
	// ScopeGlobal is the whole record set shrinking: the NetBox-wide
	// failures (scope narrowed, bulk status change, mass deletion).
	ScopeGlobal = "global"
	// ScopeZone is one zone collapsing while the total barely moves: one
	// site's records vanishing, which the global rule cannot see.
	ScopeZone = "zone"
)

// Config is the shrink-guard policy. Zero values disable the thresholds they
// belong to; see Evaluate for how they combine.
type Config struct {
	// Enabled turns the percentage thresholds on. The empty-result refusal
	// is unconditional and ignores this field.
	Enabled bool
	// GlobalDropFraction is the share of the previous total that must be
	// lost before the global rule refuses (0.05 = more than 5%).
	GlobalDropFraction float64
	// GlobalMinRecords is the absolute noise gate for the global rule: a
	// drop no larger than this never refuses, whatever the percentage.
	GlobalMinRecords int
	// ZoneDropFraction and ZoneMinRecords are the same pair applied to each
	// zone individually.
	ZoneDropFraction float64
	ZoneMinRecords   int
}

// Baseline is a published (or candidate) record set, counted globally and per
// zone. The per-zone map is what lets a single site's disappearance be seen.
type Baseline struct {
	Total int
	Zones map[string]int

	// ZonesOnDisk marks a baseline with no counts behind it but zones being
	// served all the same: the PVC cache a restarted sidecar picks up before
	// its first successful fetch. Counts cannot be recovered from rendered
	// zone files in the same units as discovery produces, so the threshold
	// rules stay disarmed until this process publishes once — but an empty
	// result must still be refused, because a restart is exactly when one is
	// likely (a token or filter change lands with the rollout that restarts
	// the pod, and its first fetch legitimately returns nothing).
	ZonesOnDisk bool
}

// FromZoneCounts builds a Baseline from per-zone record counts, summing the
// total so the two can never disagree.
func FromZoneCounts(zones map[string]int) Baseline {
	b := Baseline{Zones: make(map[string]int, len(zones))}
	for zone, n := range zones {
		b.Zones[zone] = n
		b.Total += n
	}
	return b
}

// Verdict is the guard's decision, carrying enough detail for a log line and
// a metric label.
type Verdict struct {
	Refuse bool
	Scope  string
	// Zone is set when Scope is ScopeZone.
	Zone string
	// Prev and Now are the counts that produced the verdict: the totals for
	// a global refusal, the zone's own counts for a per-zone one.
	Prev, Now    int
	Dropped      int
	DropFraction float64
}

// Reason renders the verdict for an operator.
func (v Verdict) Reason() string {
	switch v.Scope {
	case ScopeEmpty:
		if v.Prev == 0 {
			return "netbox returned 0 records but cached zones from a previous run are being served"
		}
		return fmt.Sprintf("netbox returned 0 records but %d were previously published", v.Prev)
	case ScopeZone:
		return fmt.Sprintf("zone %s shrank from %d to %d records (-%d, %.1f%%)",
			v.Zone, v.Prev, v.Now, v.Dropped, v.DropFraction*100)
	case ScopeGlobal:
		return fmt.Sprintf("record set shrank from %d to %d (-%d, %.1f%%)",
			v.Prev, v.Now, v.Dropped, v.DropFraction*100)
	default:
		return ""
	}
}

// EmptyResult is the unconditional rule: an empty-but-HTTP-200 NetBox result
// never replaces a published set. It is separate from Evaluate so the poll
// path can refuse before spending enrichment and zone discovery on a set it
// will not publish, and because it asks only whether each side is zero — the
// counts may be in different units (raw NetBox records vs rendered zone
// records) without changing the answer.
//
// Deliberately not overridable: "NetBox returned nothing" is never a state
// this system should publish on purpose. Removing every record is done by
// removing the deployment, not by forcing an empty zone set through.
// It also covers the restart window: prev carries no counts until this
// process publishes once, but Baseline.ZonesOnDisk says zones are being
// served from the PVC cache, and those must not be wiped either.
func EmptyResult(prev Baseline, nowTotal int) Verdict {
	if nowTotal > 0 {
		return Verdict{}
	}
	if prev.Total > 0 {
		return Verdict{Refuse: true, Scope: ScopeEmpty, Prev: prev.Total, Now: 0,
			Dropped: prev.Total, DropFraction: 1}
	}
	if prev.ZonesOnDisk {
		// No counts to quote: the zones predate this process.
		return Verdict{Refuse: true, Scope: ScopeEmpty}
	}
	return Verdict{}
}

// Evaluate reports whether the candidate set may be published over prev.
//
// Order matters: the unconditional empty check first, then the global rule,
// then the per-zone rule. A refusal anywhere refuses the whole cycle — the
// zones stay exactly as they are — because publishing some zones while
// holding others back would leave the served data in a state no NetBox fetch
// ever described.
func Evaluate(cfg Config, prev, now Baseline) Verdict {
	if v := EmptyResult(prev, now.Total); v.Refuse {
		return v
	}

	// Nothing published yet: a fresh install must bootstrap, including
	// against an empty NetBox.
	if prev.Total == 0 {
		return Verdict{}
	}

	if !cfg.Enabled {
		return Verdict{}
	}

	if dropped := prev.Total - now.Total; trips(dropped, prev.Total, cfg.GlobalDropFraction, cfg.GlobalMinRecords) {
		return Verdict{Refuse: true, Scope: ScopeGlobal, Prev: prev.Total, Now: now.Total,
			Dropped: dropped, DropFraction: float64(dropped) / float64(prev.Total)}
	}

	// Sorted so that an equal-sized drop in two zones always names the same
	// one; the largest drop wins so the log points at the likeliest cause.
	zones := make([]string, 0, len(prev.Zones))
	for zone := range prev.Zones {
		zones = append(zones, zone)
	}
	sort.Strings(zones)

	worst := Verdict{}
	for _, zone := range zones {
		was := prev.Zones[zone]
		is := now.Zones[zone] // absent means the zone lost every record
		dropped := was - is
		if !trips(dropped, was, cfg.ZoneDropFraction, cfg.ZoneMinRecords) {
			continue
		}
		if dropped > worst.Dropped {
			worst = Verdict{Refuse: true, Scope: ScopeZone, Zone: zone, Prev: was, Now: is,
				Dropped: dropped, DropFraction: float64(dropped) / float64(was)}
		}
	}
	return worst
}

// trips reports whether a loss of dropped records out of was is both a large
// enough share and more than the absolute floor. Both comparisons are
// strictly-greater-than, so a threshold of exactly 5% lets a 5% drop through.
//
// A non-positive fraction switches the rule off rather than making it
// maximally sensitive: zero-valued config must fail safe, or a half-filled
// Config would refuse every poll that lost a single record.
func trips(dropped, was int, fraction float64, minRecords int) bool {
	if fraction <= 0 {
		return false
	}
	if dropped <= 0 || was <= 0 {
		return false
	}
	if dropped <= minRecords {
		return false
	}
	return float64(dropped)/float64(was) > fraction
}

// Override is a single-use operator consent to publish one shrinking set. It
// is armed out of band (the ForceNetboxPoll RPC) and consumed by the first
// refusal it meets, so consent never outlives the change it was given for.
type Override struct {
	armed atomic.Bool
}

// Arm records operator consent for the next refused publish.
func (o *Override) Arm() { o.armed.Store(true) }

// Armed reports whether consent is waiting, without consuming it.
func (o *Override) Armed() bool { return o.armed.Load() }

// ConsumeIfArmed takes the consent if there is any, returning whether the
// caller may proceed past a refusal.
func (o *Override) ConsumeIfArmed() bool { return o.armed.Swap(false) }
