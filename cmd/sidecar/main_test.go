package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

func TestRegisterHealth(t *testing.T) {
	var ready atomic.Bool
	mux := http.NewServeMux()
	registerHealth(mux, &ready)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		assert.NoError(t, err, "GET %s", path)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	t.Run("livez is 200 before first successful fetch", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, get("/livez"))
	})

	t.Run("healthz is 503 before first successful fetch", func(t *testing.T) {
		assert.Equal(t, http.StatusServiceUnavailable, get("/healthz"))
	})

	t.Run("healthz is 200 once ready", func(t *testing.T) {
		ready.Store(true)
		assert.Equal(t, http.StatusOK, get("/healthz"))
		assert.Equal(t, http.StatusOK, get("/livez"))
	})
}

func TestWebhookPollAllowed(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	min := 5 * time.Second

	tests := []struct {
		name string
		last time.Time
		now  time.Time
		want bool
	}{
		{"zero value last always allows the first poll", time.Time{}, now, true},
		{"exactly at the cooldown boundary allows", now.Add(-min), now, true},
		{"just inside the cooldown window is blocked", now.Add(-min + time.Second), now, false},
		{"well within the cooldown window is blocked", now.Add(-time.Second), now, false},
		{"well past the cooldown window allows", now.Add(-2 * min), now, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, webhookPollAllowed(tt.last, tt.now, min))
		})
	}
}

// N20 (acceptance-test matrix) / gap G1, minimal policy: an
// empty-but-HTTP-200 NetBox result (token scoped to nothing, bad bulk
// edit) must never replace zones that currently hold records — under
// CISv2 the wipe would advance the serial, fire NOTIFY, and fan out to
// every site secondary via AXFR. An empty result is still accepted when
// nothing has been published yet (fresh install against unseeded NetBox).
func TestPublishAllowed(t *testing.T) {
	tests := []struct {
		name      string
		prevCount int
		newCount  int
		want      bool
	}{
		{"normal poll with records", 100, 100, true},
		{"first ever poll, empty netbox", 0, 0, true},
		{"first ever poll with records", 0, 50, true},
		{"shrink but non-empty allowed (threshold guard is N21, undecided)", 100, 1, true},
		{"empty over published records refused", 100, 0, false},
		{"empty over a single published record refused", 1, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, publishAllowed(tt.prevCount, tt.newCount))
		})
	}
}

func TestFetchNetboxRefusesEmptyOverPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"next":null,"previous":null,"results":[]}`))
	}))
	defer srv.Close()

	client, err := netboxclient.New(srv.URL, "testtoken", 100, 2, 0, 0, 0)
	require.NoError(t, err, "New() error")

	m := metrics.NewSidecar(prometheus.NewRegistry())

	// The refusal fires before record enrichment and zone discovery, so the
	// categorizer/formatter/discoverer arguments are never touched here.
	_, _, err = fetchNetbox(context.Background(), client, nil, nil, nil, nil, m, "example.org", 42)
	require.Error(t, err, "empty fetch over 42 published records must refuse to publish")
	assert.ErrorContains(t, err, "refusing to publish", "error must name the refusal")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.NetboxEmptyResponseTotal), "empty_response_total must increment")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.PollTotal.WithLabelValues("error")), "poll_total{result=\"error\"} must increment")
}
