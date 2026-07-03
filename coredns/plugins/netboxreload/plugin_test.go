package netboxreload

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
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

const testZone = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052101 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
server1 IN A 10.0.0.1
server2 IN AAAA 2001:db8::2
`

func TestServeDNS_A(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")
	req := new(dns.Msg)
	req.SetQuestion("server1.mycompany.com.", dns.TypeA)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	require.Len(t, rw.msg.Answer, 1)
	a := rw.msg.Answer[0].(*dns.A)
	assert.Equal(t, "10.0.0.1", a.A.String())
}

func TestServeDNS_AAAA(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")
	req := new(dns.Msg)
	req.SetQuestion("server2.mycompany.com.", dns.TypeAAAA)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	require.Len(t, rw.msg.Answer, 1)
}

func TestServeDNS_NXDOMAIN(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")
	req := new(dns.Msg)
	req.SetQuestion("notexist.mycompany.com.", dns.TypeA)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	assert.Equal(t, dns.RcodeNameError, rw.msg.Rcode)
	// SOA in authority section
	assert.NotEmpty(t, rw.msg.Ns)
}

func TestServeDNS_NoZonePassesThrough(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")
	// query for a name outside any loaded zone
	req := new(dns.Msg)
	req.SetQuestion("notexample.org.", dns.TypeA)
	rw := &responseRecorder{}
	// Next is nil → NextOrFailure returns SERVFAIL
	code, _ := p.ServeDNS(context.Background(), rw, req)
	assert.Equal(t, dns.RcodeServerFailure, code)
}

func TestServeDNS_NODATA(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")
	req := new(dns.Msg)
	req.SetQuestion("server1.mycompany.com.", dns.TypeAAAA) // server1 only has A
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	assert.Equal(t, dns.RcodeSuccess, code)
	assert.Equal(t, dns.RcodeSuccess, rw.msg.Rcode) // NODATA, not NXDOMAIN
	assert.Empty(t, rw.msg.Answer)
	// SOA must be in authority section per RFC 2308 §2.2
	assert.NotEmpty(t, rw.msg.Ns, "NODATA response must include SOA in authority section")
}

const cnameZone = `$ORIGIN example.org.
$TTL 300
@ IN SOA ns1.example.org. admin.example.org. (2026052101 3600 900 604800 86400)
@ IN NS ns1.example.org.
host1    IN A     10.0.0.1
alias1   IN CNAME host1.example.org.
alias2   IN CNAME alias1.example.org.
dangling IN CNAME missing.example.org.
loopa    IN CNAME loopb.example.org.
loopb    IN CNAME loopa.example.org.
external IN CNAME target.other-zone.net.
`

// answerNames returns "<name> <type>" strings for each answer RR, preserving
// order, for concise assertions on the CNAME chase chain.
func answerNames(m *dns.Msg) []string {
	out := make([]string, 0, len(m.Answer))
	for _, rr := range m.Answer {
		out = append(out, rr.Header().Name+" "+dns.TypeToString[rr.Header().Rrtype])
	}
	return out
}

func serve(t *testing.T, p *Plugin, name string, qtype uint16) *dns.Msg {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(name, qtype)
	rw := &responseRecorder{}
	code, err := p.ServeDNS(context.Background(), rw, req)
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, code)
	require.NotNil(t, rw.msg)
	return rw.msg
}

func TestServeDNS_CNAMEChase_SingleHop(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "alias1.example.org.", dns.TypeA)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	assert.Equal(t, []string{
		"alias1.example.org. CNAME",
		"host1.example.org. A",
	}, answerNames(m))
	a, ok := m.Answer[1].(*dns.A)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1", a.A.String())
}

func TestServeDNS_CNAMEChase_MultiHop(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "alias2.example.org.", dns.TypeA)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	assert.Equal(t, []string{
		"alias2.example.org. CNAME",
		"alias1.example.org. CNAME",
		"host1.example.org. A",
	}, answerNames(m))
}

func TestServeDNS_CNAMEQuery_NoChase(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "alias1.example.org.", dns.TypeCNAME)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	assert.Equal(t, []string{"alias1.example.org. CNAME"}, answerNames(m))
}

func TestServeDNS_ANYQuery_NoChase(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "alias1.example.org.", dns.TypeANY)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	assert.Equal(t, []string{"alias1.example.org. CNAME"}, answerNames(m))
}

func TestServeDNS_CNAMEChase_Dangling(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "dangling.example.org.", dns.TypeA)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	assert.Equal(t, []string{"dangling.example.org. CNAME"}, answerNames(m))
}

func TestServeDNS_CNAMEChase_Loop(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "loopa.example.org.", dns.TypeA)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	// Both CNAMEs appear at most once; resolution terminates without hanging.
	assert.Equal(t, []string{
		"loopa.example.org. CNAME",
		"loopb.example.org. CNAME",
	}, answerNames(m))
}

func TestServeDNS_CNAMEChase_ExternalTarget(t *testing.T) {
	p := newTestPlugin(t, cnameZone, "example.org")
	m := serve(t, p, "external.example.org.", dns.TypeA)
	assert.Equal(t, dns.RcodeSuccess, m.Rcode)
	assert.Equal(t, []string{"external.example.org. CNAME"}, answerNames(m))
}

func TestPollLoop_Cancellation(t *testing.T) {
	dir := t.TempDir()
	content := `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052101 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte(content), 0o644))
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

func TestTransfer_AXFR(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")

	ch, err := p.Transfer("mycompany.com.", 0)
	require.NoError(t, err)

	var all []dns.RR
	for batch := range ch {
		all = append(all, batch...)
	}

	// Must start and end with SOA
	require.NotEmpty(t, all)
	assert.Equal(t, dns.TypeSOA, all[0].Header().Rrtype, "first record must be SOA")
	assert.Equal(t, dns.TypeSOA, all[len(all)-1].Header().Rrtype, "last record must be SOA")

	// Must contain A record for server1
	var found bool
	for _, rr := range all {
		if rr.Header().Rrtype == dns.TypeA && strings.EqualFold(rr.Header().Name, "server1.mycompany.com.") {
			found = true
		}
	}
	assert.True(t, found, "AXFR should include A record for server1")
}

func TestTransfer_NotAuthoritative(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")
	_, err := p.Transfer("other.org.", 0)
	assert.Error(t, err)
}

func TestTransfer_IXFR_UpToDate(t *testing.T) {
	p := newTestPlugin(t, testZone, "mycompany.com")

	// Get current serial from a full transfer first
	ch, err := p.Transfer("mycompany.com.", 0)
	require.NoError(t, err)
	var soa *dns.SOA
	for batch := range ch {
		for _, rr := range batch {
			if s, ok := rr.(*dns.SOA); ok && soa == nil {
				soa = s
			}
		}
	}
	require.NotNil(t, soa)

	// IXFR with current serial — should return just SOA (no-op)
	ch2, err := p.Transfer("mycompany.com.", soa.Serial)
	require.NoError(t, err)
	var rrs []dns.RR
	for batch := range ch2 {
		rrs = append(rrs, batch...)
	}
	require.Len(t, rrs, 1)
	assert.Equal(t, dns.TypeSOA, rrs[0].Header().Rrtype)
}

func TestReloadZones(t *testing.T) {
	dir := t.TempDir()
	v1 := `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052101 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
old IN A 10.0.0.1
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte(v1), 0o644))
	zones, err := loadZoneDir(dir)
	require.NoError(t, err)
	p := &Plugin{Dir: dir, zones: zones}

	// overwrite zone with new record
	v2 := `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052102 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
new IN A 10.0.0.2
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte(v2), 0o644))
	require.NoError(t, p.reloadZones())

	p.mu.RLock()
	defer p.mu.RUnlock()
	assert.NotContains(t, p.zones["mycompany.com."].records, "old.mycompany.com.")
	assert.Contains(t, p.zones["mycompany.com."].records, "new.mycompany.com.")
}
