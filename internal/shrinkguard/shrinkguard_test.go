package shrinkguard

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaults mirrors the shipped policy: refuse a global drop of more than 5%
// when it is also more than 100 records, and a per-zone drop of more than 50%
// under the same absolute floor.
func defaults() Config {
	return Config{
		Enabled:            true,
		GlobalDropFraction: 0.05,
		GlobalMinRecords:   100,
		ZoneDropFraction:   0.5,
		ZoneMinRecords:     100,
	}
}

func TestEvaluateBootstrapNeverRefuses(t *testing.T) {
	// Nothing published yet: a fresh install against an empty NetBox, and a
	// fresh install against a populated one, must both proceed.
	for _, now := range []Baseline{{Total: 0}, {Total: 5000, Zones: map[string]int{"a": 5000}}} {
		v := Evaluate(defaults(), Baseline{}, now)
		assert.False(t, v.Refuse, "bootstrap must publish, got %s", v.Reason())
	}
}

func TestEvaluateEmptyResponseAlwaysRefuses(t *testing.T) {
	prev := Baseline{Total: 20000, Zones: map[string]int{"infra.example.": 20000}}

	v := Evaluate(defaults(), prev, Baseline{})
	require.True(t, v.Refuse, "an empty result must never replace a published set")
	assert.Equal(t, ScopeEmpty, v.Scope, "empty results report their own scope")

	// The empty-set refusal is the already-shipped minimal G1 policy and does
	// not depend on the percentage guard being switched on.
	off := defaults()
	off.Enabled = false
	v = Evaluate(off, prev, Baseline{})
	assert.True(t, v.Refuse, "disabling the shrink guard must not re-enable zone wipes")
	assert.Equal(t, ScopeEmpty, v.Scope)
}

// The restart window: a replacement sidecar has no counts yet but is serving
// the PVC cache. An empty result then would wipe those zones and NOTIFY every
// secondary — and a restart is exactly when an empty result is likely, since
// a bad token or filter arrives with the rollout that restarts the pod.
func TestEvaluateEmptyRefusedWhileServingCachedZones(t *testing.T) {
	restarted := Baseline{ZonesOnDisk: true}

	v := Evaluate(defaults(), restarted, Baseline{})
	require.True(t, v.Refuse, "cached zones must not be wiped by an empty fetch")
	assert.Equal(t, ScopeEmpty, v.Scope)
	assert.Contains(t, v.Reason(), "cached zones", "the reason must say why, with no counts to quote")

	// A genuinely fresh install has nothing to protect and must bootstrap.
	v = Evaluate(defaults(), Baseline{}, Baseline{})
	assert.False(t, v.Refuse, "a fresh install against an empty NetBox still starts")

	// Records arriving after a restart are published normally: the threshold
	// rules stay disarmed until this process has published once.
	v = Evaluate(defaults(), restarted, Baseline{Total: 10, Zones: map[string]int{"a.example.": 10}})
	assert.False(t, v.Refuse, "the first real fetch after a restart must publish")
}

func TestEvaluateGlobalThreshold(t *testing.T) {
	// A large published set. The largest real shrink measured over a month
	// of production was well under 2% of the set — routine churn that must
	// not trip the guard — so the table walks across that band.
	const prevTotal = 20000
	prev := Baseline{Total: prevTotal, Zones: map[string]int{"infra.example.": prevTotal}}

	cases := []struct {
		name    string
		now     int
		refuse  bool
		comment string
	}{
		{"ordinary churn", prevTotal - 250, false, "~1.3%, the shape of a real poll-to-poll change"},
		{"growth", prevTotal + 4000, false, "the set grew"},
		{"unchanged", prevTotal, false, "no drop"},
		{"3% drop", prevTotal - 600, false, "under the 5% trigger"},
		{"exactly 5%", prevTotal - 1000, false, "strictly-greater-than, so 5% itself passes"},
		{"6% drop", prevTotal - 1200, true, "past the trigger"},
		{"half the set", prevTotal / 2, true, "catastrophic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := Baseline{Total: tc.now, Zones: map[string]int{"infra.example.": tc.now}}
			// Keep the per-zone check out of this table: the single zone
			// carries every record, so only the global rule is under test
			// until the drop passes 50%.
			cfg := defaults()
			cfg.ZoneDropFraction = 0.99
			v := Evaluate(cfg, prev, now)
			assert.Equal(t, tc.refuse, v.Refuse, "%s (%s): %s", tc.name, tc.comment, v.Reason())
			if tc.refuse {
				assert.Equal(t, ScopeGlobal, v.Scope)
			}
		})
	}
}

func TestEvaluateAbsoluteFloorSuppressesSmallSetNoise(t *testing.T) {
	// A dev or k3d deployment holding a handful of records loses a large
	// percentage on every ordinary change. The absolute floor is what keeps
	// the guard quiet there; it is a noise gate, not the trigger.
	prev := Baseline{Total: 8, Zones: map[string]int{"dev.example.": 8}}
	now := Baseline{Total: 4, Zones: map[string]int{"dev.example.": 4}}

	v := Evaluate(defaults(), prev, now)
	assert.False(t, v.Refuse, "50%% of 8 records is below the absolute floor: %s", v.Reason())
}

func TestEvaluatePerZoneThreshold(t *testing.T) {
	// One site's records vanish while the total barely moves — the site is
	// small enough that the global rule cannot see it, which is exactly the
	// case the per-zone rule exists for.
	prev := Baseline{
		Total: 40000,
		Zones: map[string]int{
			"site-a.example.": 38500,
			"site-b.example.": 1500,
		},
	}
	now := Baseline{
		Total: 38500,
		Zones: map[string]int{
			"site-a.example.": 38500,
		},
	}

	global := Evaluate(Config{Enabled: true, GlobalDropFraction: 0.05, GlobalMinRecords: 100}, prev, now)
	require.False(t, global.Refuse, "precondition: the global rule alone cannot see a single site disappear")

	v := Evaluate(defaults(), prev, now)
	require.True(t, v.Refuse, "a zone losing every record must be refused")
	assert.Equal(t, ScopeZone, v.Scope)
	assert.Equal(t, "site-b.example.", v.Zone)
	assert.Equal(t, 1500, v.Dropped)
}

func TestEvaluatePerZoneReportsLargestDrop(t *testing.T) {
	// The big zone is untouched, so the 6000 lost records are under the
	// global 5% trigger and only the per-zone rule fires.
	prev := Baseline{
		Total: 158000,
		Zones: map[string]int{"a.example.": 3000, "b.example.": 5000, "c.example.": 150000},
	}
	now := Baseline{
		Total: 152000,
		Zones: map[string]int{"a.example.": 1000, "b.example.": 1000, "c.example.": 150000},
	}

	v := Evaluate(defaults(), prev, now)
	require.True(t, v.Refuse)
	// Both a and b trip; the operator is told about the biggest loser so the
	// log line points at the most likely cause.
	assert.Equal(t, "b.example.", v.Zone)
	assert.Equal(t, 4000, v.Dropped)
}

func TestEvaluateNewZonesAndRenamesDoNotTrip(t *testing.T) {
	prev := Baseline{Total: 1000, Zones: map[string]int{"a.example.": 1000}}
	now := Baseline{Total: 1600, Zones: map[string]int{"a.example.": 1000, "new.example.": 600}}

	v := Evaluate(defaults(), prev, now)
	assert.False(t, v.Refuse, "a new zone is growth, not shrink: %s", v.Reason())
}

func TestEvaluateDisabledSkipsThresholds(t *testing.T) {
	prev := Baseline{Total: 20000, Zones: map[string]int{"a.example.": 20000}}
	now := Baseline{Total: 1000, Zones: map[string]int{"a.example.": 1000}}

	cfg := defaults()
	cfg.Enabled = false
	v := Evaluate(cfg, prev, now)
	assert.False(t, v.Refuse, "an operator who switched the guard off gets no thresholds")
}

// A half-filled Config must fail safe. Zero is the Go zero value, so reading
// it as "refuse on any loss at all" would turn a forgotten field into a
// sidecar that refuses every poll.
func TestEvaluateZeroFractionDisablesItsRule(t *testing.T) {
	prev := Baseline{Total: 10000, Zones: map[string]int{"a.example.": 10000}}
	now := Baseline{Total: 9999, Zones: map[string]int{"a.example.": 9999}}

	v := Evaluate(Config{Enabled: true}, prev, now)
	assert.False(t, v.Refuse, "an unconfigured threshold must not refuse: %s", v.Reason())

	// The global rule still works when only it is configured.
	v = Evaluate(Config{Enabled: true, GlobalDropFraction: 0.05, GlobalMinRecords: 100},
		prev, Baseline{Total: 5000, Zones: map[string]int{"a.example.": 5000}})
	assert.True(t, v.Refuse)
	assert.Equal(t, ScopeGlobal, v.Scope)
}

func TestVerdictReasonNamesTheNumbers(t *testing.T) {
	prev := Baseline{Total: 20000, Zones: map[string]int{"a.example.": 20000}}
	now := Baseline{Total: 9000, Zones: map[string]int{"a.example.": 9000}}

	v := Evaluate(defaults(), prev, now)
	require.True(t, v.Refuse)
	reason := v.Reason()
	assert.Contains(t, reason, "20000")
	assert.Contains(t, reason, "9000")
	assert.Contains(t, reason, "%", "the reason states the drop as a percentage")
}

func TestOverrideIsSingleUse(t *testing.T) {
	var o Override

	assert.False(t, o.ConsumeIfArmed(), "an unarmed override must not suppress anything")

	o.Arm()
	assert.True(t, o.Armed(), "arming is visible without consuming")
	assert.True(t, o.ConsumeIfArmed(), "the armed override suppresses one refusal")
	assert.False(t, o.ConsumeIfArmed(), "and only one — the next cycle is guarded again")
	assert.False(t, o.Armed())
}

func TestCountZonesIsNilSafe(t *testing.T) {
	assert.Equal(t, Baseline{Total: 0, Zones: map[string]int{}}, FromZoneCounts(nil))

	b := FromZoneCounts(map[string]int{"a.": 3, "b.": 4})
	assert.Equal(t, 7, b.Total)
	assert.Equal(t, map[string]int{"a.": 3, "b.": 4}, b.Zones)
}
