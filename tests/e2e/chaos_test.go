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
