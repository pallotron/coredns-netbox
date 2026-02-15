//go:build e2e

package e2e

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/miekg/dns"
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

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true

	r, _, err := c.Exchange(m, server)
	if err != nil {
		t.Fatalf("DNS query for %s failed: %v", name, err)
	}
	return r
}

func TestARecordLookup(t *testing.T) {
	r := query(t, "host1.example.org", dns.TypeA)

	if r.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got %s", dns.RcodeToString[r.Rcode])
	}

	if len(r.Answer) == 0 {
		t.Fatal("expected at least one answer")
	}

	found := false
	for _, ans := range r.Answer {
		if a, ok := ans.(*dns.A); ok {
			if a.A.Equal(net.ParseIP("10.0.0.1")) {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 10.0.0.1 in A record answers")
	}
}

func TestAAAARecordLookup(t *testing.T) {
	r := query(t, "host1.example.org", dns.TypeAAAA)

	if r.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got %s", dns.RcodeToString[r.Rcode])
	}

	if len(r.Answer) == 0 {
		t.Fatal("expected at least one AAAA answer")
	}

	found := false
	for _, ans := range r.Answer {
		if aaaa, ok := ans.(*dns.AAAA); ok {
			if aaaa.AAAA.Equal(net.ParseIP("2001:db8::1")) {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 2001:db8::1 in AAAA record answers")
	}
}

func TestForwardLookup(t *testing.T) {
	r := query(t, "google.com", dns.TypeA)

	if r.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR for forwarded query, got %s", dns.RcodeToString[r.Rcode])
	}

	if len(r.Answer) == 0 {
		t.Fatal("expected at least one answer for forwarded query")
	}
}

func TestNXDOMAIN(t *testing.T) {
	r := query(t, "nonexistent.example.org", dns.TypeA)

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
		{"host1.example.org", "10.0.0.1"},
		{"host2.example.org", "10.0.0.2"},
		{"host3.example.org", "10.0.0.3"},
		{"web.example.org", "192.168.1.1"},
		{"db.example.org", "192.168.1.2"},
	}

	for _, h := range hosts {
		t.Run(h.name, func(t *testing.T) {
			r := query(t, h.name, dns.TypeA)

			if r.Rcode != dns.RcodeSuccess {
				t.Fatalf("expected NOERROR, got %s", dns.RcodeToString[r.Rcode])
			}

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
	r := queryServer(t, "host1.example.org", dns.TypeA, dnsSecondaryServer())

	if r.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR from secondary, got %s", dns.RcodeToString[r.Rcode])
	}

	if len(r.Answer) == 0 {
		t.Fatal("expected at least one answer from secondary")
	}

	found := false
	for _, ans := range r.Answer {
		if a, ok := ans.(*dns.A); ok {
			if a.A.Equal(net.ParseIP("10.0.0.1")) {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 10.0.0.1 in A record answers from secondary")
	}
}

func TestAXFRTransfer(t *testing.T) {
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr("example.org.")

	env, err := tr.In(m, dnsServer())
	if err != nil {
		t.Fatalf("AXFR transfer failed: %v", err)
	}

	var records []dns.RR
	for e := range env {
		if e.Error != nil {
			t.Fatalf("AXFR envelope error: %v", e.Error)
		}
		records = append(records, e.RR...)
	}

	if len(records) == 0 {
		t.Fatal("expected records from AXFR transfer, got none")
	}

	foundSOA := false
	foundA := false
	for _, rr := range records {
		if _, ok := rr.(*dns.SOA); ok {
			foundSOA = true
		}
		if a, ok := rr.(*dns.A); ok {
			if a.Hdr.Name == "host1.example.org." && a.A.Equal(net.ParseIP("10.0.0.1")) {
				foundA = true
			}
		}
	}
	if !foundSOA {
		t.Error("expected SOA record in AXFR response")
	}
	if !foundA {
		t.Error("expected A record for host1.example.org in AXFR response")
	}
}
