package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/shrinkguard"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
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

// fakeNetbox serves `count` IP records with sequential addresses and DNS
// names, honouring the limit/offset paging the client uses.
func fakeNetbox(t *testing.T, count int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if limit == 0 {
			limit = count
		}
		items := make([]string, 0, limit)
		for i := offset; i < offset+limit && i < count; i++ {
			items = append(items, fmt.Sprintf(
				`{"address":"10.%d.%d.%d/32","dns_name":"host%d.example.org","vrf":null,"assigned_object":null}`,
				i/65536%256, i/256%256, i%256, i))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":null,"previous":null,"results":[%s]}`,
			count, strings.Join(items, ","))
	}))
}

func testDiscoverer(t *testing.T) zonediscovery.Discoverer {
	t.Helper()
	d, err := zonediscovery.NewDiscoverer(zonediscovery.ModeZoneDepth, map[string]string{"depth": "2"})
	require.NoError(t, err)
	return d
}

func testGuard() shrinkguard.Config {
	return shrinkguard.Config{
		Enabled:            true,
		GlobalDropFraction: 0.05,
		GlobalMinRecords:   100,
		ZoneDropFraction:   0.5,
		ZoneMinRecords:     100,
	}
}

func fetchWith(t *testing.T, srv *httptest.Server, m *metrics.Sidecar,
	prev shrinkguard.Baseline, override *shrinkguard.Override,
) (zonediscovery.ZoneMap, shrinkguard.Baseline, error) {
	t.Helper()
	client, err := netboxclient.New(srv.URL, "testtoken", 1000, 2, 0, 0, 0)
	require.NoError(t, err, "New() error")
	cat, err := ipcategorizer.NewCategorizer("bmc", "^lo$", "storage", "mgmt", "mgmt", "example.org")
	require.NoError(t, err)
	return fetchNetbox(context.Background(), client, cat, nil,
		testDiscoverer(t), nil, m, "example.org", testGuard(), prev, override)
}

// N21 / gap G1: a NetBox fetch that succeeds but returns far fewer records
// than are currently served must not publish. Under CISv2 the loss would
// advance the serial, fire NOTIFY and fan out to every site secondary, so
// the sidecar keeps serving what it has until an operator says otherwise.
// The thresholds themselves are unit-tested in internal/shrinkguard; this
// pins the poll path's use of them, including the metrics an alert needs.
func TestFetchNetboxRefusesImplausibleShrink(t *testing.T) {
	srv := fakeNetbox(t, 500)
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())

	// 500 of 5000 records survive: 90% gone, far past the 5% trigger.
	prev := shrinkguard.FromZoneCounts(map[string]int{"example.org": 5000})
	zm, baseline, err := fetchWith(t, srv, m, prev, nil)

	require.Error(t, err, "a 90%% shrink must refuse to publish")
	assert.ErrorContains(t, err, "refusing to publish")
	assert.Nil(t, zm, "a refused publish hands back no zones to write")
	assert.Zero(t, baseline.Total, "and no baseline, so the served set stays the comparison point")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ShrinkGuardRefusalsTotal.WithLabelValues(shrinkguard.ScopeGlobal)))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ShrinkGuardBlocked), "blocked gauge drives the alert")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.PollTotal.WithLabelValues("error")))
}

func TestFetchNetboxPublishesWithinThresholds(t *testing.T) {
	srv := fakeNetbox(t, 4900)
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())

	// 2% off a 5000-record set: the shape of ordinary churn, which in a
	// month of production never came close to the trigger.
	prev := shrinkguard.FromZoneCounts(map[string]int{"example.org": 5000})
	zm, baseline, err := fetchWith(t, srv, m, prev, nil)

	require.NoError(t, err, "routine churn must publish")
	assert.NotEmpty(t, zm)
	assert.Equal(t, 4900, baseline.Total, "the new baseline is what was just published")
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ShrinkGuardBlocked))
}

// The override is the operator's way through a refusal, and it is single-use:
// consent given for one mass-removal must not wave through the next one.
func TestFetchNetboxOverrideIsConsumedBySinglePoll(t *testing.T) {
	srv := fakeNetbox(t, 500)
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())
	prev := shrinkguard.FromZoneCounts(map[string]int{"example.org": 5000})

	override := &shrinkguard.Override{}
	override.Arm()
	_, baseline, err := fetchWith(t, srv, m, prev, override)
	require.NoError(t, err, "an armed override must let the shrink through")
	assert.Equal(t, 500, baseline.Total)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ShrinkGuardOverridesTotal))

	// Same shrink again, no fresh consent.
	_, _, err = fetchWith(t, srv, m, prev, override)
	require.Error(t, err, "the override must not survive into the next poll")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ShrinkGuardOverridesTotal), "and must not be counted twice")
}

// An empty NetBox result is never a state to publish on purpose, so even an
// armed override does not clear it (internal/shrinkguard.EmptyResult).
func TestFetchNetboxOverrideDoesNotClearEmptyResult(t *testing.T) {
	srv := fakeNetbox(t, 0)
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())

	override := &shrinkguard.Override{}
	override.Arm()
	_, _, err := fetchWith(t, srv, m, shrinkguard.FromZoneCounts(map[string]int{"example.org": 5000}), override)

	require.Error(t, err, "an empty result must refuse even with consent armed")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ShrinkGuardRefusalsTotal.WithLabelValues(shrinkguard.ScopeEmpty)))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ShrinkGuardOverridesTotal), "and none is counted as honoured")
	// The consent is still spent: left armed, it could wave through an
	// unrelated shrink on some later poll.
	assert.False(t, override.Armed(), "consent must not survive the poll it was given for")
}

// The restart window, end to end: a replacement sidecar has no counts yet,
// but zones from the PVC cache are being served and must not be wiped by an
// empty NetBox result (the likeliest moment for one — a bad token or filter
// arrives with the rollout that restarts the pod).
func TestFetchNetboxRefusesEmptyWhileServingCachedZones(t *testing.T) {
	srv := fakeNetbox(t, 0)
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())

	_, _, err := fetchWith(t, srv, m, shrinkguard.Baseline{ZonesOnDisk: true}, nil)

	require.Error(t, err, "cached zones must not be wiped before this process has published once")
	assert.ErrorContains(t, err, "cached zones")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ShrinkGuardBlocked))
}

// Gap G4: before the first successful NetBox fetch of this process, a
// merge/write would hand removeOrphans an empty NetBox zone set and delete
// every cached zone file — the PVC cache that serve-stale across sidecar
// restarts (matrix N16/N22) depends on. Merging is only safe once NetBox
// data exists in memory, or when there is nothing on disk to destroy.
// G13: the readiness probe gates the sidecar Service, which CoreDNS's
// zone-init fetches through. Tying it to a successful fetch meant a Netbox
// outage plus a restart left CoreDNS unable to reach zones that were
// sitting on the PVC the whole time.
func TestReadyToServe(t *testing.T) {
	tests := []struct {
		name                 string
		publishedThisProcess bool
		hasExistingZones     bool
		want                 bool
		why                  string
	}{
		{
			name:                 "published this process",
			publishedThisProcess: true,
			want:                 true,
			why:                  "a successful fetch+merge is the ordinary ready path",
		},
		{
			name:             "cached zones, Netbox unreachable",
			hasExistingZones: true,
			want:             true,
			why:              "this is G13: the PVC holds servable zones, so the pod must stay in the Service and let zone-init fetch them",
		},
		{
			name:                 "cached zones and a fresh publish",
			publishedThisProcess: true,
			hasExistingZones:     true,
			want:                 true,
		},
		{
			name: "nothing published and nothing on disk",
			want: false,
			why:  "a fresh install with Netbox down has nothing to serve; answering from an empty zone is worse than not answering",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, readyToServe(tt.publishedThisProcess, tt.hasExistingZones), tt.why)
		})
	}
}

func TestMergeAllowed(t *testing.T) {
	tests := []struct {
		name               string
		fetchSucceededOnce bool
		hasExistingZones   bool
		want               bool
	}{
		{"normal operation after first successful fetch", true, true, true},
		{"first fetch ok, fresh disk", true, false, true},
		{"restart with NetBox down and cached zones: refuse", false, true, false},
		{"fresh install with NetBox down, nothing to destroy", false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mergeAllowed(tt.fetchSucceededOnce, tt.hasExistingZones))
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
	prev := shrinkguard.FromZoneCounts(map[string]int{"example.org": 42})
	_, _, err = fetchNetbox(context.Background(), client, nil, nil, nil, nil, m, "example.org",
		testGuard(), prev, nil)
	require.Error(t, err, "empty fetch over 42 published records must refuse to publish")
	assert.ErrorContains(t, err, "refusing to publish", "error must name the refusal")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.NetboxEmptyResponseTotal), "empty_response_total must increment")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.PollTotal.WithLabelValues("error")), "poll_total{result=\"error\"} must increment")
}
