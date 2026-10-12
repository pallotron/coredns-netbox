package zonegen

import (
	"fmt"
	"strconv"
	"time"
)

// NextSerial returns the next SOA serial. Serials are seeded in YYYYMMDDNN
// form for operator readability, but monotonicity wins over the date
// encoding: once a day's two-digit NN window is exhausted the serial keeps
// incrementing into the next day's numeric space (RFC 1982 — secondaries
// only compare serials, never parse them). The encoding re-synchronizes on
// the first write of a later day, when that day's base overtakes the
// carried-forward value.
func NextSerial(current uint32) uint32 {
	now := time.Now().UTC()
	todayBase := uint32(now.Year()*1000000 + int(now.Month())*10000 + now.Day()*100)

	if current < todayBase {
		return todayBase + 1
	}
	return current + 1
}

// ParseSerial parses a serial number from a string.
func ParseSerial(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid serial %q: %w", s, err)
	}
	return uint32(v), nil
}
