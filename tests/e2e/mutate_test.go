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

// forcePollAndWait triggers a NetBox poll and returns only once the
// resulting zone write has completed, so a caller's own DNS budget has to
// cover just the CoreDNS reload and cache TTL rather than the whole fetch.
//
// ForceNetboxPoll is a signal, not a synchronous call. A full poll
// re-fetches the entire record set (~19k in dev), enriches it and rewrites
// the zones, and on a slow runner that alone can outlast a 25s DNS wait.
// N13 failed exactly that way in CI while the identical commit passed
// elsewhere; the giveaway was N11's own measurement moving from 10.7s to
// 15.6s between the two runs, i.e. the runner was ~45% slower, not the code
// different.
//
// LastMergeWrite is the signal to watch rather than LastNetboxPoll: the
// poll loop sets it after the write, and it still advances when the fetch
// failed or was refused (the merge then simply rewrites the previous
// zones). So this cannot wedge on a shrink-guard refusal -- though the
// failure-mode tests keep using the raw forcePoll anyway, since there a
// cycle legitimately may not land at all.
//
// Tests that MEASURE the poll path (N11) must also keep forcePoll: the
// fetch is precisely what they are timing.
func forcePollAndWait(t *testing.T) {
	t.Helper()
	conn := grpcConn(t)
	ctx := authCtx(t)
	cc := pb.NewControlServiceClient(conn)

	before, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	require.NoError(t, err, "GetStatus before ForceNetboxPoll")

	_, err = cc.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{})
	require.NoError(t, err, "ForceNetboxPoll")

	// Both timestamps come from the sidecar's own clock, so comparing them
	// to each other is safe where comparing to the test host's would not
	// be. A scheduled poll landing in the same window can satisfy this
	// early, which just degrades to the unsynchronised behaviour.
	require.Eventually(t, func() bool {
		s, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
		return err == nil && s.LastMergeWriteUnix > before.LastMergeWriteUnix
	}, 90*time.Second, 200*time.Millisecond,
		"poll cycle did not complete a zone write within 90s")
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

// cleanupConverge is the deadline for post-test deletions to converge on
// every replica. Deliberately wider than the 25s in-test budget: cleanups
// assert nothing about product latency — their only job is handing the
// next test a converged zone — and on a degraded CI node (slow I/O,
// pod-network flaps) a deletion can need a second reload cycle.
const cleanupConverge = 40 * time.Second

// waitNameGone waits until the name returns NXDOMAIN (or an empty answer)
// from every replica. Cleanups use it so each test hands the next one a
// fully converged zone — otherwise the next test reads a baseline (SOA
// serial, record set) that the previous test's deletion is about to move.
func waitNameGone(t *testing.T, name string, qtype uint16, timeout time.Duration) {
	t.Helper()
	waitAllServers(t, timeout, func(server string) bool {
		r, ok := tryQueryServer(name, qtype, server)
		if !ok {
			// Unreachable is not "gone": a replica being replaced would
			// otherwise look like a converged deletion.
			return false
		}
		return r.Rcode == dns.RcodeNameError || len(r.Answer) == 0
	}, "%s (type %d) still served after deletion", name, qtype)
}

// soaSerialOn reads one server's SOA serial, reporting not-ok instead of
// failing the test when the query cannot be completed or the zone is not
// (yet) served.
func soaSerialOn(server, zone string) (uint32, bool) {
	r, ok := tryQueryServer(zone, dns.TypeSOA, server)
	if !ok || r.Rcode != dns.RcodeSuccess {
		return 0, false
	}
	for _, ans := range r.Answer {
		if soa, ok := ans.(*dns.SOA); ok {
			return soa.Serial, true
		}
	}
	return 0, false
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
			serial, found := soaSerialOn(server, zone)
			if !found {
				complete = false
				break
			}
			serials[serial] = true
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
		forcePollAndWait(t)
		waitNameGone(t, fqdn, dns.TypeA, cleanupConverge)
	})

	// Create.
	netboxORM(t, fmt.Sprintf(`from ipam.models import IPAddress
IPAddress.objects.create(address="10.77.0.1/24", dns_name=%q, status="active")
print("created")`, dnsName))
	forcePollAndWait(t)
	waitForDNS(t, fqdn, dns.TypeA, "10.77.0.1", 25*time.Second)
	waitForPTR(t, "10.77.0.1", fqdn, 25*time.Second)

	// Change: reassign the address.
	netboxORM(t, fmt.Sprintf(`from ipam.models import IPAddress
ip = IPAddress.objects.get(dns_name=%q)
ip.address = "10.77.0.2/24"
ip.save()
print("changed")`, dnsName))
	forcePollAndWait(t)
	waitForDNS(t, fqdn, dns.TypeA, "10.77.0.2", 25*time.Second)
	waitForPTR(t, "10.77.0.2", fqdn, 25*time.Second)

	// The old A and the old PTR must be gone from BOTH replicas (nothing
	// stale). waitForDNS above is satisfied by whichever replica reloaded
	// first; the other may serve the old zone for up to reload + cache.
	waitAllServers(t, 25*time.Second, func(server string) bool {
		r, ok := tryQueryServer(fqdn, dns.TypeA, server)
		if !ok {
			return false
		}
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
	forcePollAndWait(t)
	waitNameGone(t, fqdn, dns.TypeA, 25*time.Second)
	newPTR, err := dns.ReverseAddr("10.77.0.2")
	require.NoError(t, err)
	waitNameGone(t, newPTR, dns.TypePTR, 25*time.Second)
}

// N13 (acceptance-test matrix): a 1,000-record bulk change lands in one
// poll — all records present, a single merge pass. Generator half of
// PoC A12.
func TestMutate_N13_BatchChange(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	zone := forwardZone("dc3") // mycompany.com with STRIP_DC_LABEL

	t.Cleanup(func() {
		netboxORM(t, `from ipam.models import IPAddress
deleted, _ = IPAddress.objects.filter(dns_name__startswith="bulk-n13-").delete()
print(deleted)`)
		forcePollAndWait(t)
		waitNameGone(t, hostFQDN("bulk-n13-1", "dc3"), dns.TypeA, cleanupConverge)
	})

	// Baseline only once every replica agrees — a preceding test's cleanup
	// write may still be propagating through the 10s CoreDNS reload.
	serialBefore := stableSerial(t, zone)
	// The serial cannot count writes: it is wall-clock seconds, so a single
	// write advances it by however long the batch took, not by one. Count the
	// writes themselves instead, which is what this test actually claims.
	updatesBefore, ok := metricValue(fetchMetrics(t), `netbox_sidecar_zone_writes_total{op="update"}`)
	require.True(t, ok, `netbox_sidecar_zone_writes_total{op="update"} must be exported`)

	// 1,000 records in 10.33.x — disjoint from the seed ranges.
	netboxORM(t, `from ipam.models import IPAddress
records = [IPAddress(address=f"10.33.{i // 250}.{i % 250 + 1}/24",
                     dns_name=f"bulk-n13-{i + 1}.dc3.mycompany.com",
                     status="active")
           for i in range(1000)]
IPAddress.objects.bulk_create(records, batch_size=1000)
print("bulk created", len(records))`)
	forcePollAndWait(t)

	// First and last record resolvable…
	waitForDNS(t, hostFQDN("bulk-n13-1", "dc3"), dns.TypeA, "10.33.0.1", 25*time.Second)
	waitForDNS(t, hostFQDN("bulk-n13-1000", "dc3"), dns.TypeA, "10.33.3.250", 25*time.Second)

	// …then wait for BOTH replicas to actually serve the batch before the
	// AXFR and serial checks. The waits above go through the load-balanced
	// service, so one reloaded replica satisfies them while the other is
	// still on the old zone.
	//
	// This has to be an explicit check for the new content: stableSerial
	// returns the first serial every replica AGREES on, and before any of
	// them has reloaded that agreement is on the OLD serial. It would then
	// hand back serialBefore and the assertion below would read as "no
	// zone write happened" when in fact one had, just not everywhere yet.
	waitAllServers(t, 25*time.Second, func(server string) bool {
		return answersTolerant(server, hostFQDN("bulk-n13-1", "dc3"))
	}, "every replica must serve the batch before its serial is read")
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

	// One merge pass for the whole batch, not one write per record. The
	// 1,000 addresses sit in four /24s that all fall inside the single
	// configured IPv4 reverse zone (10.in-addr.arpa, see
	// dev/coredns-netbox-values.yaml), so one pass rewrites exactly two zone
	// files: the dc3 forward zone and that reverse zone. The regression this
	// guards against — a write per record — would be ~1000.
	//
	// The periodic poll may fire between bulk_create and forcePoll, but both
	// see the same data and the content hash suppresses the redundant write,
	// so exactly one pass lands either way.
	require.NotEqual(t, serialBefore, serialAfter, "the batch must produce a zone write")
	updatesAfter, ok := metricValue(fetchMetrics(t), `netbox_sidecar_zone_writes_total{op="update"}`)
	require.True(t, ok, `netbox_sidecar_zone_writes_total{op="update"} must be exported`)
	require.Equal(t, updatesBefore+2, updatesAfter,
		"a 1000-record batch must land as ONE merge pass over the forward and reverse zones (serial %d -> %d)",
		serialBefore, serialAfter)
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
		forcePollAndWait(t)
		waitNameGone(t, fqdn, dns.TypeA, cleanupConverge)
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

// N1 (acceptance-test matrix): only status=active IPs are published — the
// FLEET-47 semantic. The whole policy is a server-side filter (the fetch
// URL's status= params, default active-only; configurable via
// NETBOX_PUBLISH_STATUSES), so this pins the end-to-end default: reserved,
// deprecated and dhcp IPs must never materialize as A or PTR records.
func TestMutate_N1_OnlyActivePublished(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	statuses := []string{"active", "reserved", "deprecated", "dhcp"}
	fqdnFor := func(status string) string { return hostFQDN("server-n1-"+status, "dc1") }

	t.Cleanup(func() {
		netboxORM(t, `from ipam.models import IPAddress
deleted, _ = IPAddress.objects.filter(dns_name__startswith="server-n1-").delete()
print(deleted)`)
		forcePollAndWait(t)
		waitNameGone(t, fqdnFor("active"), dns.TypeA, cleanupConverge)
	})

	netboxORM(t, `from ipam.models import IPAddress
for i, status in enumerate(["active", "reserved", "deprecated", "dhcp"]):
    IPAddress.objects.create(address=f"10.77.1.{i + 1}/24",
                             dns_name=f"server-n1-{status}.dc1.mycompany.com",
                             status=status)
print("created 4")`)
	forcePollAndWait(t)

	// All four rows landed before the same poll, so the zone version that
	// serves the active record is the exact zone that would contain the
	// other three if the filter were broken. Once every replica serves it,
	// single-shot negative checks are race-free.
	waitForDNS(t, fqdnFor("active"), dns.TypeA, "10.77.1.1", 25*time.Second)
	waitForPTR(t, "10.77.1.1", fqdnFor("active"), 25*time.Second)
	waitAllServers(t, 25*time.Second, func(server string) bool {
		r, ok := tryQueryServer(fqdnFor("active"), dns.TypeA, server)
		return ok && r.Rcode == dns.RcodeSuccess && len(r.Answer) > 0
	}, "active record must be served by every replica before the negative checks")

	for i, status := range statuses[1:] {
		addr := fmt.Sprintf("10.77.1.%d", i+2)
		for _, server := range dnsProbeServers() {
			r := queryServer(t, fqdnFor(status), dns.TypeA, server)
			require.Truef(t, r.Rcode == dns.RcodeNameError || len(r.Answer) == 0,
				"%s IP must not have an A record (server %s)", status, server)
			ptrName, err := dns.ReverseAddr(addr)
			require.NoError(t, err)
			rp := queryServer(t, ptrName, dns.TypePTR, server)
			require.Truef(t, rp.Rcode == dns.RcodeNameError || len(rp.Answer) == 0,
				"%s IP must not have a PTR record (server %s)", status, server)
		}
	}
}

// N6 (acceptance-test matrix), passthrough half: hostile dns_name values
// must never corrupt the zone. NetBox's API validates dns_name, but ORM
// writes (plugins, custom scripts, legacy data) bypass validation, so the
// generator is the last line of defense. The batch includes a newline
// injection: if it reached the zone file verbatim it would materialize an
// attacker-chosen record.
func TestMutate_N6_HostileDNSNames(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	canary := hostFQDN("server-n6-canary", "dc1")
	injected := hostFQDN("evil-n6-injected", "dc1")

	t.Cleanup(func() {
		netboxORM(t, `from ipam.models import IPAddress
addrs = [f"10.77.2.{i}/24" for i in range(1, 6)]
deleted, _ = IPAddress.objects.filter(address__in=addrs).delete()
print(deleted)`)
		forcePollAndWait(t)
		waitNameGone(t, canary, dns.TypeA, cleanupConverge)
	})

	// One legit canary plus the hostile names, all in one batch: once the
	// canary resolves, the post-batch zone is being served. (Backtick Go
	// string: the \n below reaches Python verbatim, which turns it into a
	// real newline inside the stored dns_name.)
	netboxORM(t, `from ipam.models import IPAddress
hostile = [
    "bad name.dc1.mycompany.com",
    "semi;colon.dc1.mycompany.com",
    "quote\"host.dc1.mycompany.com",
    "newline.dc1.mycompany.com\nevil-n6-injected.dc1.mycompany.com. IN A 192.0.2.66",
]
IPAddress.objects.create(address="10.77.2.1/24",
                         dns_name="server-n6-canary.dc1.mycompany.com", status="active")
for i, name in enumerate(hostile):
    IPAddress.objects.create(address=f"10.77.2.{i + 2}/24", dns_name=name, status="active")
print("created", 1 + len(hostile))`)
	forcePollAndWait(t)

	// The zone must still load: the canary from the same batch resolves. If
	// a hostile name wedges the reload, this is where it shows.
	waitForDNS(t, canary, dns.TypeA, "10.77.2.1", 25*time.Second)

	// The injection attempt must not have materialized a record.
	for _, server := range dnsProbeServers() {
		r := queryServer(t, injected, dns.TypeA, server)
		require.Truef(t, r.Rcode == dns.RcodeNameError || len(r.Answer) == 0,
			"newline injection must not materialize %s (server %s)", injected, server)
	}

	// And the zone as a whole still transfers and parses.
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(forwardZone("dc1")))
	env, err := tr.In(m, dnsServer())
	require.NoError(t, err, "AXFR must still work with hostile records present")
	for e := range env {
		require.NoError(t, e.Error, "AXFR envelope must parse")
	}
}
