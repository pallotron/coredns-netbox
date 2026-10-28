package zonegen

import (
	"fmt"
	"strconv"
	"time"
)

// maxSerialDrift bounds how far ahead of wall-clock time a serial can
// legitimately run. The collision guard in NextSerial advances the serial by
// one second per extra write landing in the same second, so reaching a year
// of drift would take ~31.5M writes inside a single second. Anything past it
// is not drift: it is a serial from the old YYYYMMDDNN scheme (or a corrupt
// one), and gets replaced rather than carried forward forever.
const maxSerialDrift = 365 * 24 * 60 * 60

// NextSerial returns the next SOA serial: seconds since the Unix epoch, with
// a monotonic guard.
//
// Secondaries never parse a serial, they only compare it (RFC 1982), so the
// only hard requirements are that it differs from the previous value and
// sorts after it. Wall-clock seconds satisfy both while staying readable —
// the serial converts straight back to the moment the zone was generated.
//
// The guard exists because two writes inside the same second would otherwise
// share a serial, and a secondary already holding it would see no change and
// never transfer: correct on the primary, stale everywhere else, with no
// failure recorded on either side. The webhook fast path writes the instant
// an event arrives, so same-second writes are routine rather than an edge
// case. On a collision the serial takes current+1, running a second or two
// ahead of the clock and re-converging as soon as writes pause.
//
// This replaces YYYYMMDDNN, which had only two digits of room per day. Past
// 99 writes the serial ran into the next day's numeric space, and because
// re-synchronisation needs a later day's base to overtake the carried value,
// sustained load above 99 writes/day made the encoded date drift away from
// the real one permanently.
//
// Switching schemes moves the serial BACKWARDS once, by ~233 million: a 2026
// YYYYMMDDNN serial is ~2.03e9 against ~1.79e9 of Unix time. A secondary
// holding the old value reads the new one as older (RFC 1982) and stops
// transferring until its state is cleared. The drift branch below performs
// that reset deliberately on first run — see docs/deployment.md before
// rolling this out anywhere with live secondaries.
func NextSerial(current uint32) uint32 {
	ts := uint32(time.Now().UTC().Unix())

	// A legacy YYYYMMDDNN serial, a corrupt one, or the 2106 epoch wrap.
	// All three want the same thing: take wall-clock time and move on. The
	// subtraction is ordered this way so it cannot overflow uint32, and at
	// the 2106 wrap the resulting jump is what RFC 1982 reads as newer.
	if current > ts && current-ts > maxSerialDrift {
		return ts
	}
	// Same-second write (or clock stepped backwards): stay strictly
	// increasing rather than reuse a serial a secondary may already hold.
	if ts <= current {
		return current + 1
	}
	return ts
}

// ParseSerial parses a serial number from a string.
func ParseSerial(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid serial %q: %w", s, err)
	}
	return uint32(v), nil
}
