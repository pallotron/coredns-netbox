package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pallotron/coredns-netbox/internal/ipcategorizer"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/nameformat"
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
		testDiscoverer(t), nil, m, "example.org", false, testGuard(), prev, override)
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
	_, _, err = fetchNetbox(context.Background(), client, nil, nil, nil, nil, m, "example.org", false,
		testGuard(), prev, nil)
	require.Error(t, err, "empty fetch over 42 published records must refuse to publish")
	assert.ErrorContains(t, err, "refusing to publish", "error must name the refusal")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.NetboxEmptyResponseTotal), "empty_response_total must increment")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.PollTotal.WithLabelValues("error")), "poll_total{result=\"error\"} must increment")
}

// A primary_ip4 / oob_ip that carries an explicit dns_name must still
// drive the device and BMC records. The explicit record is kept as-is
// alongside them.
func TestEnrichPrimaryAndOOBWithDNSName(t *testing.T) {
	cat, err := ipcategorizer.NewCategorizer("(?i)bmc|idrac", "^lo$", "storage", "mgmt", "(?i)mgmt|^eno[0-9]", "example.org")
	require.NoError(t, err)

	const dev = "web-01"
	records := []netboxclient.IPRecord{
		{ID: 50, Address: "10.0.0.50", Family: 4, DeviceName: dev, InterfaceName: "eno1", DNSName: "web.example.org"},
		{ID: 51, Address: "10.0.0.51", Family: 4, DeviceName: dev, InterfaceName: "eno2"},
		{ID: 60, Address: "10.9.0.60", Family: 4, DeviceName: dev, InterfaceName: "idrac", DNSName: "web-oob.example.org"},
		{ID: 61, Address: "10.9.0.61", Family: 4, DeviceName: dev, InterfaceName: "idrac-shared"},
	}
	devices := []netboxclient.DeviceRecord{{Name: dev, Status: "active", PrimaryIP4ID: 50, OOBIPID: 60}}

	got := enrichRecordsWithDeviceNames(records, devices, cat, "example.org", nil, false, nil)

	byKind := map[string]string{}
	var explicit []string
	for _, r := range got {
		switch r.Kind {
		case netboxclient.KindDevice, netboxclient.KindBMC:
			byKind[r.Kind] = r.Address
		case "":
			explicit = append(explicit, r.DNSName)
		}
	}
	assert.Equal(t, "10.0.0.50", byKind[netboxclient.KindDevice], "device record must use primary_ip4")
	assert.Equal(t, "10.9.0.60", byKind[netboxclient.KindBMC], "BMC record must use oob_ip")
	assert.ElementsMatch(t, []string{"web.example.org", "web-oob.example.org"}, explicit,
		"explicit dns_name records are still published unchanged")
}

// An interface name too long for DNS must not cost its address the device
// PTR: with interface records on, the address still reverse-resolves to the
// device name, exactly as with them off.
func TestEnrichTooLongInterfaceKeepsDevicePTR(t *testing.T) {
	cat, err := ipcategorizer.NewCategorizer("bmc", "^lo$", "storage", "mgmt", "mgmt", "example.org")
	require.NoError(t, err)

	const vm = "dc1-h1a-r101-prod-vm-07"
	records := []netboxclient.IPRecord{{
		ID: 15, Address: "10.20.0.15", Family: 4, DeviceName: vm,
		InterfaceName: "port-3f6c2a1e-9b7d-4c5a-8e2f-1d0b9a7c6e54-provider-net-vlan-1001", // 64 chars
	}}
	devices := []netboxclient.DeviceRecord{{Name: vm, Status: "active", PrimaryIP4ID: 15}}

	for _, ifaceRecords := range []bool{false, true} {
		enriched := enrichRecordsWithDeviceNames(records, devices, cat, "example.org", nil, ifaceRecords, nil)
		zones, err := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil).Discover(enriched)
		require.NoError(t, err)
		ptrs := zones["10.in-addr.arpa"]
		require.Len(t, ptrs, 1, "interfaceRecords=%v: exactly one PTR", ifaceRecords)
		assert.True(t, strings.HasPrefix(ptrs[0].DNSName, vm+"."),
			"interfaceRecords=%v: PTR must target the device name, got %s", ifaceRecords, ptrs[0].DNSName)
	}
}

// netbox_sidecar_ptr_uncovered_ips reports the distinct IPs of the last
// published poll that no reverse zone covers. It is a level, not a running
// count: a second identical poll leaves it unchanged.
func TestFetchNetboxSetsPTRUncoveredGauge(t *testing.T) {
	srv := fakeNetbox(t, 50) // 50 distinct 10.x addresses
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())

	client, err := netboxclient.New(srv.URL, "testtoken", 1000, 2, 0, 0, 0)
	require.NoError(t, err)
	cat, err := ipcategorizer.NewCategorizer("bmc", "^lo$", "storage", "mgmt", "mgmt", "example.org")
	require.NoError(t, err)
	// No reverse zone covers 10.0.0.0/8: every address is an uncovered private IP.
	reverse := zonediscovery.NewReverseZoneDiscoverer([]string{"168.192.in-addr.arpa"}, nil)

	for poll := 1; poll <= 2; poll++ {
		_, _, err := fetchNetbox(context.Background(), client, cat, nil,
			testDiscoverer(t), reverse, m, "example.org", false, testGuard(), shrinkguard.Baseline{}, nil)
		require.NoError(t, err, "poll %d", poll)
		assert.Equal(t, 50.0, testutil.ToFloat64(m.PTRUncoveredIPs.WithLabelValues("private")),
			"poll %d: 50 distinct uncovered private IPs", poll)
		assert.Equal(t, 0.0, testutil.ToFloat64(m.PTRUncoveredIPs.WithLabelValues("public")), "poll %d", poll)
	}
}

// A broken name template logs one error per device per poll, not one per
// record that needs the device's name (device, aliases, every interface).
func TestEnrichBrokenTemplateLogsOncePerDevice(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cat, err := ipcategorizer.NewCategorizer("bmc", "^lo$", "storage", "mgmt", "(?i)mgmt", "example.org")
	require.NoError(t, err)
	formatter, err := nameformat.New(
		[]string{`^(?P<dc>[a-z0-9]+)-(?P<role>[a-z]+)-(?P<idx>[0-9]+)$`},
		`{{.name}} extra.{{.domain}}`, // renders an invalid name
		[]string{`{{.role}}{{.idx}}.{{.domain}}`}, "", "example.org",
	)
	require.NoError(t, err)

	var records []netboxclient.IPRecord
	for i, iface := range []string{"mgmt0", "eno1", "eno2", "eno3", "eno4"} {
		records = append(records, netboxclient.IPRecord{
			ID: i + 1, Address: fmt.Sprintf("10.0.0.%d", i+1), Family: 4,
			DeviceName: "dc1-hv-01", InterfaceName: iface,
		})
	}

	enrichRecordsWithDeviceNames(records, nil, cat, "example.org", formatter, true, nil)
	assert.Equal(t, 1, strings.Count(buf.String(), "nameformat: canonical template"),
		"one error for the device, not one per record built from its name")
}

// The device fetch runs alongside the IP fetch: the fake NetBox holds every
// IP request until a device request has arrived, which only happens if the
// two overlap.
func TestFetchNetboxFetchesIPsAndDevicesConcurrently(t *testing.T) {
	deviceSeen := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ipam/ip-addresses/":
			select {
			case <-deviceSeen:
			case <-time.After(2 * time.Second):
				http.Error(w, "no device request while an IP request was in flight", http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprint(w, `{"count": 1, "results": [{"id": 1, "address": "10.0.0.1/32", "dns_name": "host1.example.org"}]}`)
		case "/api/dcim/devices/", "/api/virtualization/virtual-machines/":
			once.Do(func() { close(deviceSeen) })
			_, _ = fmt.Fprint(w, `{"count": 0, "results": []}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := metrics.NewSidecar(prometheus.NewRegistry())

	_, _, err := fetchWith(t, srv, m, shrinkguard.Baseline{}, nil)
	require.NoError(t, err, "IP and device fetches must overlap")
}
