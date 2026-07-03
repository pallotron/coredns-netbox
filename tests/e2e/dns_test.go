//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/miekg/dns"
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
	probe := hostFQDN("server1-mgmt", "dc1")
	wantIP := net.ParseIP("10.1.0.1")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r := queryServer(t, probe, dns.TypeA, dnsServer())
		if r.Rcode == dns.RcodeSuccess {
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("primary DNS not ready: timed out waiting for 10.1.0.1")
}

func query(t *testing.T, name string, qtype uint16) *dns.Msg {
	return queryServer(t, name, qtype, dnsServer())
}

func queryServer(t *testing.T, name string, qtype uint16, server string) *dns.Msg {
	t.Helper()

	c := new(dns.Client)
	c.Timeout = 5 * time.Second
	// Use TCP instead of UDP for better compatibility (some setups might block UDP)
	c.Net = "tcp"

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true

	r, _, err := c.Exchange(m, server)
	require.NoError(t, err, "DNS query for %s failed", name)
	return r
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

// PTRs must target the canonical name only — never an alias.
func TestPTRTargetsCanonicalOnly(t *testing.T) {
	skipUnlessNameTemplates(t)
	waitForDeviceRecords(t)

	ptrName, err := dns.ReverseAddr(tmplMgmtIP)
	require.NoError(t, err)

	r := query(t, ptrName, dns.TypePTR)
	require.Equal(t, dns.RcodeSuccess, r.Rcode)
	require.NotEmpty(t, r.Answer)

	for _, ans := range r.Answer {
		ptr, ok := ans.(*dns.PTR)
		require.True(t, ok)
		require.Equal(t, dns.Fqdn(tmplCanonical), ptr.Ptr,
			"PTR must target the canonical FQDN, never an alias")
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
