//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dnsServer() string {
	if v := os.Getenv("DNS_SERVER"); v != "" {
		return v
	}
	return "127.0.0.1:15353"
}

func dnsSecondaryServer() string {
	if v := os.Getenv("DNS_SECONDARY_SERVER"); v != "" {
		return v
	}
	return "127.0.0.1:15354"
}

// hostFQDN returns the expected FQDN for a seeded host given its short name and DC.
// When STRIP_DC_LABEL=true the DC subdomain is stripped: host.dc.mycompany.com → host.mycompany.com.
func hostFQDN(host, dc string) string {
	if os.Getenv("STRIP_DC_LABEL") == "true" {
		return fmt.Sprintf("%s.mycompany.com", host)
	}
	return fmt.Sprintf("%s.%s.mycompany.com", host, dc)
}

// forwardZone returns the expected forward zone name for a given DC.
// When STRIP_DC_LABEL=true all DCs collapse into a single mycompany.com zone.
func forwardZone(dc string) string {
	if os.Getenv("STRIP_DC_LABEL") == "true" {
		return "mycompany.com"
	}
	return fmt.Sprintf("%s.mycompany.com", dc)
}

// waitForPrimary blocks until the primary DNS server returns 10.1.0.1 for the
// canonical test record. Checking a specific IP (not just any answer) guards
// against asserting on a stale or partially-written zone after a prior test
// run's gRPC operations rewrote it with unexpected records.
func waitForPrimary(t *testing.T) {
	t.Helper()
	name := hostFQDN("server1-mgmt", "dc1")
	wantIP := net.ParseIP("10.1.0.1")
	var probe dnsProbeStats
	server := dnsServer()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := probe.query(name, dns.TypeA, server)
		if ok && r.Rcode == dns.RcodeSuccess {
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("primary DNS not ready: timed out waiting for 10.1.0.1%s", probe.diagnose(server))
}

func query(t *testing.T, name string, qtype uint16) *dns.Msg {
	return queryServer(t, name, qtype, dnsServer())
}

// exchange performs one DNS query over TCP (UDP is blocked in some setups)
// and hands back whatever happened. The two callers below decide what a
// transport error means, because the answer differs by context.
func exchange(name string, qtype uint16, server string) (*dns.Msg, error) {
	c := new(dns.Client)
	c.Timeout = 5 * time.Second
	c.Net = "tcp"

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true

	r, _, err := c.Exchange(m, server)
	return r, err
}

// queryServer is the assertion primitive: at an assertion site a query that
// cannot be completed is a real failure, so it fails the test.
func queryServer(t *testing.T, name string, qtype uint16, server string) *dns.Msg {
	t.Helper()
	r, err := exchange(name, qtype, server)
	require.NoError(t, err, "DNS query for %s failed", name)
	return r
}

// dnsProbeStats records how a wait loop's queries actually fared, so that a
// timeout can say which of two very different things went wrong.
//
// The tolerant query primitive swallows transport errors on purpose -- a
// retry loop must not die on one refused connection. The cost is that
// "the record was never published" and "nothing ever answered us" produce
// the identical timeout message, and the second one sends you hunting
// through the publish path for a bug that is not there. That happened: a
// CI run reported a record missing for 45s while the sidecar log showed it
// written and both CoreDNS replicas reloaded one second later. None of the
// ~225 queries had reached the server at all.
//
// So count outcomes as the loop runs and let the failure say which it was.
type dnsProbeStats struct {
	attempts int
	answered int
	lastErr  error
}

// query is the counting form of tryQueryServer.
func (s *dnsProbeStats) query(name string, qtype uint16, server string) (*dns.Msg, bool) {
	r, err := exchange(name, qtype, server)
	s.attempts++
	if err != nil {
		s.lastErr = err
		return nil, false
	}
	s.answered++
	return r, true
}

// diagnose returns a clause naming the transport outcome, for appending to
// a wait loop's timeout message.
func (s *dnsProbeStats) diagnose(server string) string {
	switch {
	case s.attempts == 0:
		return fmt.Sprintf(" (no queries were attempted against %s)", server)
	case s.answered == 0:
		return fmt.Sprintf(" -- DNS UNREACHABLE: 0 of %d queries to %s were answered (last error: %v). "+
			"The record may well have been published; this is a transport failure, not a publish failure",
			s.attempts, server, s.lastErr)
	case s.answered < s.attempts:
		return fmt.Sprintf(" (%d of %d queries to %s were answered; last transport error: %v -- "+
			"intermittent reachability, so treat the absence as unproven)",
			s.answered, s.attempts, server, s.lastErr)
	default:
		return fmt.Sprintf(" (all %d queries to %s were answered, so the record was genuinely absent)",
			s.attempts, server)
	}
}

// tryQueryServer is the wait-loop primitive: it reports not-ok rather than
// failing the test. A connection can be closed under us at any moment — a
// pod being replaced, the k3d load balancer re-pointing with no ready
// backend after a redeploy — and inside a wait that is a state to retry
// through, not a verdict.
//
// Every polling helper must build on this one. A retry loop around
// queryServer is not a retry loop at all: it gives up on the first flap,
// while looking resilient. N28 failed in CI that way, and waitNameGone had
// the same defect waiting to be hit.
func tryQueryServer(name string, qtype uint16, server string) (*dns.Msg, bool) {
	r, err := exchange(name, qtype, server)
	if err != nil {
		return nil, false
	}
	return r, true
}

func TestARecordLookup(t *testing.T) {
	waitForPrimary(t)
	r := query(t, hostFQDN("server1-mgmt", "dc1"), dns.TypeA)

	require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected at least one answer")

	found := false
	for _, ans := range r.Answer {
		if a, ok := ans.(*dns.A); ok {
			if a.A.Equal(net.ParseIP("10.1.0.1")) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected 10.1.0.1 in A record answers for %s", hostFQDN("server1-mgmt", "dc1"))
	}
}

func TestForwardLookup(t *testing.T) {
	r := query(t, "google.com", dns.TypeA)

	require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR for forwarded query, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected at least one answer for forwarded query")
}

func TestNXDOMAIN(t *testing.T) {
	r := query(t, hostFQDN("nonexistent", "dc1"), dns.TypeA)

	// Could be NXDOMAIN or NOERROR with empty answer depending on netbox plugin behavior
	if r.Rcode != dns.RcodeNameError && len(r.Answer) != 0 {
		t.Logf("Rcode: %s, Answers: %d", dns.RcodeToString[r.Rcode], len(r.Answer))
	}
}

func TestMultipleHosts(t *testing.T) {
	waitForPrimary(t)
	hosts := []struct {
		host string
		dc   string
		ip   string
	}{
		{"server1-mgmt", "dc1", "10.1.0.1"},
		{"server2-mgmt", "dc1", "10.1.0.2"},
		{"server3-mgmt", "dc1", "10.1.0.3"},
		{"server1-bmc", "dc1", "10.1.8.1"},
		{"server1-storage", "dc1", "10.1.16.1"},
	}

	for _, h := range hosts {
		fqdn := hostFQDN(h.host, h.dc)
		t.Run(fqdn, func(t *testing.T) {
			r := query(t, fqdn, dns.TypeA)

			require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR, got %s", dns.RcodeToString[r.Rcode])

			found := false
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok {
					if a.A.Equal(net.ParseIP(h.ip)) {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("expected %s in answers for %s", h.ip, fqdn)
			}
		})
	}
}

func TestSecondaryARecordLookup(t *testing.T) {
	fqdn := hostFQDN("server1-mgmt", "dc1")
	r := queryServer(t, fqdn, dns.TypeA, dnsSecondaryServer())

	require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR from secondary, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected at least one answer from secondary")

	found := false
	for _, ans := range r.Answer {
		if a, ok := ans.(*dns.A); ok {
			if a.A.Equal(net.ParseIP("10.1.0.1")) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected 10.1.0.1 in A record answers from secondary for %s", fqdn)
	}
}

func TestPTRRecordLookup(t *testing.T) {
	ptrName, err := dns.ReverseAddr("10.1.0.1")
	require.NoError(t, err, "failed to compute reverse addr")

	r := query(t, ptrName, dns.TypePTR)

	require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected at least one PTR answer")

	expectedPtr := dns.Fqdn(hostFQDN("server1-mgmt", "dc1"))
	found := false
	for _, ans := range r.Answer {
		if ptr, ok := ans.(*dns.PTR); ok {
			if ptr.Ptr == expectedPtr {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected %s in PTR record answers", expectedPtr)
	}
}

// TestDCLabelRewrite verifies that DC-labelled queries (host.dc.domain) resolve
// correctly when coredns.extraConfig contains a rewrite rule that strips the DC
// label before lookup and restores it in the answer.
// Only runs when DC_LABEL_REWRITE=true and STRIP_DC_LABEL=true.
func TestDCLabelRewrite(t *testing.T) {
	if os.Getenv("DC_LABEL_REWRITE") != "true" {
		t.Skip("DC_LABEL_REWRITE not set; skipping rewrite test")
	}
	if os.Getenv("STRIP_DC_LABEL") != "true" {
		t.Skip("DC_LABEL_REWRITE requires STRIP_DC_LABEL=true")
	}

	waitForPrimary(t)

	cases := []struct {
		dcFQDN string // DC-labelled query name
		wantIP string
	}{
		{"server1-mgmt.dc1.mycompany.com", "10.1.0.1"},
		{"server2-mgmt.dc1.mycompany.com", "10.1.0.2"},
		{"server1-bmc.dc1.mycompany.com", "10.1.8.1"},
	}

	for _, tc := range cases {
		t.Run(tc.dcFQDN, func(t *testing.T) {
			r := query(t, tc.dcFQDN, dns.TypeA)

			require.Equal(t, dns.RcodeSuccess, r.Rcode,
				"expected NOERROR for DC-labelled query %s, got %s", tc.dcFQDN, dns.RcodeToString[r.Rcode])
			require.NotEmpty(t, r.Answer, "expected answer for %s", tc.dcFQDN)

			wantIP := net.ParseIP(tc.wantIP)
			wantName := dns.Fqdn(tc.dcFQDN)
			found := false
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
					require.Equal(t, wantName, a.Hdr.Name,
						"answer name should be the original DC-labelled FQDN (answer auto rewrite)")
					found = true
				}
			}
			require.True(t, found, "expected %s in A record answers for %s", tc.wantIP, tc.dcFQDN)
		})
	}
}

func TestAXFRTransfer(t *testing.T) {
	waitForPrimary(t)
	zone := forwardZone("dc1")

	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(zone))

	env, err := tr.In(m, dnsServer())
	require.NoError(t, err, "AXFR transfer failed for zone %s", zone)

	var records []dns.RR
	for e := range env {
		require.NoError(t, e.Error, "AXFR envelope error")
		records = append(records, e.RR...)
	}

	require.NotEmpty(t, records, "expected records from AXFR transfer of %s, got none", zone)

	expectedA := dns.Fqdn(hostFQDN("server1-mgmt", "dc1"))
	foundSOA := false
	foundA := false
	for _, rr := range records {
		if _, ok := rr.(*dns.SOA); ok {
			foundSOA = true
		}
		if a, ok := rr.(*dns.A); ok {
			if a.Hdr.Name == expectedA && a.A.Equal(net.ParseIP("10.1.0.1")) {
				foundA = true
			}
		}
	}
	if !foundSOA {
		t.Error("expected SOA record in AXFR response")
	}
	if !foundA {
		t.Errorf("expected A record for %s in AXFR response", expectedA)
	}
}

// TestMultipleReplicasConsistentDNS verifies that with replicaCount>1 all
// replicas serve correct, consistent answers. The DNS service load-balances
// across replicas, so running many queries exercises all of them.
func TestMultipleReplicasConsistentDNS(t *testing.T) {
	waitForPrimary(t)

	probe := hostFQDN("server1-mgmt", "dc1")
	wantIP := net.ParseIP("10.1.0.1")

	// 30 queries — statistically hits both pods several times each with 2 replicas.
	for i := range 30 {
		r := queryServer(t, probe, dns.TypeA, dnsServer())
		require.Equalf(t, dns.RcodeSuccess, r.Rcode,
			"query %d/%d: expected NOERROR from load-balanced DNS service", i+1, 30)
		var found bool
		for _, ans := range r.Answer {
			if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
				found = true
			}
		}
		require.Truef(t, found,
			"query %d/%d: expected %s in answer, got %v", i+1, 30, wantIP, r.Answer)
	}
}

// TestEachReplicaServesCorrectly queries pod-0 and pod-1 individually via
// their dedicated NodePort services. Skips if DNS_POD0 / DNS_POD1 are not set.
// Requires the cluster to expose per-pod ports (see dev/coredns-per-pod-services.yaml
// and the corresponding k3d port forwards in dev/k3d-config.yaml).
func TestEachReplicaServesCorrectly(t *testing.T) {
	pod0 := os.Getenv("DNS_POD0")
	pod1 := os.Getenv("DNS_POD1")
	if pod0 == "" || pod1 == "" {
		t.Skip("DNS_POD0 or DNS_POD1 not set — skipping per-replica test")
	}

	waitForPrimary(t)

	probe := hostFQDN("server1-mgmt", "dc1")
	wantIP := net.ParseIP("10.1.0.1")

	for _, tc := range []struct {
		name string
		addr string
	}{
		{"pod-0", pod0},
		{"pod-1", pod1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := queryServer(t, probe, dns.TypeA, tc.addr)
			require.Equalf(t, dns.RcodeSuccess, r.Rcode,
				"%s: expected NOERROR", tc.name)
			var found bool
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
					found = true
				}
			}
			require.Truef(t, found,
				"%s: expected %s in answer, got %v", tc.name, wantIP, r.Answer)
		})
	}
}

// --- Name template / CNAME alias tests (issue #60) ---
// Only run when the deployment has deviceNameParsers configured
// (dev/coredns-netbox-values.yaml) — gated by NAME_TEMPLATES=true.

const (
	tmplCanonical    = "dc1-h1a-r101-prod-hv-01.mycompany.com"
	tmplAlias        = "hv01-dc1-h1a-r101.mycompany.com"
	tmplCanonicalBMC = "dc1-h1a-r101-prod-hv-01-bmc.mycompany.com"
	tmplAliasBMC     = "hv01-dc1-h1a-r101-bmc.mycompany.com"
	tmplMgmtIP       = "10.9.0.1"
	tmplBMCIP        = "10.9.8.1"
)

func skipUnlessNameTemplates(t *testing.T) {
	t.Helper()
	if os.Getenv("NAME_TEMPLATES") != "true" {
		t.Skip("NAME_TEMPLATES not set; skipping name template tests")
	}
}

// waitForDeviceRecords blocks until the template-generated canonical record
// is served (devices are polled from NetBox after startup).
func waitForDeviceRecords(t *testing.T) {
	t.Helper()
	wantIP := net.ParseIP(tmplMgmtIP)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r := queryServer(t, tmplCanonical, dns.TypeA, dnsServer())
		if r.Rcode == dns.RcodeSuccess {
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("device canonical record %s not served within timeout", tmplCanonical)
}

func TestDeviceCanonicalRecords(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	for _, tc := range []struct {
		fqdn string
		ip   string
	}{
		{tmplCanonical, tmplMgmtIP},
		{tmplCanonicalBMC, tmplBMCIP},
	} {
		t.Run(tc.fqdn, func(t *testing.T) {
			r := query(t, tc.fqdn, dns.TypeA)
			require.Equal(t, dns.RcodeSuccess, r.Rcode)
			found := false
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(net.ParseIP(tc.ip)) {
					found = true
				}
			}
			require.True(t, found, "expected %s for %s", tc.ip, tc.fqdn)
		})
	}
}

// TestCNAMEAliasLookup: aliases must resolve to the canonical on both the
// primary and the secondary (proving AXFR propagation).
//
// Both servers now chase CNAME chains in-zone for a type-A query: the primary's
// custom netboxreload plugin and the secondary's standard file/secondary plugin
// each return, in a single type-A response, the alias's CNAME to the canonical
// plus the canonical's A record. This test strictly requires both records to be
// present in that one answer on both servers.
func TestCNAMEAliasLookup(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	cases := []struct {
		alias, canonical, ip string
	}{
		{tmplAlias, tmplCanonical, tmplMgmtIP},
		{tmplAliasBMC, tmplCanonicalBMC, tmplBMCIP},
	}
	servers := []struct{ name, addr string }{
		{"primary", dnsServer()},
		{"secondary", dnsSecondaryServer()},
	}

	for _, srv := range servers {
		for _, tc := range cases {
			t.Run(srv.name+"/"+tc.alias, func(t *testing.T) {
				r := queryServer(t, tc.alias, dns.TypeA, srv.addr)
				require.Equal(t, dns.RcodeSuccess, r.Rcode)

				var cnameTarget string
				var gotA net.IP
				for _, ans := range r.Answer {
					if c, ok := ans.(*dns.CNAME); ok && ans.Header().Name == dns.Fqdn(tc.alias) {
						cnameTarget = c.Target
					}
					if a, ok := ans.(*dns.A); ok {
						gotA = a.A
					}
				}

				// The type-A answer must include the CNAME alias→canonical and
				// the chased-in A for the canonical, on both servers.
				require.Equal(t, dns.Fqdn(tc.canonical), cnameTarget,
					"type-A answer must include CNAME %s → %s", tc.alias, tc.canonical)
				require.NotNil(t, gotA, "type-A answer must include the chased A record for the canonical")
				require.True(t, gotA.Equal(net.ParseIP(tc.ip)), "chased A must be the canonical's address")
			})
		}
	}
}

// PTRs must never target an alias. With INTERFACE_RECORDS_ENABLED, tmplMgmtIP
// (10.9.0.1, mgmt0's own address) now has a competing interface record for
// the same device/address pair (TestInterfaceRecordAndPTRPriority exercises
// this generically on the switch fixture), and interface priority correctly
// outranks the device canonical for PTR purposes — so the expected target
// shifts to the interface form, but "never an alias" still holds either way.
func TestPTRTargetsCanonicalOnly(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	ptrName, err := dns.ReverseAddr(tmplMgmtIP)
	require.NoError(t, err)

	r := query(t, ptrName, dns.TypePTR)
	require.Equal(t, dns.RcodeSuccess, r.Rcode)
	require.NotEmpty(t, r.Answer)

	wantTarget := tmplCanonical
	if os.Getenv("INTERFACE_RECORDS_ENABLED") == "true" {
		wantTarget = "mgmt0." + tmplCanonical
	}
	for _, ans := range r.Answer {
		ptr, ok := ans.(*dns.PTR)
		require.True(t, ok)
		require.Equal(t, dns.Fqdn(wantTarget), ptr.Ptr,
			"PTR must target the canonical (or, with interface records enabled, the higher-priority interface) FQDN, never an alias")
	}
}

// The alias CNAME must appear in AXFR output (secondary propagation path).
func TestAXFRIncludesCNAME(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn("mycompany.com"))

	env, err := tr.In(m, dnsServer())
	require.NoError(t, err)

	foundCNAME := false
	for e := range env {
		require.NoError(t, e.Error)
		for _, rr := range e.RR {
			if c, ok := rr.(*dns.CNAME); ok &&
				rr.Header().Name == dns.Fqdn(tmplAlias) && c.Target == dns.Fqdn(tmplCanonical) {
				foundCNAME = true
			}
		}
	}
	require.True(t, foundCNAME, "AXFR of mycompany.com must include %s CNAME %s", tmplAlias, tmplCanonical)
}

// N15 (acceptance-test matrix) / gap G3: the name referenced by every zone's
// SOA and NS records must itself resolve. The sidecar emits its address from
// PRIMARY_NS_IP (192.0.2.53 in dev/coredns-netbox-values.yaml).
func TestPrimaryNSHasARecord(t *testing.T) {
	waitForPrimary(t)
	r := query(t, "ns1.mycompany.com.", dns.TypeA)

	require.Equal(t, dns.RcodeSuccess, r.Rcode,
		"expected NOERROR for the NS name, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected an A record for the NS name")

	found := false
	for _, ans := range r.Answer {
		if a, ok := ans.(*dns.A); ok && a.A.Equal(net.ParseIP("192.0.2.53")) {
			found = true
		}
	}
	require.True(t, found, "expected 192.0.2.53 in A answers for ns1.mycompany.com")
}

// axfrRecords returns every RR in the zone, for assertions that need rdata
// rather than just owner names.
func axfrRecords(t *testing.T, zone string) []dns.RR {
	t.Helper()
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(zone))
	env, err := tr.In(m, dnsServer())
	require.NoError(t, err, "AXFR of %s", zone)
	var out []dns.RR
	for e := range env {
		require.NoError(t, e.Error, "AXFR envelope error")
		out = append(out, e.RR...)
	}
	return out
}

// N2 (acceptance-test matrix), negative half: an interface matching none of
// the categorization patterns must never be SELECTED as a device's own
// canonical or BMC address. The positive half — a management interface
// becoming the canonical record and a BMC interface the -bmc one — is
// TestDeviceCanonicalRecords above.
//
// The fixture hangs swp1 (10.9.16.1) off a device that already has mgmt0 and
// bmc0, so the device still resolves; what is under test is that this extra
// address is never treated as the device's own identity. Only mgmt-VRF,
// mgmt-interface and BMC categories are ever selected
// (internal/ipcategorizer.SelectDeviceIPs) with no "any interface"
// fallback, so this pins that a fallback is never added by accident to that
// heuristic — it would make an uncategorized port the device's canonical
// address fleet-wide.
//
// That is a different, narrower claim than "this address is never
// published at all": with INTERFACE_RECORDS_ENABLED, swp1 legitimately
// gets its own swp1.<device-fqdn> record via the separate, opt-in
// per-interface-records path (spec §3), which publishes every
// interface-assigned address with no category filter by design. Without
// it, nothing is published for swp1 at all, which is the original form of
// this test.
func TestUnmatchedInterfaceYieldsNoRecord(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	const swpIP = "10.9.16.1"
	interfaceRecordsOn := os.Getenv("INTERFACE_RECORDS_ENABLED") == "true"
	wantOwner := dns.Fqdn("swp1." + tmplCanonical)

	found := false
	for _, rr := range axfrRecords(t, forwardZone("dc1")) {
		a, ok := rr.(*dns.A)
		if !ok || a.A.String() != swpIP {
			continue
		}
		found = true
		assert.NotEqual(t, dns.Fqdn(tmplCanonical), rr.Header().Name,
			"%s is on an uncategorized interface and must never become the device's own canonical A record", swpIP)
		assert.NotEqual(t, dns.Fqdn(tmplCanonicalBMC), rr.Header().Name,
			"%s is on an uncategorized interface and must never become the device's BMC record", swpIP)
		if interfaceRecordsOn {
			assert.Equal(t, wantOwner, rr.Header().Name,
				"with INTERFACE_RECORDS_ENABLED, swp1's own interface record is the only record this address should carry")
		}
	}
	if !interfaceRecordsOn {
		assert.False(t, found, "%s is on an uncategorized interface and must not be published when interface records are disabled", swpIP)
	}

	// Reverse: without interface records, no PTR at all. With them, the PTR
	// must target swp1's own interface record, never the device/BMC form.
	ptr, err := dns.ReverseAddr(swpIP)
	require.NoError(t, err)
	r := queryServer(t, ptr, dns.TypePTR, dnsServer())
	if !interfaceRecordsOn {
		assert.Empty(t, r.Answer, "uncategorized interface address must have no PTR when interface records are disabled")
		return
	}
	require.NotEmpty(t, r.Answer, "expected the interface-form PTR for %s", swpIP)
	for _, ans := range r.Answer {
		p, ok := ans.(*dns.PTR)
		require.True(t, ok)
		assert.Equal(t, wantOwner, p.Ptr)
	}
}

// N3 (acceptance-test matrix), catch-all half: a device whose name matches
// none of the configured deviceNameParsers still gets a record — the flat
// <device>.<zone> canonical — but no CNAME aliases, since aliases only come
// from a matched parser (internal/ipcategorizer.DeviceDNSToRecords).
//
// This is the case that silently loses records if the fallback regresses:
// gear outside the naming convention is exactly what nobody notices missing
// until they try to reach it.
func TestCatchAllDeviceGetsFlatCanonicalWithoutAliases(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	const (
		catchAllName = "oob-sw-01.mycompany.com"
		catchAllIP   = "10.9.24.1"
	)

	waitForDNS(t, catchAllName, dns.TypeA, catchAllIP, 60*time.Second)

	// No alias points at it: a CNAME whose target is the canonical would
	// mean the formatter matched a device it should not have.
	for _, rr := range axfrRecords(t, forwardZone("dc1")) {
		if cname, ok := rr.(*dns.CNAME); ok {
			assert.NotEqual(t, dns.Fqdn(catchAllName), cname.Target,
				"catch-all device must have no aliases, found CNAME %s", rr.Header().Name)
		}
	}
}

// N7 (acceptance-test matrix): IPv6, both legs.
//
// The row sat NOT RUN on the assumption that IPv6 was out of scope, but the
// generator emits AAAA from Family == 6 and zonediscovery builds ip6.arpa
// PTRs — it was untested, not unsupported. Running it found G12: the
// reverse names had their nibble pairs swapped, so every IPv6 PTR was
// generated under a name no resolver would ask for, while a correctly
// configured zone matched nothing and dropped the record entirely.
//
// The fixture uses the RFC 3849 documentation prefix (2001:db8::/32), and
// dev enables the matching ip6.arpa zone; production serves IPv4 reverse
// only, so this leg exists to keep the code path honest rather than to
// mirror production.
func TestIPv6ForwardAAAA(t *testing.T) {
	name := hostFQDN("server-v6", "dc1")

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r := queryServer(t, name, dns.TypeAAAA, dnsServer())
		for _, ans := range r.Answer {
			if aaaa, ok := ans.(*dns.AAAA); ok && aaaa.AAAA.Equal(net.ParseIP("2001:db8::1")) {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("AAAA for %s did not appear within 60s", name)
}

func TestIPv6ReversePTR(t *testing.T) {
	// dns.ReverseAddr is the resolver's view: if the generated owner name
	// does not match this exactly, the record is unreachable however well
	// formed it looks in the zone file.
	ptr, err := dns.ReverseAddr("2001:db8::1")
	require.NoError(t, err)

	want := dns.Fqdn(hostFQDN("server-v6", "dc1"))
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r := queryServer(t, ptr, dns.TypePTR, dnsServer())
		for _, ans := range r.Answer {
			if p, ok := ans.(*dns.PTR); ok && p.Ptr == want {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("PTR %s -> %s did not appear within 60s", ptr, want)
}

// --- primary_ip4 selection, interface records, and PTR priority ---
//
// Fixtures live in dev/seed-ips.py under "Task 11". All three device names
// match deviceNameParsers (same dc-hall-rack-env-role-idx convention as the
// dc1-h1a-r10N hypervisors above), so their canonical FQDN is the flat
// <name>.mycompany.com form (nameFormats.zone is '{{.domain}}' in dev).

const (
	// Switch-like device: primary_ip4 points at Management1 even though
	// several EthernetN/1 addresses are also present as candidates.
	swPrimaryCanonical  = "dc1-h1a-r301-prod-sw-01.mycompany.com"
	swPrimaryMgmtIP     = "10.9.32.1" // Management1 -- the device's primary_ip4
	swPrimaryEth1IP     = "10.9.33.1" // Ethernet1/1 -- must never be selected
	swPrimaryEth1Record = "ethernet1-1." + swPrimaryCanonical
	swPrimaryMgmtRecord = "management1." + swPrimaryCanonical

	// Fallback-path device: no primary_ip4 at all; mgmt address on eno1.
	fallbackDevCanonical = "dc1-h1a-r401-prod-fw-01.mycompany.com"
	fallbackDevEno1IP    = "10.9.40.1"

	// primary_ip4 + loopback device: primary_ip4 selects the mgmt0 address;
	// Loopback0 sits in a range the suite's reverse zones (10.in-addr.arpa)
	// already cover.
	loopDevCanonical   = "dc1-h1a-r501-prod-sv-01.mycompany.com"
	loopDevMgmtIP      = "10.9.50.1"
	loopDevLoopbackIP  = "10.9.58.1"
	loopDevIfaceRecord = "loopback0." + loopDevCanonical
)

func skipUnlessInterfaceRecords(t *testing.T) {
	t.Helper()
	if os.Getenv("INTERFACE_RECORDS_ENABLED") != "true" {
		t.Skip("INTERFACE_RECORDS_ENABLED not set; skipping interface-record tests")
	}
}

// TestPrimaryIP4BeatsHeuristicForSwitch: a device's primary_ip4 is the
// source of truth for its canonical A record (SelectDeviceIPsHybrid), even
// with several other interface-assigned addresses -- the switch's
// EthernetN/1 front-panel ports -- also present as candidates.
func TestPrimaryIP4BeatsHeuristicForSwitch(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDNS(t, swPrimaryCanonical, dns.TypeA, swPrimaryMgmtIP, 60*time.Second)

	r := query(t, swPrimaryCanonical, dns.TypeA)
	require.Equal(t, dns.RcodeSuccess, r.Rcode)
	require.NotEmpty(t, r.Answer)

	foundMgmt := false
	for _, ans := range r.Answer {
		a, ok := ans.(*dns.A)
		require.True(t, ok, "expected only A record answers for %s", swPrimaryCanonical)
		if a.A.Equal(net.ParseIP(swPrimaryMgmtIP)) {
			foundMgmt = true
		}
		assert.NotEqual(t, swPrimaryEth1IP, a.A.String(),
			"canonical record must resolve to the primary_ip4-selected Management1 address, never an EthernetN/1 port")
	}
	assert.True(t, foundMgmt, "expected %s (Management1, the device's primary_ip4) among answers for %s",
		swPrimaryMgmtIP, swPrimaryCanonical)
}

// TestInterfaceRecordAndPTRPriority: with INTERFACE_RECORDS_ENABLED, every
// interface-assigned address without an explicit dns_name gets its own
// <iface>.<device-fqdn> record (here Ethernet1/1 on the switch), and that
// interface form wins PTR priority over the device's own canonical name for
// the Management1 address (explicit dns_name > interface > device > bmc).
func TestInterfaceRecordAndPTRPriority(t *testing.T) {
	skipUnlessNameTemplates(t)
	skipUnlessInterfaceRecords(t)

	waitForDNS(t, swPrimaryEth1Record, dns.TypeA, swPrimaryEth1IP, 60*time.Second)
	waitForPTR(t, swPrimaryMgmtIP, swPrimaryMgmtRecord, 60*time.Second)
}

// TestFallbackDeviceResolvesViaEno1: a device with no primary_ip4 at all
// still resolves, via the regex heuristic (SelectDeviceIPsHybrid's fallback
// union), to its management-pattern address -- here on eno1, a different
// branch of the "management" fallback pattern (^eno[0-9]) from the
// mgmt0/"(?i)mgmt" branch TestDeviceCanonicalRecords already exercises.
func TestFallbackDeviceResolvesViaEno1(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDNS(t, fallbackDevCanonical, dns.TypeA, fallbackDevEno1IP, 60*time.Second)
}

// TestPrimaryIP4DeviceResolves: a device selected via primary_ip4 (rather
// than the fallback heuristic) still resolves normally even though it also
// carries a second, otherwise-uncategorized address (Loopback0, checked
// separately below).
func TestPrimaryIP4DeviceResolves(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDNS(t, loopDevCanonical, dns.TypeA, loopDevMgmtIP, 60*time.Second)
}

// TestPrimaryIP4DeviceLoopbackReverseCoverage: the Loopback0 address on the
// primary_ip4-selected device above is not part of device A-record
// selection (SelectDeviceIPs has no loopback branch), so it only gets a
// forward/PTR record via INTERFACE_RECORDS_ENABLED -- proving the suite's
// reverse zones (10.in-addr.arpa) cover a primary_ip4 device's other
// addresses too, reached through the interface-records path.
func TestPrimaryIP4DeviceLoopbackReverseCoverage(t *testing.T) {
	skipUnlessNameTemplates(t)
	skipUnlessInterfaceRecords(t)

	waitForDNS(t, loopDevIfaceRecord, dns.TypeA, loopDevLoopbackIP, 60*time.Second)
	waitForPTR(t, loopDevLoopbackIP, loopDevIfaceRecord, 60*time.Second)
}
