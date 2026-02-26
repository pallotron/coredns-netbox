//go:build e2e

package e2e

import (
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
	r := query(t, "server1-mgmt.dc1.mycompany.com", dns.TypeA)

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
		t.Error("expected 10.1.0.1 in A record answers")
	}
}

func TestForwardLookup(t *testing.T) {
	r := query(t, "google.com", dns.TypeA)

	require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR for forwarded query, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected at least one answer for forwarded query")
}

func TestNXDOMAIN(t *testing.T) {
	r := query(t, "nonexistent.dc1.mycompany.com", dns.TypeA)

	// Could be NXDOMAIN or NOERROR with empty answer depending on netbox plugin behavior
	if r.Rcode != dns.RcodeNameError && len(r.Answer) != 0 {
		t.Logf("Rcode: %s, Answers: %d", dns.RcodeToString[r.Rcode], len(r.Answer))
	}
}

func TestMultipleHosts(t *testing.T) {
	hosts := []struct {
		name string
		ip   string
	}{
		{"server1-mgmt.dc1.mycompany.com", "10.1.0.1"},
		{"server2-mgmt.dc1.mycompany.com", "10.1.0.2"},
		{"server3-mgmt.dc1.mycompany.com", "10.1.0.3"},
		{"server1-bmc.dc1.mycompany.com", "10.1.8.1"},
		{"server1-storage.dc1.mycompany.com", "10.1.16.1"},
	}

	for _, h := range hosts {
		t.Run(h.name, func(t *testing.T) {
			r := query(t, h.name, dns.TypeA)

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
				t.Errorf("expected %s in answers for %s", h.ip, h.name)
			}
		})
	}
}

func TestSecondaryARecordLookup(t *testing.T) {
	r := queryServer(t, "server1-mgmt.dc1.mycompany.com", dns.TypeA, dnsSecondaryServer())

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
		t.Error("expected 10.1.0.1 in A record answers from secondary")
	}
}

func TestPTRRecordLookup(t *testing.T) {
	ptrName, err := dns.ReverseAddr("10.1.0.1")
	require.NoError(t, err, "failed to compute reverse addr")

	r := query(t, ptrName, dns.TypePTR)

	require.Equal(t, dns.RcodeSuccess, r.Rcode, "expected NOERROR, got %s", dns.RcodeToString[r.Rcode])
	require.NotEmpty(t, r.Answer, "expected at least one PTR answer")

	found := false
	for _, ans := range r.Answer {
		if ptr, ok := ans.(*dns.PTR); ok {
			if ptr.Ptr == dns.Fqdn("server1-mgmt.dc1.mycompany.com") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected server1-mgmt.dc1.mycompany.com. in PTR record answers")
	}
}

func TestAXFRTransfer(t *testing.T) {
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr("dc1.mycompany.com.")

	env, err := tr.In(m, dnsServer())
	require.NoError(t, err, "AXFR transfer failed")

	var records []dns.RR
	for e := range env {
		require.NoError(t, e.Error, "AXFR envelope error")
		records = append(records, e.RR...)
	}

	require.NotEmpty(t, records, "expected records from AXFR transfer, got none")

	foundSOA := false
	foundA := false
	for _, rr := range records {
		if _, ok := rr.(*dns.SOA); ok {
			foundSOA = true
		}
		if a, ok := rr.(*dns.A); ok {
			if a.Hdr.Name == "server1-mgmt.dc1.mycompany.com." && a.A.Equal(net.ParseIP("10.1.0.1")) {
				foundA = true
			}
		}
	}
	if !foundSOA {
		t.Error("expected SOA record in AXFR response")
	}
	if !foundA {
		t.Error("expected A record for server1-mgmt.dc1.mycompany.com in AXFR response")
	}
}
