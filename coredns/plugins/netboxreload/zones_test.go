package netboxreload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadZoneFile(t *testing.T) {
	dir := t.TempDir()
	content := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (
    2026052101 3600 900 604800 86400
)
@ IN NS ns1.infra.cx.
server1 IN A 10.0.0.1
server1 IN AAAA 2001:db8::1
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(content), 0o644))

	z, err := loadZoneFile(filepath.Join(dir, "db.infra.cx"))
	require.NoError(t, err)

	assert.Equal(t, "infra.cx.", z.origin)
	aRecs := z.records["server1.infra.cx."]
	require.Len(t, aRecs, 2)
	assert.Equal(t, dns.TypeA, aRecs[0].Header().Rrtype)
	assert.Equal(t, dns.TypeAAAA, aRecs[1].Header().Rrtype)
	apexRecs := z.records["infra.cx."]
	require.NotEmpty(t, apexRecs)
	types := make(map[uint16]bool)
	for _, rr := range apexRecs {
		types[rr.Header().Rrtype] = true
	}
	assert.True(t, types[dns.TypeSOA])
	assert.True(t, types[dns.TypeNS])
}

func TestLoadZoneDir(t *testing.T) {
	dir := t.TempDir()
	zone1 := `$ORIGIN a.cx.
$TTL 300
@ IN SOA ns1.a.cx. admin.a.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.a.cx.
host1 IN A 10.1.0.1
`
	zone2 := `$ORIGIN b.cx.
$TTL 300
@ IN SOA ns1.b.cx. admin.b.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.b.cx.
host2 IN A 10.2.0.1
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.a.cx"), []byte(zone1), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.b.cx"), []byte(zone2), 0o644))
	// non-zone file should be ignored
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.json"), []byte("{}"), 0o644))

	zones, err := loadZoneDir(dir)
	require.NoError(t, err)
	assert.Len(t, zones, 2)
	assert.Contains(t, zones, "a.cx.")
	assert.Contains(t, zones, "b.cx.")
}
