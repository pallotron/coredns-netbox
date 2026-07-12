package netboxwebhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) (srv *httptest.Server, store dynamicstore.DynamicStore, mergeSignal, pollSignal chan struct{}) {
	t.Helper()
	store, err := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	require.NoError(t, err)
	disc := &zonediscovery.ZoneDepthDiscoverer{Depth: 2}
	m := metrics.NewSidecar(prometheus.NewRegistry())
	mergeSignal = make(chan struct{}, 1)
	pollSignal = make(chan struct{}, 1)

	mux := http.NewServeMux()
	Register(mux, testSecret, store, disc, mergeSignal, pollSignal, m)
	return httptest.NewServer(mux), store, mergeSignal, pollSignal
}

func computeSignature(secret string, body []byte) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func postSigned(t *testing.T, srv *httptest.Server, body []byte, secret string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+Path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hook-Signature", computeSignature(secret, body))
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func TestHandler_CreatedEvent_UpsertsRecord(t *testing.T) {
	srv, store, mergeSignal, _ := newTestServer(t)
	defer srv.Close()

	resp := postSigned(t, srv, readFixture(t, "created.json"), testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 1)
	assert.Equal(t, "webhook-test-1.mycompany.com", got[0].DNSName)
	assert.Equal(t, netboxclient.SourceWebhook, got[0].Source)

	select {
	case <-mergeSignal:
	default:
		t.Fatal("expected mergeSignal to be sent")
	}
}

func TestHandler_InvalidSignature_Rejected(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	resp := postSigned(t, srv, readFixture(t, "created.json"), "wrong-secret")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))
}

func TestHandler_RenameEvent_DeletesOldName(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	resp1 := postSigned(t, srv, readFixture(t, "created.json"), testSecret)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2 := postSigned(t, srv, readFixture(t, "updated_rename.json"), testSecret)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 1, "old name must be removed, only the renamed record remains")
	assert.Equal(t, "webhook-test-1-renamed.mycompany.com", got[0].DNSName)
}

func TestHandler_DeletedEvent_RemovesRecord(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	resp1 := postSigned(t, srv, readFixture(t, "created.json"), testSecret)
	resp1.Body.Close()
	resp2 := postSigned(t, srv, readFixture(t, "updated_rename.json"), testSecret)
	resp2.Body.Close()

	resp3 := postSigned(t, srv, readFixture(t, "deleted.json"), testSecret)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))
}

func TestHandler_StaleEvent_Ignored(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	// Seed a webhook-sourced record with a newer AppliedAt than the fixture's
	// timestamp (2026-07-12T07:59:53Z) — simulates a newer update having
	// already been applied before this (older, out-of-order) delivery arrives.
	require.NoError(t, store.UpsertRecords("mycompany.com", []netboxclient.IPRecord{
		{DNSName: "webhook-test-1.mycompany.com", Address: "10.99.99.99", Family: 4,
			Source: netboxclient.SourceWebhook, AppliedAt: time.Now()},
	}))

	resp := postSigned(t, srv, readFixture(t, "created.json"), testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 1)
	assert.Equal(t, "10.99.99.99", got[0].Address, "older event must not overwrite the newer applied record")
}

func TestHandler_ManualRecordPinned_WebhookCannotOverride(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	require.NoError(t, store.UpsertRecords("mycompany.com", []netboxclient.IPRecord{
		{DNSName: "webhook-test-1.mycompany.com", Address: "10.1.1.1", Family: 4}, // Source == "" (manual)
	}))

	resp := postSigned(t, srv, readFixture(t, "created.json"), testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 1)
	assert.Equal(t, "10.1.1.1", got[0].Address, "manually-pinned record must not be overwritten by a webhook event")
}

func TestHandler_UnsupportedObjectType_Ignored(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	body := []byte(`{"event":"created","timestamp":"2026-07-12T08:00:00Z","object_type":"dcim.device","data":{}}`)
	resp := postSigned(t, srv, body, testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))
}

// TestHandler_CreatedEvent_EmptyDNSName_TriggersPoll covers records that
// rely on device-based DNS name generation (ipcategorizer/nameformat) rather
// than an explicit dns_name in Netbox — the majority case in some
// deployments. A single webhook event can't compute that name (it needs
// cross-record context: comparing all of a device's interfaces, resolving
// zone-wide CNAME alias collisions), so the handler triggers a full poll
// instead of writing directly, and must not touch the store itself.
func TestHandler_CreatedEvent_EmptyDNSName_TriggersPoll(t *testing.T) {
	srv, store, mergeSignal, pollSignal := newTestServer(t)
	defer srv.Close()

	body := []byte(`{"event":"created","timestamp":"2026-07-12T08:00:00Z","object_type":"ipam.ipaddress","data":{"address":"10.0.0.9/32","dns_name":""}}`)
	resp := postSigned(t, srv, body, testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"), "no direct write — the handler can't compute a device-based name from one event")

	select {
	case <-pollSignal:
	default:
		t.Fatal("expected a full poll to be triggered for a dns_name-less event")
	}
	select {
	case <-mergeSignal:
		t.Fatal("mergeSignal must not fire when no direct store write happened")
	default:
	}
}

// TestHandler_DeletedEvent_EmptyDNSName_TriggersPoll mirrors the created-event
// case: a "deleted" event whose prechange snapshot has no dns_name means the
// removed IP was part of device-based DNS generation too — removing it may
// change which interface the categorizer now prefers for that device, which
// again needs the full poll's cross-record context.
func TestHandler_DeletedEvent_EmptyDNSName_TriggersPoll(t *testing.T) {
	srv, store, mergeSignal, pollSignal := newTestServer(t)
	defer srv.Close()

	body := []byte(`{"event":"deleted","timestamp":"2026-07-12T08:00:00Z","object_type":"ipam.ipaddress","data":null,"snapshots":{"prechange":{"dns_name":""}}}`)
	resp := postSigned(t, srv, body, testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))

	select {
	case <-pollSignal:
	default:
		t.Fatal("expected a full poll to be triggered for a dns_name-less deleted event")
	}
	select {
	case <-mergeSignal:
		t.Fatal("mergeSignal must not fire when no direct store write happened")
	default:
	}
}

// TestHandler_RenameEvent_MergeSignalFiresWhenNewZoneUnresolved covers review
// finding 1: applyUpsert's rename branch deletes the old name from its own
// zone, then tries to resolve a zone for the new name. If the old-name delete
// succeeded but the new name doesn't map to any configured zone, the old
// delete is still a real store mutation and mergeSignal must fire — even
// though no new record could be written.
func TestHandler_RenameEvent_MergeSignalFiresWhenNewZoneUnresolved(t *testing.T) {
	srv, store, mergeSignal, _ := newTestServer(t)
	defer srv.Close()

	// Seed the old name so the rename's delete step has something to remove.
	resp1 := postSigned(t, srv, readFixture(t, "created.json"), testSecret)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)
	// Drain the mergeSignal sent by the create so the assertion below only
	// reflects the rename event itself.
	select {
	case <-mergeSignal:
	default:
		t.Fatal("expected mergeSignal from the initial create")
	}

	// Rename to a DNS name with only one label, which ZoneDepthDiscoverer
	// (Depth: 2, used by newTestServer) cannot resolve to any zone.
	body := []byte(`{"event":"updated","timestamp":"2026-07-12T08:00:00Z","object_type":"ipam.ipaddress","data":{"address":"10.99.99.5/32","dns_name":"unresolvable"},"snapshots":{"prechange":{"dns_name":"webhook-test-1.mycompany.com"}}}`)
	resp2 := postSigned(t, srv, body, testSecret)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	assert.Empty(t, store.GetRecords("mycompany.com"), "old name must have been deleted despite the new name's zone being unresolvable")
	select {
	case <-mergeSignal:
	default:
		t.Fatal("expected mergeSignal to fire because the rename's old-name delete mutated the store")
	}
}

// TestHandler_DeletedEvent_ManualRecordPinned_NotDeleted covers review finding
// 2: applyDelete must not remove a manually-added (non-webhook-sourced)
// record for the same name, mirroring upsertGuarded's existing precedence
// rule for upserts.
func TestHandler_DeletedEvent_ManualRecordPinned_NotDeleted(t *testing.T) {
	srv, store, mergeSignal, _ := newTestServer(t)
	defer srv.Close()

	require.NoError(t, store.UpsertRecords("mycompany.com", []netboxclient.IPRecord{
		{DNSName: "webhook-test-1-renamed.mycompany.com", Address: "10.1.1.1", Family: 4}, // Source == "" (manual)
	}))

	resp := postSigned(t, srv, readFixture(t, "deleted.json"), testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 1)
	assert.Equal(t, "10.1.1.1", got[0].Address, "manually-added record must not be deleted by a webhook deleted event")

	select {
	case <-mergeSignal:
		t.Fatal("mergeSignal must not fire when the delete was skipped")
	default:
	}
}

// TestHandler_RenameEvent_ManualRecordPinned_OldNameNotDeleted covers
// whole-branch review finding 1: applyUpsert's rename branch must apply the
// same manual-record precedence check applyDelete already uses before
// deleting the old name. Without it, a manually-added (non-webhook-sourced)
// record occupying the old name would be permanently, silently deleted —
// data loss that nothing else (not the periodic poll, not
// ReconcileWebhookSourced) would ever repair, since manual records only ever
// exist in dynamicstore.
func TestHandler_RenameEvent_ManualRecordPinned_OldNameNotDeleted(t *testing.T) {
	srv, store, mergeSignal, _ := newTestServer(t)
	defer srv.Close()

	// Seed a manually-added record (Source == "") at the name the rename
	// event's snapshots.prechange.dns_name will reference.
	require.NoError(t, store.UpsertRecords("mycompany.com", []netboxclient.IPRecord{
		{DNSName: "webhook-test-1.mycompany.com", Address: "10.1.1.1", Family: 4},
	}))

	// updated_rename.json renames webhook-test-1.mycompany.com (the manually
	// pinned name) to webhook-test-1-renamed.mycompany.com.
	resp := postSigned(t, srv, readFixture(t, "updated_rename.json"), testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 2, "the manual old-name record must survive, and the new name must still be upserted")

	byName := make(map[string]netboxclient.IPRecord, len(got))
	for _, r := range got {
		byName[r.DNSName] = r
	}

	manual, ok := byName["webhook-test-1.mycompany.com"]
	require.True(t, ok, "manually-added record at the old name must not be deleted by the rename")
	assert.Equal(t, "10.1.1.1", manual.Address)
	assert.Empty(t, manual.Source, "manual record's source must remain unset")

	renamed, ok := byName["webhook-test-1-renamed.mycompany.com"]
	require.True(t, ok, "handler must still proceed to upsert the new name despite skipping the old-name delete")
	assert.Equal(t, netboxclient.SourceWebhook, renamed.Source)

	// The new-name upsert is a real store mutation, so mergeSignal must still
	// fire even though the old-name delete was skipped.
	select {
	case <-mergeSignal:
	default:
		t.Fatal("expected mergeSignal to fire because the new-name upsert mutated the store")
	}
}

// TestHandler_ConcurrentSameNameDeliveries_NewerEventWins covers whole-branch
// review finding 2: without a mutex serializing the read-decide-write
// sequence in apply, two concurrent deliveries for the same DNS name can
// both read the same "before" state, both pass their AppliedAt ordering
// guard, and then race on the write — leaving the outcome dependent on write
// arrival order rather than event recency. With the handler-level mutex in
// place, the guard's read-decide-write sequence is atomic per event, so
// regardless of which goroutine's HTTP request is scheduled/accepted first,
// the record with the later AppliedAt timestamp must always be the one left
// in the store.
func TestHandler_ConcurrentSameNameDeliveries_NewerEventWins(t *testing.T) {
	srv, store, _, _ := newTestServer(t)
	defer srv.Close()

	const dnsName = "webhook-test-1.mycompany.com"
	older := []byte(`{"event":"created","timestamp":"2026-07-12T08:00:00Z","object_type":"ipam.ipaddress","data":{"address":"10.50.50.1/32","dns_name":"` + dnsName + `"}}`)
	newer := []byte(`{"event":"created","timestamp":"2026-07-12T08:00:05Z","object_type":"ipam.ipaddress","data":{"address":"10.50.50.2/32","dns_name":"` + dnsName + `"}}`)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resp := postSigned(t, srv, older, testSecret)
		resp.Body.Close()
	}()
	go func() {
		defer wg.Done()
		resp := postSigned(t, srv, newer, testSecret)
		resp.Body.Close()
	}()
	wg.Wait()

	got := store.GetRecords("mycompany.com")
	require.Len(t, got, 1)
	assert.Equal(t, dnsName, got[0].DNSName)
	assert.Equal(t, "10.50.50.2", got[0].Address, "the record from the newer event must win, regardless of delivery/arrival order")
}

func TestRegister_EmptySecretDoesNotRegisterRoute(t *testing.T) {
	store, err := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	require.NoError(t, err)
	disc := &zonediscovery.ZoneDepthDiscoverer{Depth: 2}
	m := metrics.NewSidecar(prometheus.NewRegistry())
	mux := http.NewServeMux()
	Register(mux, "", store, disc, make(chan struct{}, 1), make(chan struct{}, 1), m)

	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+Path, "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
