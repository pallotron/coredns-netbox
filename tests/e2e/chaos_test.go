//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
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
				r, _, err := c.Exchange(m, dnsServer())
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
	if serialAfter != serialBefore {
		t.Logf("G5: sidecar restart bumped %s serial %d -> %d (restart-triggered rewrite; decision pending)",
			zone, serialBefore, serialAfter)
	}
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
