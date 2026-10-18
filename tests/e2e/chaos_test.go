//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
)

// Chaos tests mutate cluster state (scale NetBox to zero, delete the sidecar
// pod) and take several minutes each, so they only run when explicitly asked
// for: CHAOS_TESTS=true (make test.e2e.chaos / CI). They restore state via
// t.Cleanup even on failure.

func requireChaos(t *testing.T) {
	t.Helper()
	if os.Getenv("CHAOS_TESTS") != "true" {
		t.Skip("CHAOS_TESTS not set — skipping chaos test (use make test.e2e.chaos)")
	}
}

func kubeContext() string {
	if v := os.Getenv("E2E_KUBE_CONTEXT"); v != "" {
		return v
	}
	return "k3d-coredns-netbox"
}

func kubectlOut(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"--context", kubeContext()}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), string(out))
	return string(out)
}

func scaleDeploy(t *testing.T, ns, deploy string, replicas int) {
	t.Helper()
	kubectlOut(t, "-n", ns, "scale", "deploy/"+deploy, fmt.Sprintf("--replicas=%d", replicas))
}

func waitDeployReady(t *testing.T, ns, deploy string, timeout time.Duration) {
	t.Helper()
	kubectlOut(t, "-n", ns, "rollout", "status", "deploy/"+deploy, fmt.Sprintf("--timeout=%s", timeout))
}

// waitPodRunning waits for a pod matching selector to reach phase Running.
// Unlike rollout status it does NOT require readiness — a sidecar restarted
// while NetBox is down stays Running-but-NotReady by design (readiness is
// gated on the first successful fetch+merge).
func waitPodRunning(t *testing.T, ns, selector string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("kubectl", "--context", kubeContext(), "-n", ns,
			"get", "pod", "-l", selector, "-o", "jsonpath={.items[*].status.phase}").CombinedOutput()
		if err == nil && strings.Contains(string(out), "Running") && !strings.Contains(string(out), "Pending") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("pod with selector %q in %s not Running within %s", selector, ns, timeout)
}

// dnsProbe queries the canonical seeded record at a fixed rate on a background
// goroutine and records any answer that is not NOERROR-with-10.1.0.1. No
// testify calls happen on the goroutine (unsafe); failures are counted and
// asserted after Stop.
type dnsProbe struct {
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	total    int
	failures int
	first    string
}

type probeStats struct {
	Total        int
	Failures     int
	FirstFailure string
}

func startProbe(t *testing.T, interval time.Duration) *dnsProbe {
	t.Helper()
	return startProbeAgainst(t, interval, dnsServer())
}

// startProbeAgainst is startProbe aimed at a specific server — the secondary
// (N29) has to be watched while the primary it transfers from is down.
func startProbeAgainst(t *testing.T, interval time.Duration, server string) *dnsProbe {
	t.Helper()
	p := &dnsProbe{stop: make(chan struct{}), done: make(chan struct{})}
	name := dns.Fqdn(hostFQDN("server1-mgmt", "dc1"))
	want := "10.1.0.1"
	go func() {
		defer close(p.done)
		c := &dns.Client{Net: "tcp", Timeout: 3 * time.Second}
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				m := new(dns.Msg)
				m.SetQuestion(name, dns.TypeA)
				r, _, err := c.Exchange(m, server)
				ok := false
				if err == nil && r.Rcode == dns.RcodeSuccess {
					for _, ans := range r.Answer {
						if a, isA := ans.(*dns.A); isA && a.A.String() == want {
							ok = true
						}
					}
				}
				p.mu.Lock()
				p.total++
				if !ok {
					p.failures++
					if p.first == "" {
						if err != nil {
							p.first = fmt.Sprintf("%s: %v", time.Now().Format(time.RFC3339), err)
						} else {
							p.first = fmt.Sprintf("%s: rcode=%s answers=%d",
								time.Now().Format(time.RFC3339), dns.RcodeToString[r.Rcode], len(r.Answer))
						}
					}
				}
				p.mu.Unlock()
			}
		}
	}()
	return p
}

// FailuresSoFar reads the running failure count without stopping the probe
// — used to assert mid-outage, while the probe keeps covering recovery.
func (p *dnsProbe) FailuresSoFar() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures
}

// waitForDNSOn is waitForDNS against an explicit server (the secondary, in
// N29's case, which must be queried directly rather than via the primary's
// load-balanced service).
func waitForDNSOn(t *testing.T, server, name, wantIP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := queryServer(t, name, dns.TypeA, server)
		for _, ans := range r.Answer {
			if a, ok := ans.(*dns.A); ok && a.A.String() == wantIP {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s -> %s did not appear on %s within %s", name, wantIP, server, timeout)
}

// axfrNames transfers a zone and returns every owner name in it. A zone
// that transfers at all proves the file parsed; the names prove it is the
// zone we expect rather than an empty shell.
func axfrNames(t *testing.T, zone string) []string {
	t.Helper()
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(zone))
	env, err := tr.In(m, dnsServer())
	require.NoError(t, err, "AXFR of %s", zone)
	var names []string
	for e := range env {
		require.NoError(t, e.Error, "AXFR envelope error")
		for _, rr := range e.RR {
			names = append(names, rr.Header().Name)
		}
	}
	return names
}

func (p *dnsProbe) Stop() probeStats {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return probeStats{Total: p.total, Failures: p.failures, FirstFailure: p.first}
}

// metricValue scans a Prometheus text exposition body for the first line
// starting with seriesPrefix (name, or name{labels}) and returns its value.
func metricValue(body, seriesPrefix string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, seriesPrefix) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func soaSerial(t *testing.T, zone string) uint32 {
	t.Helper()
	r := query(t, dns.Fqdn(zone), dns.TypeSOA)
	require.Equal(t, dns.RcodeSuccess, r.Rcode, "SOA query for %s", zone)
	for _, ans := range r.Answer {
		if soa, ok := ans.(*dns.SOA); ok {
			return soa.Serial
		}
	}
	t.Fatalf("no SOA record in answer for %s", zone)
	return 0
}

func pollInterval() time.Duration {
	if v := os.Getenv("E2E_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 60 * time.Second
}

// N17 (acceptance-test matrix): NetBox down mid-run. Queries keep serving
// from the last good zones, zone_staleness_seconds climbs and
// poll_total{result="error"} increments while NetBox is away, and the first
// poll after recovery resets staleness with no restart. Component analog of
// PoC A9.
func TestChaos_N17_NetboxDownMidRun(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	// Restore NetBox no matter how the test exits.
	t.Cleanup(func() {
		scaleDeploy(t, "netbox", "netbox", 1)
		waitDeployReady(t, "netbox", "netbox", 10*time.Minute)
	})

	before := fetchMetrics(t)
	errBefore, _ := metricValue(before, `netbox_sidecar_poll_total{result="error"}`)

	probe := startProbe(t, 500*time.Millisecond)

	scaleDeploy(t, "netbox", "netbox", 0)

	// Outlast at least two poll intervals so ≥2 polls fail.
	time.Sleep(2*pollInterval() + pollInterval()/2)

	during := fetchMetrics(t)
	errDuring, ok := metricValue(during, `netbox_sidecar_poll_total{result="error"}`)
	require.True(t, ok, "poll_total{result=\"error\"} series must exist during the outage")
	require.Greater(t, errDuring, errBefore, "failed polls must be counted during the outage")
	staleness, ok := metricValue(during, "netbox_sidecar_zone_staleness_seconds")
	require.True(t, ok, "zone_staleness_seconds series must exist")
	require.Greater(t, staleness, 0.0, "staleness must climb while NetBox is down")

	// Recover and wait for the next successful poll to reset staleness.
	scaleDeploy(t, "netbox", "netbox", 1)
	waitDeployReady(t, "netbox", "netbox", 10*time.Minute)

	recoveryDeadline := time.Now().Add(3*pollInterval() + 2*time.Minute)
	recovered := false
	for time.Now().Before(recoveryDeadline) {
		after := fetchMetrics(t)
		if s, ok := metricValue(after, "netbox_sidecar_zone_staleness_seconds"); ok && s == 0 {
			recovered = true
			break
		}
		time.Sleep(5 * time.Second)
	}

	stats := probe.Stop()
	require.True(t, recovered, "staleness must reset after NetBox returns, without a sidecar restart")
	require.Greater(t, stats.Total, 0, "probe must have run")
	require.Zero(t, stats.Failures,
		"DNS must keep answering through the whole outage; first failure: %s (of %d queries)",
		stats.FirstFailure, stats.Total)
}

// N22 (acceptance-test matrix): killing the sidecar must not disturb DNS —
// CoreDNS replicas keep serving their zones while the replacement sidecar
// (strategy Recreate) comes back and re-fetches.
//
// Serial note (gap G5, decision pending): the replacement's generators start
// with an empty content hash, so today the first Generate after a restart
// rewrites every zone with serial+1 and re-transfers to all secondaries.
// The serial delta is logged here, not asserted, until G5 is decided.
func TestChaos_N22_SidecarKilled(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	zone := forwardZone("dc1")
	serialBefore := soaSerial(t, zone)

	probe := startProbe(t, 500*time.Millisecond)

	kubectlOut(t, "-n", "coredns-netbox", "delete", "pod",
		"-l", "app.kubernetes.io/component=sidecar", "--wait=false")
	waitDeployReady(t, "coredns-netbox", "coredns-netbox-sidecar", 5*time.Minute)

	// Give the replacement one fetch+merge cycle.
	time.Sleep(pollInterval() + 30*time.Second)

	stats := probe.Stop()
	require.Greater(t, stats.Total, 0, "probe must have run")
	require.Zero(t, stats.Failures,
		"DNS must keep answering across a sidecar restart; first failure: %s (of %d queries)",
		stats.FirstFailure, stats.Total)

	serialAfter := soaSerial(t, zone)
	require.GreaterOrEqual(t, serialAfter, serialBefore, "serial must never regress (RFC 1982)")
	// G5 (fixed): the replacement sidecar seeds its content fingerprint
	// from the zone file, so an unchanged record set must NOT be rewritten
	// — a restart no longer bumps every serial and fans out a full AXFR
	// re-transfer to every secondary. This was a Logf measurement while
	// the decision was pending; now it is the pin.
	require.Equal(t, serialBefore, serialAfter,
		"sidecar restart must not rewrite an unchanged zone (G5): serial %d -> %d", serialBefore, serialAfter)
}

// N16 (acceptance-test matrix): NetBox down when the sidecar starts. The
// replacement sidecar must come up Running (not CrashLoopBackOff), must NOT
// destroy the cached zone files (gap G4 — pre-fix it merged an empty NetBox
// set and removeOrphans wiped the PVC), and DNS must answer throughout. The
// pod stays NotReady until NetBox returns — that is by design (readiness
// gates on the first successful fetch+merge).
func TestChaos_N16_NetboxDownAtSidecarStartup(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	sidecarSel := "app.kubernetes.io/component=sidecar"

	t.Cleanup(func() {
		scaleDeploy(t, "netbox", "netbox", 1)
		waitDeployReady(t, "netbox", "netbox", 10*time.Minute)
		waitDeployReady(t, "coredns-netbox", "coredns-netbox-sidecar", 10*time.Minute)
	})

	probe := startProbe(t, 500*time.Millisecond)

	// NetBox gone first, THEN restart the sidecar: the replacement starts
	// with a failing initial fetch and only the PVC cache to serve.
	scaleDeploy(t, "netbox", "netbox", 0)
	time.Sleep(10 * time.Second)
	kubectlOut(t, "-n", "coredns-netbox", "delete", "pod", "-l", sidecarSel, "--wait=false")

	waitPodRunning(t, "coredns-netbox", sidecarSel, 3*time.Minute)

	// Hold the degraded state across a poll interval: the replacement must
	// neither crash-loop nor wipe the cache (the probe would catch a wipe
	// the moment a reload pushed empty zones to CoreDNS).
	time.Sleep(pollInterval() + 30*time.Second)

	restarts := kubectlOut(t, "-n", "coredns-netbox", "get", "pod", "-l", sidecarSel,
		"-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}")
	require.Equal(t, "0", strings.TrimSpace(restarts),
		"replacement sidecar must not crash-loop while NetBox is down")

	// Recovery: NetBox back -> sidecar becomes Ready and polls succeed.
	scaleDeploy(t, "netbox", "netbox", 1)
	waitDeployReady(t, "netbox", "netbox", 10*time.Minute)
	waitDeployReady(t, "coredns-netbox", "coredns-netbox-sidecar", 10*time.Minute)

	stats := probe.Stop()
	require.Greater(t, stats.Total, 0, "probe must have run")
	require.Zero(t, stats.Failures,
		"DNS must answer through sidecar restart + NetBox outage + recovery; first failure: %s (of %d queries)",
		stats.FirstFailure, stats.Total)
}

// N19 (acceptance-test matrix): a revoked API token surfaces as poll
// errors; the last good zones keep serving (N17 semantics); recovery after
// the token is restored needs no restart. The dev NetBox issues v2 tokens
// (HMAC-hashed at rest — the plaintext cannot be recreated after a
// delete), so revocation is an expiry flip rather than a row delete: the
// sidecar sees the same HTTP 403 either way.
func TestChaos_N19_TokenRevoked(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	// Restore every token no matter how the test exits.
	t.Cleanup(func() {
		netboxORM(t, `from users.models import Token
print("restored", Token.objects.update(expires=None))`)
		forcePoll(t)
	})

	before := fetchMetrics(t)
	errBefore, _ := metricValue(before, `netbox_sidecar_poll_total{result="error"}`)

	probe := startProbe(t, 500*time.Millisecond)

	netboxORM(t, `import datetime
from django.utils import timezone
from users.models import Token
print("expired", Token.objects.update(expires=timezone.now() - datetime.timedelta(days=1)))`)
	forcePoll(t)

	// The failed poll must be counted while answers keep coming from the
	// last good zones.
	deadline := time.Now().Add(pollInterval() + time.Minute)
	counted := false
	for time.Now().Before(deadline) {
		during := fetchMetrics(t)
		if v, ok := metricValue(during, `netbox_sidecar_poll_total{result="error"}`); ok && v > errBefore {
			counted = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	require.True(t, counted, "revoked token must surface as poll errors (error count still at %v)", errBefore)

	// Restore and verify recovery without a restart: the next successful
	// poll resets staleness.
	netboxORM(t, `from users.models import Token
print("restored", Token.objects.update(expires=None))`)
	forcePoll(t)

	recoveryDeadline := time.Now().Add(pollInterval() + 2*time.Minute)
	recovered := false
	for time.Now().Before(recoveryDeadline) {
		after := fetchMetrics(t)
		if s, ok := metricValue(after, "netbox_sidecar_zone_staleness_seconds"); ok && s == 0 {
			recovered = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	stats := probe.Stop()
	require.True(t, recovered, "polls must succeed again after the token is restored, without a restart")
	require.Greater(t, stats.Total, 0, "probe must have run")
	require.Zero(t, stats.Failures,
		"DNS must keep answering while the token is revoked; first failure: %s (of %d queries)",
		stats.FirstFailure, stats.Total)
}

// scaleStatefulSet scales a StatefulSet and waits for the rollout to settle.
func scaleStatefulSet(t *testing.T, ns, name string, replicas int) {
	t.Helper()
	kubectlOut(t, "-n", ns, "scale", "sts/"+name, fmt.Sprintf("--replicas=%d", replicas))
}

// waitStatefulSetReady blocks until every replica of the StatefulSet is ready.
func waitStatefulSetReady(t *testing.T, ns, name string, timeout time.Duration) {
	t.Helper()
	kubectlOut(t, "-n", ns, "rollout", "status", "sts/"+name, fmt.Sprintf("--timeout=%s", timeout))
}

// N24 (acceptance-test matrix): a rolling restart of the CoreDNS primary
// must not drop client queries. This is not an exotic failure mode — every
// `helm upgrade` performs exactly this, so if readiness lets a pod take
// traffic before its zones are loaded, the deploy IS the incident. With two
// replicas behind the service, the surviving pod must cover each pod's
// restart window.
func TestChaos_N24_RollingRestartUnderLoad(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	t.Cleanup(func() {
		waitStatefulSetReady(t, "coredns-netbox", "coredns-netbox", 10*time.Minute)
	})

	// 10 queries/second against the load-balanced service for the whole
	// rollout — the per-pod ports would dodge exactly the failover this
	// test exists to measure.
	probe := startProbe(t, 100*time.Millisecond)

	kubectlOut(t, "-n", "coredns-netbox", "rollout", "restart", "sts/coredns-netbox")
	waitStatefulSetReady(t, "coredns-netbox", "coredns-netbox", 10*time.Minute)

	// Let the probe cover the settling period after the last pod reports
	// ready, where a premature readiness signal would surface.
	time.Sleep(10 * time.Second)

	stats := probe.Stop()
	require.Greater(t, stats.Total, 100, "probe must have run across the rollout")
	require.Zero(t, stats.Failures,
		"rolling restart must not drop queries; first failure: %s (of %d queries)",
		stats.FirstFailure, stats.Total)
}

// N29 (acceptance-test matrix): the secondary keeps answering while the
// primary it transfers from is entirely gone, and resumes transferring when
// the primary returns — no restart, no manual intervention. Component
// analog of PoC A9: under CISv2 every site secondary depends on exactly
// this when Central is unreachable.
func TestChaos_N29_SecondaryServesThroughPrimaryOutage(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	// The secondary must already hold the zone before the primary goes
	// away; a secondary that never transferred proves nothing.
	waitForDNSOn(t, dnsSecondaryServer(), hostFQDN("server1-mgmt", "dc1"), "10.1.0.1", 60*time.Second)

	t.Cleanup(func() {
		scaleStatefulSet(t, "coredns-netbox", "coredns-netbox", 2)
		waitStatefulSetReady(t, "coredns-netbox", "coredns-netbox", 10*time.Minute)
	})

	probe := startProbeAgainst(t, 500*time.Millisecond, dnsSecondaryServer())

	scaleStatefulSet(t, "coredns-netbox", "coredns-netbox", 0)
	kubectlOut(t, "-n", "coredns-netbox", "wait", "--for=delete",
		"pod", "-l", "app.kubernetes.io/component=primary", "--timeout=5m")

	// Outlast a refresh window so the secondary actually attempts (and
	// fails) a transfer while serving from its own copy. SOA expire is
	// days, so nothing should be dropped here.
	time.Sleep(90 * time.Second)

	// Primary is gone: it must be the secondary answering, not a stale
	// route to the primary's service.
	require.Zero(t, probe.FailuresSoFar(), "secondary must answer throughout the primary outage")

	scaleStatefulSet(t, "coredns-netbox", "coredns-netbox", 2)
	waitStatefulSetReady(t, "coredns-netbox", "coredns-netbox", 10*time.Minute)
	waitForPrimary(t)

	// Recovery: a change made after the outage must reach the secondary
	// again, proving transfers resumed without intervention.
	stats := probe.Stop()
	require.Greater(t, stats.Total, 0, "probe must have run")
	require.Zero(t, stats.Failures,
		"secondary must answer throughout and after the outage; first failure: %s (of %d queries)",
		stats.FirstFailure, stats.Total)
	waitForDNSOn(t, dnsSecondaryServer(), hostFQDN("server1-mgmt", "dc1"), "10.1.0.1", 2*time.Minute)
}

// N23 (acceptance-test matrix): losing the sidecar's PVC must be survivable
// — the replacement regenerates every zone from NetBox within one poll
// cycle. The PVC exists to serve stale across restarts (G4/N16); this is
// the opposite case, where there is nothing cached and NetBox is healthy,
// and it is also what a fresh install does.
func TestChaos_N23_SidecarPVCLost(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	zone := forwardZone("dc1")
	serialBefore := soaSerial(t, zone)

	// Scale to 0 first: the PVC is RWO and cannot be deleted while mounted.
	scaleDeploy(t, "coredns-netbox", "coredns-netbox-sidecar", 0)
	kubectlOut(t, "-n", "coredns-netbox", "wait", "--for=delete",
		"pod", "-l", "app.kubernetes.io/component=sidecar", "--timeout=5m")

	t.Cleanup(func() {
		scaleDeploy(t, "coredns-netbox", "coredns-netbox-sidecar", 1)
		waitDeployReady(t, "coredns-netbox", "coredns-netbox-sidecar", 10*time.Minute)
	})

	// Capture the claim's shape before destroying it: the Deployment mounts
	// this PVC by name, so "losing the volume" means deleting it AND
	// providing a fresh empty one — otherwise the replacement pod simply
	// never schedules and the test would measure nothing.
	const pvcName = "coredns-netbox-sidecar-data"
	storageClass := strings.TrimSpace(kubectlOut(t, "-n", "coredns-netbox", "get", "pvc", pvcName,
		"-o", "jsonpath={.spec.storageClassName}"))
	size := strings.TrimSpace(kubectlOut(t, "-n", "coredns-netbox", "get", "pvc", pvcName,
		"-o", "jsonpath={.spec.resources.requests.storage}"))
	require.NotEmpty(t, size, "must read the existing PVC size")

	kubectlOut(t, "-n", "coredns-netbox", "delete", "pvc", pvcName, "--timeout=5m")

	manifest := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: coredns-netbox
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: %s
`, pvcName, size)
	if storageClass != "" {
		manifest += fmt.Sprintf("  storageClassName: %s\n", storageClass)
	}
	manifestPath := filepath.Join(t.TempDir(), "pvc.yaml")
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o644))
	kubectlOut(t, "-n", "coredns-netbox", "apply", "-f", manifestPath)

	start := time.Now()
	scaleDeploy(t, "coredns-netbox", "coredns-netbox-sidecar", 1)
	waitDeployReady(t, "coredns-netbox", "coredns-netbox-sidecar", 10*time.Minute)

	// Readiness gates on the first successful fetch+merge, so by here the
	// zones have been regenerated from NetBox onto a brand-new volume.
	waitForDNS(t, hostFQDN("server1-mgmt", "dc1"), dns.TypeA, "10.1.0.1", 2*time.Minute)
	t.Logf("N23 PVC-loss recovery: %s from scale-up to serving", time.Since(start).Round(time.Second))

	// The zone must come back COMPLETE, not merely present.
	require.NotEmpty(t, axfrNames(t, zone), "zone must transfer after PVC loss")
	waitForPTR(t, "10.1.0.1", hostFQDN("server1-mgmt", "dc1"), 60*time.Second)

	// Serial continuity is lost with the volume (the file carried it), so
	// the only safe assertion is that a serial exists and the zone serves.
	serialAfter := soaSerial(t, zone)
	t.Logf("N23 serial across PVC loss: %d -> %d (continuity is file-backed; a rebuilt volume restarts the sequence)",
		serialBefore, serialAfter)
	require.Positive(t, serialAfter, "regenerated zone must carry a serial")
}

// helmDeploy re-runs the dev deployment command, optionally with extra
// --set overrides. The chart path is relative to tests/e2e (go test runs
// with the package directory as its working directory).
func helmDeploy(t *testing.T, extra ...string) {
	t.Helper()
	args := []string{
		"upgrade", "--install", "coredns-netbox", "../../helm/coredns-netbox",
		"-n", "coredns-netbox",
		"-f", "../../dev/coredns-netbox-values.yaml",
		"--set", "netbox.existingSecret=netbox-api-token",
		"--kube-context", kubeContext(),
		"--wait", "--timeout", "5m",
	}
	args = append(args, extra...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	require.NoError(t, err, "helm %s: %s", strings.Join(args, " "), string(out))
}

// axfrSucceeds reports whether a full zone transfer completes from this
// host. A refused transfer surfaces either as an error from the transfer
// itself or as a failing envelope, depending on how the server declines —
// both count as denied.
func axfrSucceeds(t *testing.T, zone string) bool {
	t.Helper()
	tr := new(dns.Transfer)
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(zone))
	env, err := tr.In(m, dnsServer())
	if err != nil {
		t.Logf("N30 AXFR of %s: transfer setup failed: %v", zone, err)
		return false
	}
	records := 0
	for e := range env {
		if e.Error != nil {
			t.Logf("N30 AXFR of %s: envelope error: %v", zone, e.Error)
			return false
		}
		records += len(e.RR)
	}
	return records > 0
}

// N30 (acceptance-test matrix): a restricted transfer ACL must actually
// refuse unlisted clients. Dev and CI run transfer.to: ['*'], so the deny
// path has never once executed here — yet CISv2 relies on it to keep zone
// data from transferring to anything that asks. This restricts the ACL to
// the secondary alone, confirms an unlisted client is refused while the
// listed one still works, and restores the open ACL afterwards.
func TestChaos_N30_TransferACLEnforced(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	zone := forwardZone("dc1")
	require.True(t, axfrSucceeds(t, zone), "baseline: AXFR must work with the open dev ACL")

	// The primary sees the secondary's AXFR arrive from its pod IP, so that
	// is what has to be allow-listed.
	secondaryIP := strings.TrimSpace(kubectlOut(t, "-n", "coredns-netbox", "get", "pod",
		"-l", "app.kubernetes.io/component=secondary",
		"-o", "jsonpath={.items[0].status.podIP}"))
	require.NotEmpty(t, secondaryIP, "secondary pod IP must be readable")

	// Restore the open ACL no matter how this exits — later tests and reruns
	// depend on transfers working.
	t.Cleanup(func() {
		helmDeploy(t)
		waitStatefulSetReady(t, "coredns-netbox", "coredns-netbox", 10*time.Minute)
		waitForPrimary(t)
	})

	helmDeploy(t, "--set", "transfer.to[0]="+secondaryIP)
	waitStatefulSetReady(t, "coredns-netbox", "coredns-netbox", 10*time.Minute)
	waitForPrimary(t)

	// This host is not the secondary, so its transfer must be refused —
	// while ordinary queries keep working (the ACL governs transfers only).
	require.False(t, axfrSucceeds(t, zone),
		"an unlisted client must not be able to transfer the zone")
	waitForDNS(t, hostFQDN("server1-mgmt", "dc1"), dns.TypeA, "10.1.0.1", 25*time.Second)

	// The allow-listed secondary still serves the zone it transferred.
	waitForDNSOn(t, dnsSecondaryServer(), hostFQDN("server1-mgmt", "dc1"), "10.1.0.1", 60*time.Second)
}

// waitMetric polls the sidecar's /metrics until the named series reaches
// want, or fails at the deadline. Used for state the guard exposes rather
// than for counters, which are compared before/after instead.
func waitMetric(t *testing.T, seriesPrefix string, want float64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last float64
	var seen bool
	for time.Now().Before(deadline) {
		if v, ok := metricValue(fetchMetrics(t), seriesPrefix); ok {
			last, seen = v, true
			if v == want {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s did not reach %v within %s (last seen: %v, present: %v)",
		seriesPrefix, want, timeout, last, seen)
}

// forcePollAllowShrink triggers a NetBox poll carrying single-use operator
// consent for the shrink guard — the documented way to clear a refusal.
func forcePollAllowShrink(t *testing.T) {
	t.Helper()
	cc := pb.NewControlServiceClient(grpcConn(t))
	_, err := cc.ForceNetboxPoll(authCtx(t), &pb.ForceNetboxPollRequest{AllowShrink: true})
	require.NoError(t, err, "ForceNetboxPoll(allow_shrink)")
}

// massDeprecate flips the given share of the seeded server* records to
// deprecated. With the default publish policy (active only) the sidecar's
// next fetch sees them gone — a reversible stand-in for a mass deletion
// that leaves no chance of losing dev data.
func massDeprecate(t *testing.T, percent int) {
	t.Helper()
	out := netboxORM(t, fmt.Sprintf(`
from ipam.models import IPAddress
ids = list(IPAddress.objects.filter(dns_name__startswith="server", status="active").order_by("id").values_list("id", flat=True))
cut = len(ids) * %d // 100
print("deprecated", IPAddress.objects.filter(id__in=ids[:cut]).update(status="deprecated"), "of", len(ids))
`, percent))
	t.Logf("mass-deprecate: %s", strings.TrimSpace(out))
}

func massRestore(t *testing.T) {
	t.Helper()
	out := netboxORM(t, `
from ipam.models import IPAddress
print("restored", IPAddress.objects.filter(dns_name__startswith="server", status="deprecated").update(status="active"))
`)
	t.Logf("mass-restore: %s", strings.TrimSpace(out))
}

// waitZoneAnswering waits until the zone answers an SOA at all. The bare
// query helpers fail the test on a transport error, and the first query
// after a helm redeploy (N30's cleanup runs one immediately before N21) can
// still hit a closed connection through the k3d load balancer even after the
// StatefulSet reports ready — the endpoint is reachable a moment before it
// serves.
func waitZoneAnswering(t *testing.T, zone string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		c := &dns.Client{Timeout: 5 * time.Second, Net: "tcp"}
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(zone), dns.TypeSOA)
		r, _, err := c.Exchange(m, dnsServer())
		if err == nil && r.Rcode == dns.RcodeSuccess && len(r.Answer) > 0 {
			return
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	t.Fatalf("zone %s did not answer SOA within %s (last error: %v)", zone, timeout, lastErr)
}

// answersTolerant reports whether server answers name, treating a transport
// error as "not yet" instead of failing the test. The hard-failing query
// helpers are wrong inside a convergence wait: a pod being replaced closes
// connections, which is a state to wait through, not to fail on.
func answersTolerant(server, name string) bool {
	c := &dns.Client{Timeout: 5 * time.Second, Net: "tcp"}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := c.Exchange(m, server)
	return err == nil && r.Rcode == dns.RcodeSuccess && len(r.Answer) > 0
}

// N21 (acceptance-test matrix) / gap G1: a NetBox fetch that succeeds but
// returns far fewer records than are being served must not publish. The
// zones stay exactly as they are — same SOA serial, same answers, no NOTIFY
// to the secondaries — until an operator consents, and that consent is one
// gRPC call rather than a redeploy.
//
// The shrink here is a bulk status flip rather than a delete: with the
// default active-only publish policy the sidecar cannot tell the difference
// (both are "NetBox returned fewer records"), and it is reversible.
func TestChaos_N21_MassRemovalRefusedUntilOverride(t *testing.T) {
	requireChaos(t)
	waitForPrimary(t)

	const zone = "mycompany.com"
	canary := hostFQDN("server1-mgmt", "dc1")

	waitZoneAnswering(t, zone, 60*time.Second)
	waitForDNS(t, canary, dns.TypeA, "10.1.0.1", 25*time.Second)
	serialBefore := stableSerial(t, zone)
	namesBefore := len(axfrNames(t, zone))
	require.Greater(t, namesBefore, 100,
		"precondition: the seeded zone must be big enough to pass the guard's absolute floor")

	// Restore the records no matter how this exits: everything after this
	// point leaves NetBox altered.
	t.Cleanup(func() {
		massRestore(t)
		forcePoll(t)
		// Hand the environment back fully converged. This test moves most of
		// the seed twice (removal, then restore), and each move is a whole-
		// zone rewrite plus a CoreDNS reload on every replica; the tests that
		// follow budget ~25s for their own small changes to land and will
		// fail on the tail of this one. Waiting on the load-balanced endpoint
		// alone is not enough — it proves only that the faster replica
		// caught up.
		waitAllServers(t, 3*time.Minute, func(server string) bool {
			return answersTolerant(server, canary)
		}, "every replica must serve the restored records before the next test runs")
		// Both replicas agreeing on a serial means the restore write has
		// landed everywhere, not just the canary's zone.
		stableSerial(t, zone)
		// Never hand over a blocked guard: the restore is growth, so the poll
		// above must have cleared it.
		waitMetric(t, "netbox_sidecar_shrink_guard_blocked", 0, 60*time.Second)
	})

	massDeprecate(t, 90)
	forcePoll(t)

	// The guard must refuse, and say so where an alert can see it.
	waitMetric(t, "netbox_sidecar_shrink_guard_blocked", 1, 90*time.Second)
	refusals, ok := metricValue(fetchMetrics(t), `netbox_sidecar_shrink_guard_refusals_total{scope="global"}`)
	require.True(t, ok, "a refusal must be counted by scope")
	require.GreaterOrEqual(t, refusals, 1.0)

	// Nothing about what is served may have changed.
	assert.Equal(t, serialBefore, stableSerial(t, zone),
		"a refused publish must not advance the serial (it would NOTIFY every secondary)")
	assert.Len(t, axfrNames(t, zone), namesBefore, "the zone must still hold every name")
	for _, server := range dnsProbeServers() {
		r := queryServer(t, canary, dns.TypeA, server)
		assert.NotEmpty(t, r.Answer,
			"%s must keep answering %s from the zones it already has", server, canary)
	}

	// A second poll without consent stays refused: the shrink does not
	// become the new normal just because it persisted.
	forcePoll(t)
	time.Sleep(10 * time.Second)
	assert.Equal(t, serialBefore, stableSerial(t, zone), "still refused on the next poll")

	// One gRPC call later, the operator's intent is honoured.
	forcePollAllowShrink(t)
	waitNameGone(t, canary, dns.TypeA, 90*time.Second)
	waitMetric(t, "netbox_sidecar_shrink_guard_blocked", 0, 60*time.Second)

	after := fetchMetrics(t)
	overrides, ok := metricValue(after, "netbox_sidecar_shrink_guard_overrides_total")
	require.True(t, ok, "overrides must be counted")
	assert.GreaterOrEqual(t, overrides, 1.0)
	assert.Less(t, len(axfrNames(t, zone)), namesBefore/2, "the zone must have actually shrunk")
}
