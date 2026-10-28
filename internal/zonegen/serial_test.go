package zonegen

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextSerialIsWallClockSeconds(t *testing.T) {
	before := uint32(time.Now().UTC().Unix())
	s := NextSerial(0)
	after := uint32(time.Now().UTC().Unix())

	assert.GreaterOrEqual(t, s, before, "serial must be at least the time the call started")
	assert.LessOrEqual(t, s, after, "serial must not run ahead of the clock from a cold start")
}

// The reason wall-clock seconds cannot be used raw. Two writes inside one
// second would otherwise produce the same serial, and a secondary already
// holding it sees no change and never transfers — the zone stays correct on
// the primary and stale everywhere else, with no error recorded on either
// side. Same shape as G14, moved into the AXFR path.
//
// The webhook fast path writes the instant an event arrives, so a burst
// inside a single second is the normal case, not an edge case.
func TestNextSerialSameSecondWritesStayDistinct(t *testing.T) {
	seen := map[uint32]bool{}
	s := NextSerial(0)
	seen[s] = true

	// Tight loop: every iteration lands in the same second or the next one.
	for i := 0; i < 500; i++ {
		next := NextSerial(s)
		require.Greater(t, next, s, "serial regressed on write %d", i+2)
		require.False(t, seen[next], "serial %d reused on write %d", next, i+2)
		seen[next] = true
		s = next
	}
}

// N10 (acceptance matrix): strictly monotonic at any realistic change rate.
// Under YYYYMMDDNN this was the G2 bug — the 100th same-day write wrapped
// back to todayBase+1 and froze every AXFR secondary until the next UTC day.
// The scheme no longer has a per-day ceiling, so the pin is simply that no
// burst size regresses.
func TestNextSerialMonotonicAcrossBurst(t *testing.T) {
	s := NextSerial(0)
	for i := 0; i < 150; i++ {
		next := NextSerial(s)
		require.Greater(t, next, s, "serial regressed on write %d", i+2)
		s = next
	}
}

// Adopting this scheme has to step the serial backwards once: a 2026
// YYYYMMDDNN serial is ~2.03e9 against ~1.79e9 of wall-clock time. Without
// this branch the monotonic guard would carry the legacy value forward one
// increment at a time and never reach wall-clock seconds — the new scheme
// would appear to deploy and change nothing, which is exactly the silent
// no-op G15 was about.
func TestNextSerialAdoptsWallClockFromLegacySerial(t *testing.T) {
	const legacy = 2026102801 // YYYYMMDDNN for 2026-10-28, write 01
	now := uint32(time.Now().UTC().Unix())
	require.Less(t, now, uint32(legacy), "precondition: wall clock is still behind legacy serials")

	adopted := NextSerial(legacy)
	assert.Less(t, adopted, uint32(legacy), "the legacy serial must be dropped, not carried forward")
	assert.GreaterOrEqual(t, adopted, now, "adopted serial is wall-clock time")

	// One-time only: the next call must behave normally, not reset again.
	next := NextSerial(adopted)
	assert.Greater(t, next, adopted, "after adoption the serial advances normally")
	assert.LessOrEqual(t, next-adopted, uint32(2), "a normal advance is seconds, not another reset")
}

// Drift produced by the collision guard is legitimate and must survive: a
// burst leaves the serial slightly ahead of the clock, and resetting it
// would hand a secondary a serial it has already seen.
func TestNextSerialKeepsLegitimateDrift(t *testing.T) {
	now := uint32(time.Now().UTC().Unix())

	for _, ahead := range []uint32{1, 60, 3600, maxSerialDrift} {
		current := now + ahead
		got := NextSerial(current)
		assert.Equal(t, current+1, got,
			"a serial %ds ahead of the clock is drift, not a legacy value", ahead)
	}

	// Just past the bound it is no longer explicable as drift.
	beyond := now + maxSerialDrift + 2
	assert.Less(t, NextSerial(beyond), beyond, "past the drift bound the serial resets to wall clock")
}
