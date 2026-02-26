package netboxclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dtoMetric "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

type mockIPList struct {
	Count    int          `json:"count"`
	Next     *string      `json:"next"`
	Previous *string      `json:"previous"`
	Results  []mockIPAddr `json:"results"`
}

type mockIPAddr struct {
	ID      int    `json:"id"`
	URL     string `json:"url"`
	Display string `json:"display"`
	Address string `json:"address"`
	DNSName string `json:"dns_name"`
	Status  struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"status"`
	Family struct {
		Value int    `json:"value"`
		Label string `json:"label"`
	} `json:"family"`
	NatOutside []interface{} `json:"nat_outside"`
}

func newMockAddr(id int, addr, dnsName string) mockIPAddr {
	m := mockIPAddr{
		ID:         id,
		URL:        fmt.Sprintf("http://netbox.example.com/api/ipam/ip-addresses/%d/", id),
		Display:    addr,
		Address:    addr,
		DNSName:    dnsName,
		NatOutside: []interface{}{},
	}
	m.Status.Value = "active"
	m.Status.Label = "Active"
	m.Family.Value = 4
	m.Family.Label = "IPv4"
	return m
}

func TestFetchIPAddresses(t *testing.T) {
	// Create 5 test records
	allRecords := []mockIPAddr{
		newMockAddr(1, "10.0.0.1/24", "host1.example.org"),
		newMockAddr(2, "10.0.0.2/24", "host2.example.org"),
		newMockAddr(3, "10.0.0.3/24", "host3.example.org"),
		newMockAddr(4, "10.0.0.4/24", "host4.example.org"),
		newMockAddr(5, "10.0.0.5/24", "host5.example.org"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ipam/ip-addresses/" {
			http.NotFound(w, r)
			return
		}

		q := r.URL.Query()
		limit := 2
		offset := 0
		if v := q.Get("limit"); v != "" {
			_, _ = fmt.Sscanf(v, "%d", &limit)
		}
		if v := q.Get("offset"); v != "" {
			_, _ = fmt.Sscanf(v, "%d", &offset)
		}

		end := offset + limit
		if end > len(allRecords) {
			end = len(allRecords)
		}
		start := offset
		if start > len(allRecords) {
			start = len(allRecords)
		}

		resp := mockIPList{
			Count:   len(allRecords),
			Results: allRecords[start:end],
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := New(srv.URL, "testtoken", 2, 5, 0, 0, 0)
	require.NoError(t, err, "New() error")

	records, err := client.FetchIPAddresses(context.Background())
	require.NoError(t, err, "FetchIPAddresses() error")

	if len(records) != 5 {
		t.Errorf("expected 5 records, got %d", len(records))
	}

	// Verify CIDR was stripped
	for _, r := range records {
		if r.Address == "" {
			t.Error("empty address")
		}
		if r.DNSName == "" {
			t.Error("empty DNS name")
		}
		if r.Family != 4 {
			t.Errorf("expected family 4, got %d", r.Family)
		}
		if len(r.Address) > 0 && r.Address[len(r.Address)-1] == '/' {
			t.Errorf("address still has trailing slash: %s", r.Address)
		}
	}
}

func TestStripCIDR(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"10.0.0.1/24", "10.0.0.1"},
		{"192.168.1.1/32", "192.168.1.1"},
		{"10.0.0.1", "10.0.0.1"},
		{"2001:db8::1/64", "2001:db8::1"},
	}

	for _, tt := range tests {
		got := stripCIDR(tt.input)
		if got != tt.want {
			t.Errorf("stripCIDR(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFetchIPAddresses_RetryOnTransientError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.URL.Query().Get("limit") == "1" {
			// Probe request — always succeed
			resp := ipListResponse{Count: 1}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if attempts <= 3 { // fail first 2 page fetches (attempts 2 and 3)
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		// Succeed on 3rd page fetch
		resp := ipListResponse{
			Count:   1,
			Results: []ipItem{{Address: "10.0.0.1/24", DNSName: "host.example.org"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	retryCounter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_retries"})

	c, err := New(srv.URL, "token", 100, 1, 3, time.Millisecond, 10*time.Millisecond)
	require.NoError(t, err, "New")
	c.RetryCounter = retryCounter

	records, err := c.FetchIPAddresses(context.Background())
	require.NoError(t, err, "expected success after retries")
	require.NotEmpty(t, records, "expected at least 1 record")
	dto := &dtoMetric.Metric{}
	_ = retryCounter.Write(dto)
	if got := dto.Counter.GetValue(); got == 0 {
		t.Errorf("expected retry counter > 0, got %v (attempts=%d)", got, attempts)
	}
}

func TestFetchIPAddresses_RetryExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "token", 100, 1, 2, time.Millisecond, 10*time.Millisecond)
	require.NoError(t, err, "New")

	_, err = c.FetchIPAddresses(context.Background())
	require.Error(t, err, "expected error after retry exhaustion")
}

func TestFetchIPAddresses_RetryRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "token", 100, 1, 10, time.Second, 30*time.Second)
	require.NoError(t, err, "New")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = c.FetchIPAddresses(ctx)
	require.Error(t, err, "expected error due to context cancellation")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("retry did not respect context cancellation (elapsed %v)", elapsed)
	}
}
