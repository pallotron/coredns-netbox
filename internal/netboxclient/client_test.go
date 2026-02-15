package netboxclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
			fmt.Sscanf(v, "%d", &limit)
		}
		if v := q.Get("offset"); v != "" {
			fmt.Sscanf(v, "%d", &offset)
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
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := New(srv.URL, "testtoken", 2, 5)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	records, err := client.FetchIPAddresses(context.Background())
	if err != nil {
		t.Fatalf("FetchIPAddresses() error: %v", err)
	}

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
