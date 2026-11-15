//go:build e2e

package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const webhookSecret = "dev-webhook-secret"

func signBody(t *testing.T, body []byte) string {
	t.Helper()
	mac := hmac.New(sha512.New, []byte(webhookSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhook_CreatedEventUpdatesDNSWithoutPoll(t *testing.T) {
	name := fmt.Sprintf("webhook-e2e-%d.mycompany.com", time.Now().UnixNano())
	body, err := json.Marshal(map[string]any{
		"event":       "created",
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
		"object_type": "ipam.ipaddress",
		"data": map[string]any{
			"address":  "10.77.0.1/32",
			"dns_name": name,
		},
		"snapshots": map[string]any{"prechange": nil, "postchange": nil},
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:18082/webhook/netbox", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hook-Signature", signBody(t, body))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The zone-depth default groups this under "mycompany.com"; the record
	// should be resolvable well before POLL_INTERVAL (60s) would next fire.
	c := &dns.Client{Timeout: 5 * time.Second, Net: "tcp"}
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), dns.TypeA)

	// 25s, not 15s: the worst case is a full CoreDNS reload interval (10s)
	// plus the cache TTL (5s), so a 15s budget had no headroom at all and
	// failed intermittently in CI. The claim being tested is that the
	// webhook beats POLL_INTERVAL (60s), which 25s still demonstrates.
	deadline := time.Now().Add(25 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		r, _, err := c.Exchange(msg, dnsServer())
		if err == nil && r.Rcode == dns.RcodeSuccess {
			for _, ans := range r.Answer {
				if a, ok := ans.(*dns.A); ok && a.A.Equal(net.ParseIP("10.77.0.1")) {
					found = true
				}
			}
		}
		if found {
			break
		}
		time.Sleep(1 * time.Second)
	}
	assert.True(t, found, "webhook-created record should resolve within 15s, well under POLL_INTERVAL")
}

// postWebhook signs body with the dev webhook secret and posts it to the
// sidecar, requiring a 200.
func postWebhook(t *testing.T, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:18082/webhook/netbox", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hook-Signature", signBody(t, body))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// setOverrideDevPrimary points the override device's primary_ip4 at addr
// (an address already assigned to one of its interfaces).
func setOverrideDevPrimary(t *testing.T, addr string) {
	t.Helper()
	netboxORM(t, fmt.Sprintf(`from dcim.models import Device
from ipam.models import IPAddress
d = Device.objects.get(name=%q)
d.primary_ip4 = IPAddress.objects.get(address=%q)
d.save()
print("primary_ip4 set")`, "dc1-h1a-r601-prod-sv-02", addr+"/24"))
}

// awayFromPeriodicPoll returns once the next periodic poll is at least
// margin away, waiting for one to pass if it is due sooner, so a DNS change
// seen within margin can only have come from something other than it.
func awayFromPeriodicPoll(t *testing.T, margin time.Duration) {
	t.Helper()
	const series = "netbox_sidecar_last_successful_poll_timestamp_seconds"
	last, ok := metricValue(fetchMetrics(t), series)
	require.True(t, ok, "%s must be exported", series)
	next := time.Unix(int64(last), 0).Add(pollInterval())
	if time.Until(next) >= margin {
		return
	}
	deadline := time.Now().Add(pollInterval() + time.Minute)
	for time.Now().Before(deadline) {
		if v, ok := metricValue(fetchMetrics(t), series); ok && v > last {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no periodic poll within %s", pollInterval()+time.Minute)
}

// A dcim.device webhook event triggers a poll, so a primary_ip4 change is
// published well before the next periodic poll. NetBox does not fire event
// rules for ORM writes, so the test makes the change and then posts the
// event NetBox would have sent for it.
func TestMutate_WebhookDeviceEventRepublishesPrimaryIP(t *testing.T) {
	requireChaos(t)
	skipUnlessNameTemplates(t)
	waitForPrimary(t)
	waitForDNS(t, overrideDevCanonical, dns.TypeA, overrideDevPrimaryIP, 60*time.Second)

	t.Cleanup(func() {
		setOverrideDevPrimary(t, overrideDevPrimaryIP)
		forcePoll(t)
		waitForDNS(t, overrideDevCanonical, dns.TypeA, overrideDevPrimaryIP, 60*time.Second)
	})

	const budget = 25 * time.Second // CoreDNS reload (10s) + cache TTL (5s) + fetch
	awayFromPeriodicPoll(t, budget+10*time.Second)

	setOverrideDevPrimary(t, overrideDevMgmt0IP)
	body, err := json.Marshal(map[string]any{
		"event":       "updated",
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
		"object_type": "dcim.device",
		"data":        map[string]any{"name": "dc1-h1a-r601-prod-sv-02"},
		"snapshots":   map[string]any{"prechange": nil, "postchange": nil},
	})
	require.NoError(t, err)
	postWebhook(t, body)

	waitForDNS(t, overrideDevCanonical, dns.TypeA, overrideDevMgmt0IP, budget)
}
