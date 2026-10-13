//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
)

// Mutation tests change NetBox data through the Django ORM (the dev NetBox
// has no webhooks, so changes propagate only via polls — the exact path
// these rows exist to verify). They create their own records in address
// space disjoint from the seed (10.77.x / 10.78.x / 10.33.x) and clean up
// after themselves, but still run in the gated pass (CHAOS_TESTS=true)
// because they mutate shared NetBox state and take minutes.

// netboxORM runs a small Python snippet inside the NetBox pod via
// manage.py shell — the same mechanism as dev/seed-ips.py. The snippet is
// passed as a single argv element: no shell quoting pitfalls.
func netboxORM(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "--context", kubeContext(),
		"-n", "netbox", "exec", "deploy/netbox", "-c", "netbox", "--",
		"python", "/opt/netbox/netbox/manage.py", "shell",
		"--no-startup", "--no-imports", "-c", script).CombinedOutput()
	require.NoError(t, err, "netbox ORM script failed: %s", string(out))
	return string(out)
}

// forcePoll triggers a full NetBox fetch + merge + reload via gRPC and
// returns immediately; callers wait on DNS (25s budget: fetch + 10s
// CoreDNS reload + 5s cache + buffer — see grpc_test.go).
func forcePoll(t *testing.T) {
	t.Helper()
	conn := grpcConn(t)
	cc := pb.NewControlServiceClient(conn)
	_, err := cc.ForceNetboxPoll(authCtx(t), &pb.ForceNetboxPollRequest{})
	require.NoError(t, err, "ForceNetboxPoll")
}

// dnsProbeServers returns every DNS endpoint an absence check must agree
// on: both per-pod addresses when exposed (DNS_POD0/DNS_POD1, set by the
// make targets and CI), else the load-balanced service. The two CoreDNS
// replicas reload their zone files independently (up to 10s skew) and
// each has its own answer cache, so a single load-balanced query only
// proves ONE replica converged.
func dnsProbeServers() []string {
	if p0, p1 := os.Getenv("DNS_POD0"), os.Getenv("DNS_POD1"); p0 != "" && p1 != "" {
		return []string{p0, p1}
	}
	return []string{dnsServer()}
}

// waitAllServers polls until cond holds on every probe server in the same
// round, or fails the test at the deadline. Use it for every "record must
// be gone" assertion: a positive waitForDNS satisfied by the faster
// replica says nothing about the slower one.
func waitAllServers(t *testing.T, timeout time.Duration, cond func(server string) bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, server := range dnsProbeServers() {
			if !cond(server) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf(msg, args...)
}

// waitNameGone waits until the name returns NXDOMAIN (or an empty answer)
// from every replica. Cleanups use it so each test hands the next one a
// fully converged zone — otherwise the next test reads a baseline (SOA
// serial, record set) that the previous test's deletion is about to move.
func waitNameGone(t *testing.T, name string, qtype uint16, timeout time.Duration) {
	t.Helper()
	waitAllServers(t, timeout, func(server string) bool {
		r := queryServer(t, name, qtype, server)
		return r.Rcode == dns.RcodeNameError || len(r.Answer) == 0
	}, "%s (type %d) still served after deletion", name, qtype)
}

// stableSerial returns the zone's SOA serial once every replica agrees on
// it. Right after a zone write the replicas can serve different serials
// for up to a reload interval; reading just one risks a stale baseline.
func stableSerial(t *testing.T, zone string) uint32 {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		serials := map[uint32]bool{}
		complete := true
		for _, server := range dnsProbeServers() {
			r := queryServer(t, dns.Fqdn(zone), dns.TypeSOA, server)
			found := false
			if r.Rcode == dns.RcodeSuccess {
				for _, ans := range r.Answer {
					if soa, ok := ans.(*dns.SOA); ok {
						serials[soa.Serial] = true
						found = true
					}
				}
			}
			if !found {
				complete = false
				break
			}
		}
		if complete && len(serials) == 1 {
			for s := range serials {
				return s
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("SOA serial for %s did not converge across %v", zone, dnsProbeServers())
	return 0
}

// N12 (acceptance-test matrix): change & delete through the poll path —
// the generator half of PoC A3/A4. Reassigning an IP updates both the A
// and the PTR; deleting the IP removes both; nothing stale survives the
// following poll.
func TestMutate_N12_ChangeAndDelete(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	fqdn := hostFQDN("server-n12", "dc1")
	dnsName := "server-n12.dc1.mycompany.com" // ORM value keeps the DC label; STRIP_DC_LABEL removes it

	t.Cleanup(func() {
		netboxORM(t, `from ipam.models import IPAddress
deleted, _ = IPAddress.objects.filter(dns_name="server-n12.dc1.mycompany.com").delete()
print(deleted)`)
		forcePoll(t)
		waitNameGone(t, fqdn, dns.TypeA, 25*time.Second)
	})

	// Create.
	netboxORM(t, fmt.Sprintf(`from ipam.models import IPAddress
IPAddress.objects.create(address="10.77.0.1/24", dns_name=%q, status="active")
print("created")`, dnsName))
	forcePoll(t)
	waitForDNS(t, fqdn, dns.TypeA, "10.77.0.1", 25*time.Second)
	waitForPTR(t, "10.77.0.1", fqdn, 25*time.Second)

	// Change: reassign the address.
	netboxORM(t, fmt.Sprintf(`from ipam.models import IPAddress
ip = IPAddress.objects.get(dns_name=%q)
ip.address = "10.77.0.2/24"
ip.save()
print("changed")`, dnsName))
	forcePoll(t)
	waitForDNS(t, fqdn, dns.TypeA, "10.77.0.2", 25*time.Second)
	waitForPTR(t, "10.77.0.2", fqdn, 25*time.Second)

	// The old A and the old PTR must be gone from BOTH replicas (nothing
	// stale). waitForDNS above is satisfied by whichever replica reloaded
	// first; the other may serve the old zone for up to reload + cache.
	waitAllServers(t, 25*time.Second, func(server string) bool {
		r := queryServer(t, fqdn, dns.TypeA, server)
		for _, ans := range r.Answer {
			if a, ok := ans.(*dns.A); ok && a.A.String() == "10.77.0.1" {
				return false
			}
		}
		return true
	}, "old address 10.77.0.1 must not be served after reassignment")
	oldPTR, err := dns.ReverseAddr("10.77.0.1")
	require.NoError(t, err)
	waitNameGone(t, oldPTR, dns.TypePTR, 25*time.Second)

	// Delete: A and PTR both gone after the next poll.
	netboxORM(t, fmt.Sprintf(`from ipam.models import IPAddress
deleted, _ = IPAddress.objects.filter(dns_name=%q).delete()
print(deleted)`, dnsName))
	forcePoll(t)
	waitNameGone(t, fqdn, dns.TypeA, 25*time.Second)
	newPTR, err := dns.ReverseAddr("10.77.0.2")
	require.NoError(t, err)
	waitNameGone(t, newPTR, dns.TypePTR, 25*time.Second)
}

// N13 (acceptance-test matrix): a 1,000-record bulk change lands in one
// poll — all records present, a single serial bump. Generator half of
// PoC A12.
func TestMutate_N13_BatchChange(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	zone := forwardZone("dc3") // mycompany.com with STRIP_DC_LABEL

	t.Cleanup(func() {
		netboxORM(t, `from ipam.models import IPAddress
deleted, _ = IPAddress.objects.filter(dns_name__startswith="bulk-n13-").delete()
print(deleted)`)
		forcePoll(t)
		waitNameGone(t, hostFQDN("bulk-n13-1", "dc3"), dns.TypeA, 25*time.Second)
	})

	// Baseline only once every replica agrees — a preceding test's cleanup
	// write may still be propagating through the 10s CoreDNS reload.
	serialBefore := stableSerial(t, zone)

	// 1,000 records in 10.33.x — disjoint from the seed ranges.
	netboxORM(t, `from ipam.models import IPAddress
records = [IPAddress(address=f"10.33.{i // 250}.{i % 250 + 1}/24",
                     dns_name=f"bulk-n13-{i + 1}.dc3.mycompany.com",
                     status="active")
           for i in range(1000)]
IPAddress.objects.bulk_create(records, batch_size=1000)
print("bulk created", len(records))`)
	forcePoll(t)

	// First and last record resolvable…
	waitForDNS(t, hostFQDN("bulk-n13-1", "dc3"), dns.TypeA, "10.33.0.1", 25*time.Second)
	waitForDNS(t, hostFQDN("bulk-n13-1000", "dc3"), dns.TypeA, "10.33.3.250", 25*time.Second)

	// …then wait for BOTH replicas to converge before the AXFR and serial
	// checks — the load-balanced query can otherwise hit the replica that
	// has not reloaded yet.
	serialAfter := stableSerial(t, zone)

	// ALL 1000 present via AXFR.
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(zone))
	env, err := tr.In(m, dnsServer())
	require.NoError(t, err, "AXFR of %s", zone)
	count := 0
	for e := range env {
		require.NoError(t, e.Error, "AXFR envelope error")
		for _, rr := range e.RR {
			if strings.HasPrefix(rr.Header().Name, "bulk-n13-") {
				count++
			}
		}
	}
	require.Equal(t, 1000, count, "all 1000 bulk records must be in the zone")

	// Single serial bump for the whole batch. The periodic poll may fire
	// between bulk_create and forcePoll, but both see the same data, so
	// exactly one zone write happens either way.
	require.Equal(t, serialBefore+1, serialAfter,
		"a 1000-record batch must land as ONE zone write (serial %d -> %d)", serialBefore, serialAfter)
}

// N11 (acceptance-test matrix), poll half: a freshly created NetBox IP is
// resolvable within one poll + reload. The webhook fast-path half is
// already covered by TestWebhook_CreatedEventUpdatesDNSWithoutPoll; here
// the webhook path is absent by construction (ORM write, no webhooks in
// the dev NetBox), so the measured latency is purely poll + generate +
// reload + cache.
func TestMutate_N11_PollPathCreateTiming(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	fqdn := hostFQDN("server-n11", "dc1")

	t.Cleanup(func() {
		netboxORM(t, `from ipam.models import IPAddress
deleted, _ = IPAddress.objects.filter(dns_name="server-n11.dc1.mycompany.com").delete()
print(deleted)`)
		forcePoll(t)
		waitNameGone(t, fqdn, dns.TypeA, 25*time.Second)
	})

	netboxORM(t, `from ipam.models import IPAddress
IPAddress.objects.create(address="10.78.0.1/24", dns_name="server-n11.dc1.mycompany.com", status="active")
print("created")`)

	start := time.Now()
	forcePoll(t)
	waitForDNS(t, fqdn, dns.TypeA, "10.78.0.1", 25*time.Second)
	elapsed := time.Since(start)

	t.Logf("N11 poll-path create-to-resolvable: %s (budget: fetch + 10s reload + 5s cache)", elapsed)
	require.Less(t, elapsed, 25*time.Second, "create must be resolvable within one poll + reload")
}
