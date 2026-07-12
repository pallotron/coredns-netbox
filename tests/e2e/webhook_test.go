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

	deadline := time.Now().Add(15 * time.Second)
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
