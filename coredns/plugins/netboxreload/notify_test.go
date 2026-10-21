package netboxreload

import (
	"fmt"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zoneWithSerial builds a minimal zone carrying just an apex SOA, which is
// all the notify decision looks at.
func zoneWithSerial(t *testing.T, origin string, serial uint32) *zone {
	t.Helper()
	rr, err := dns.NewRR(fmt.Sprintf("%s 300 IN SOA ns1.%s admin.%s %d 300 120 604800 86400",
		origin, origin, origin, serial))
	require.NoError(t, err)
	return &zone{origin: origin, records: map[string][]dns.RR{origin: {rr}}}
}

func TestSerialChangedZones(t *testing.T) {
	a1 := zoneWithSerial(t, "a.example.", 2026101801)
	a2 := zoneWithSerial(t, "a.example.", 2026101802)
	b1 := zoneWithSerial(t, "b.example.", 2026101801)

	tests := []struct {
		name string
		old  map[string]*zone
		cur  map[string]*zone
		want []string
	}{
		{
			name: "serial advanced",
			old:  map[string]*zone{"a.example.": a1},
			cur:  map[string]*zone{"a.example.": a2},
			want: []string{"a.example."},
		},
		{
			// A reload can swap in freshly rendered bytes carrying the same
			// serial — a restarted sidecar replaying its zones, say. Every
			// secondary would do a pointless SOA check for nothing.
			name: "same serial, new zone object",
			old:  map[string]*zone{"a.example.": a1},
			cur:  map[string]*zone{"a.example.": zoneWithSerial(t, "a.example.", 2026101801)},
			want: nil,
		},
		{
			name: "first load notifies everything",
			old:  map[string]*zone{},
			cur:  map[string]*zone{"a.example.": a1, "b.example.": b1},
			want: []string{"a.example.", "b.example."},
		},
		{
			name: "only the changed zone is notified",
			old:  map[string]*zone{"a.example.": a1, "b.example.": b1},
			cur:  map[string]*zone{"a.example.": a2, "b.example.": b1},
			want: []string{"a.example."},
		},
		{
			// A zone that disappeared has nobody to notify: a secondary
			// keeps serving its copy until expiry, which is the behaviour
			// the serve-stale story depends on.
			name: "removed zone is not notified",
			old:  map[string]*zone{"a.example.": a1, "b.example.": b1},
			cur:  map[string]*zone{"a.example.": a1},
			want: nil,
		},
		{
			name: "zone without an apex SOA cannot be transferred, so is skipped",
			old:  map[string]*zone{},
			cur:  map[string]*zone{"a.example.": {origin: "a.example.", records: map[string][]dns.RR{}}},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serialChangedZones(tt.old, tt.cur))
		})
	}
}

// Without a transfer block there is nobody allowed to transfer the zone, so
// there is nobody to notify — and the send must not panic on the nil handle.
func TestSendNotifiesWithoutTransferPlugin(t *testing.T) {
	p := &Plugin{}
	assert.NotPanics(t, func() { p.sendNotifies([]string{"a.example."}) })
}

func TestZoneSerial(t *testing.T) {
	z := zoneWithSerial(t, "a.example.", 42)
	got, ok := z.serial()
	require.True(t, ok)
	assert.Equal(t, uint32(42), got)

	_, ok = (&zone{origin: "a.example.", records: map[string][]dns.RR{}}).serial()
	assert.False(t, ok, "a zone with no apex SOA reports no serial")
}
