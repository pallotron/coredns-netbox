package netboxreload

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blankRW satisfies dns.ResponseWriter with no-op implementations.
type blankRW struct{}

func (blankRW) LocalAddr() net.Addr        { return nil }
func (blankRW) RemoteAddr() net.Addr       { return nil }
func (blankRW) WriteMsg(*dns.Msg) error    { return nil }
func (blankRW) Write([]byte) (int, error)  { return 0, nil }
func (blankRW) Close() error               { return nil }
func (blankRW) TsigStatus() error          { return nil }
func (blankRW) TsigTimersOnly(bool)        {}
func (blankRW) Hijack()                    {}

// responseRecorder captures the DNS message written by ServeDNS.
type responseRecorder struct {
	blankRW
	msg *dns.Msg
}

func (r *responseRecorder) WriteMsg(m *dns.Msg) error {
	r.msg = m
	return nil
}

func newTestPlugin(t *testing.T, zoneContent, zoneName string) *Plugin {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db."+zoneName), []byte(zoneContent), 0o644))
	zones, err := loadZoneDir(dir)
	require.NoError(t, err)
	return &Plugin{Dir: dir, zones: zones}
}

const testZone = `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
server1 IN A 10.0.0.1
server2 IN AAAA 2001:db8::2
`

func TestServeDNS_A(t *testing.T) {
	p := newTestPlugin(t, testZone, "infra.cx")
	req := new(dns.Msg)
	req.SetQuestion("server1.infra.cx.", dns.TypeA)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	require.Len(t, rw.msg.Answer, 1)
	a := rw.msg.Answer[0].(*dns.A)
	assert.Equal(t, "10.0.0.1", a.A.String())
}

func TestServeDNS_AAAA(t *testing.T) {
	p := newTestPlugin(t, testZone, "infra.cx")
	req := new(dns.Msg)
	req.SetQuestion("server2.infra.cx.", dns.TypeAAAA)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	require.Len(t, rw.msg.Answer, 1)
}

func TestServeDNS_NXDOMAIN(t *testing.T) {
	p := newTestPlugin(t, testZone, "infra.cx")
	req := new(dns.Msg)
	req.SetQuestion("notexist.infra.cx.", dns.TypeA)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	assert.Equal(t, dns.RcodeNameError, rw.msg.Rcode)
	// SOA in authority section
	assert.NotEmpty(t, rw.msg.Ns)
}

func TestServeDNS_NoZonePassesThrough(t *testing.T) {
	p := newTestPlugin(t, testZone, "infra.cx")
	// query for a name outside any loaded zone
	req := new(dns.Msg)
	req.SetQuestion("example.com.", dns.TypeA)
	rw := &responseRecorder{}
	// Next is nil → NextOrFailure returns SERVFAIL
	code, _ := p.ServeDNS(context.Background(), rw, req)
	assert.Equal(t, dns.RcodeServerFailure, code)
}

func TestServeDNS_NODATA(t *testing.T) {
	p := newTestPlugin(t, testZone, "infra.cx")
	req := new(dns.Msg)
	req.SetQuestion("server1.infra.cx.", dns.TypeAAAA) // server1 only has A
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	assert.Equal(t, dns.RcodeSuccess, rw.msg.Rcode) // NODATA, not NXDOMAIN
	assert.Empty(t, rw.msg.Answer)
	// SOA must be in authority section per RFC 2308 §2.2
	assert.NotEmpty(t, rw.msg.Ns, "NODATA response must include SOA in authority section")
}

func TestPollLoop_Cancellation(t *testing.T) {
	dir := t.TempDir()
	content := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(content), 0o644))
	zones, err := loadZoneDir(dir)
	require.NoError(t, err)
	p := &Plugin{Dir: dir, zones: zones, PollInterval: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.pollLoop(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pollLoop did not stop after context cancellation")
	}
}

func TestReloadZones(t *testing.T) {
	dir := t.TempDir()
	v1 := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
old IN A 10.0.0.1
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(v1), 0o644))
	zones, err := loadZoneDir(dir)
	require.NoError(t, err)
	p := &Plugin{Dir: dir, zones: zones}

	// overwrite zone with new record
	v2 := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052102 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
new IN A 10.0.0.2
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(v2), 0o644))
	require.NoError(t, p.reloadZones())

	p.mu.RLock()
	defer p.mu.RUnlock()
	assert.NotContains(t, p.zones["infra.cx."].records, "old.infra.cx.")
	assert.Contains(t, p.zones["infra.cx."].records, "new.infra.cx.")
}
