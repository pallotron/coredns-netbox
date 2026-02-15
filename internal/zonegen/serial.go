package zonegen

import (
	"fmt"
	"strconv"
	"time"
)

// NextSerial returns the next SOA serial in YYYYMMDDNN format.
// If the current serial is from today, it increments NN.
// Otherwise it starts a new day with NN=01.
func NextSerial(current uint32) uint32 {
	now := time.Now().UTC()
	todayBase := uint32(now.Year()*1000000 + int(now.Month())*10000 + now.Day()*100)

	if current >= todayBase && current < todayBase+99 {
		return current + 1
	}
	return todayBase + 1
}

// ParseSerial parses a serial number from a string.
func ParseSerial(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid serial %q: %w", s, err)
	}
	return uint32(v), nil
}
