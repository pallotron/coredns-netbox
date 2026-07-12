# Netbox Webhook Trigger Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the sidecar apply Netbox `ipam.ipaddress` changes (create/update/delete) directly from a signed Netbox webhook, without a NATS/Benthos relay and without a full Netbox re-fetch — implementing GitHub issue [#27](https://github.com/pallotron/coredns-netbox/issues/27).

**Architecture:** A new HTTP route on the sidecar's existing mux (`internal/netboxwebhook`) verifies Netbox's `X-Hook-Signature` (HMAC-SHA512), parses the webhook payload, and writes directly into `internal/dynamicstore` — reusing the same `mergeSignal`-driven re-merge path that `DynamicZoneService.UpsertRecord` already uses today (`internal/grpcserver/zone_service.go:48-58`). Records written by the webhook are tagged `Source: "webhook"` and carry the Netbox event's timestamp; this prevents two problems: (1) out-of-order concurrent webhook deliveries clobbering a newer change with an older one, and (2) a webhook-derived record permanently shadowing a name after the next full poll has already re-fetched newer truth for it. The periodic poll loop keeps running unchanged and reconciles (drops) stale webhook-sourced overlay entries on every full poll.

**Tech Stack:** Go 1.25, standard library `net/http`, `crypto/hmac`, `crypto/sha512` (no new dependencies), existing `testify` assert/require in tests.

## Global Constraints

- Go version: 1.25.7 (see `go.mod`).
- Tests use `testify` `assert`/`require`, never manual `t.Fatal` checks (project convention — see `internal/dynamicstore/file_store_test.go` for the established style).
- Run `make test.unit` (all packages, `-race`) after every task; it must stay green.
- No new third-party dependencies — HMAC/SHA512 and JSON are standard library.
- **Scope limitation (must be documented, not silently dropped):** the webhook fast-path only handles IP addresses that already have `dns_name` populated in Netbox. Records that rely on device-name-based enrichment (`ipcategorizer`/`nameformat`, used when `dns_name` is empty — see `cmd/sidecar/main.go:463-491`) are **not** handled by the webhook; those still require the periodic full poll, because that enrichment needs cross-record context (CNAME collision resolution in `zonegen.resolveCNAMECollisions`) that a single webhook event doesn't have. A webhook event with an empty `dns_name` is a no-op (logged at debug, `200 OK`).
- The webhook route is registered on the mux **only if** `NETBOX_WEBHOOK_SECRET` is set — there is no accidental unauthenticated trigger endpoint.

---

### Task 1: Export shared IP-object parsing from `netboxclient`

The webhook's `data` field has the same JSON shape as a single item in `/api/ipam/ip-addresses/`'s `results` array (confirmed against a real Netbox v4.6.0 capture — see issue #27 comment). Rather than duplicating the field-mapping logic (`internal/netboxclient/client.go:195-229`), extract it into a function both the existing fetch path and the new webhook path call.

**Files:**
- Modify: `internal/netboxclient/client.go:159-249` (extract shared mapping)
- Test: `internal/netboxclient/client_test.go` (add new test, existing tests must keep passing unchanged)

**Interfaces:**
- Produces: `netboxclient.RecordFromJSON(raw []byte) (IPRecord, error)` — used by Task 5.

- [ ] **Step 1: Write the failing test**

Add to `internal/netboxclient/client_test.go`:

```go
func TestRecordFromJSON(t *testing.T) {
	raw := []byte(`{
		"id": 18001,
		"address": "10.9.0.1/24",
		"dns_name": "host1.example.org",
		"vrf": {"name": "mgmt-vrf"},
		"assigned_object_type": "dcim.interface",
		"assigned_object": {
			"id": 1,
			"name": "mgmt0",
			"device": {"id": 1, "name": "dc1-h1a-r101-prod-hv-01"}
		}
	}`)

	rec, err := netboxclient.RecordFromJSON(raw)
	require.NoError(t, err)
	assert.Equal(t, "host1.example.org", rec.DNSName)
	assert.Equal(t, "10.9.0.1", rec.Address, "CIDR suffix must be stripped")
	assert.Equal(t, 4, rec.Family)
	assert.Equal(t, "dc1-h1a-r101-prod-hv-01", rec.DeviceName)
	assert.Equal(t, "mgmt0", rec.InterfaceName)
	assert.Equal(t, "mgmt-vrf", rec.VRF)
}

func TestRecordFromJSON_NoAssignedObject(t *testing.T) {
	raw := []byte(`{"address": "10.99.99.5/32", "dns_name": "standalone.example.org", "vrf": null, "assigned_object": null}`)

	rec, err := netboxclient.RecordFromJSON(raw)
	require.NoError(t, err)
	assert.Equal(t, "standalone.example.org", rec.DNSName)
	assert.Empty(t, rec.DeviceName)
	assert.Empty(t, rec.VRF)
}

func TestRecordFromJSON_InvalidJSON(t *testing.T) {
	_, err := netboxclient.RecordFromJSON([]byte(`not-json`))
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netboxclient/... -run TestRecordFromJSON -v`
Expected: FAIL — `undefined: netboxclient.RecordFromJSON`

- [ ] **Step 3: Extract the shared mapping and add the exported function**

In `internal/netboxclient/client.go`, replace the inline parsing loop inside `fetchIPAddressesOnce` (the body of the `for _, ip := range resp.Results` loop, lines 196-229) and add two new functions. The full updated function:

```go
// fetchIPAddressesOnce retrieves all active IP addresses from Netbox
// using parallel paginated requests (single attempt, no retry).
func (c *Client) fetchIPAddressesOnce(ctx context.Context) ([]IPRecord, error) {
	// Probe request to get total count
	probeResp, err := c.fetchPage(ctx, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}

	total := probeResp.Count
	if total == 0 {
		return nil, nil
	}

	totalPages := int(math.Ceil(float64(total) / float64(c.pageSize)))

	// Fan-out with bounded concurrency
	// Use indexed results to ensure deterministic ordering
	sem := make(chan struct{}, c.maxConcurrency)
	results := make([][]IPRecord, totalPages)
	errsCh := make(chan error, totalPages)

	var wg sync.WaitGroup
	for page := 0; page < totalPages; page++ {
		wg.Add(1)
		go func(pageNum int, offset int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := c.fetchPage(ctx, offset, c.pageSize)
			if err != nil {
				errsCh <- fmt.Errorf("fetch page offset=%d: %w", offset, err)
				return
			}

			records := make([]IPRecord, 0, len(resp.Results))
			for _, ip := range resp.Results {
				records = append(records, ipItemToRecord(ip))
			}
			results[pageNum] = records
		}(page, page*c.pageSize)
	}

	wg.Wait()
	close(errsCh)

	// Check for errors
	if err := <-errsCh; err != nil {
		return nil, err
	}

	// Collect results in deterministic page order
	var all []IPRecord
	for _, records := range results {
		all = append(all, records...)
	}

	return all, nil
}

// RecordFromJSON parses a single Netbox IP address object into an IPRecord.
// It accepts the same JSON shape as one element of /api/ipam/ip-addresses/'s
// "results" array, which is also the shape of a Netbox webhook's "data" field
// for an ipam.ipaddress event (verified against a Netbox v4.6.0 webhook capture).
// Fields present in a webhook payload but not read here (e.g. "id", "family",
// "status") are intentionally ignored — family is derived from the address
// string, exactly as fetchIPAddressesOnce already does.
func RecordFromJSON(raw []byte) (IPRecord, error) {
	var item ipItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return IPRecord{}, fmt.Errorf("decode ip address object: %w", err)
	}
	return ipItemToRecord(item), nil
}

// ipItemToRecord maps a decoded Netbox IP address object to an IPRecord.
func ipItemToRecord(ip ipItem) IPRecord {
	addr := stripCIDR(ip.Address)
	family := 4
	if strings.Contains(addr, ":") {
		family = 6
	}

	deviceName := ""
	interfaceName := ""
	if ip.AssignedObject != nil {
		interfaceName = ip.AssignedObject.Name
		if ip.AssignedObject.Device != nil {
			deviceName = ip.AssignedObject.Device.Name
		} else if ip.AssignedObject.VirtualMachine != nil {
			deviceName = ip.AssignedObject.VirtualMachine.Name
		}
	}

	vrf := ""
	if ip.VRF != nil {
		vrf = ip.VRF.Name
	}

	return IPRecord{
		DNSName:       ip.DNSName,
		Address:       addr,
		Family:        family,
		DeviceName:    deviceName,
		InterfaceName: interfaceName,
		VRF:           vrf,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/netboxclient/... -v`
Expected: PASS — all of `TestRecordFromJSON*` plus every pre-existing test in the package (`TestFetchIPAddresses`, `TestFetchIPAddresses_VMInterface`, etc., unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/netboxclient/client.go internal/netboxclient/client_test.go
git commit -m "refactor: extract shared Netbox IP-object parsing as RecordFromJSON"
```

---

### Task 2: Add `Source`/`AppliedAt` metadata to `IPRecord`

**Files:**
- Modify: `internal/netboxclient/client.go:28-42` (add fields + constant)
- Test: `internal/zonegen/generator_test.go` (add a test codifying that these fields never affect zone hashing/output)

**Interfaces:**
- Produces: `netboxclient.IPRecord.Source string`, `netboxclient.IPRecord.AppliedAt time.Time`, `netboxclient.SourceWebhook = "webhook"` constant. The zero value of `Source` (`""`) means "Netbox-fetched or manually added via gRPC" — existing behavior, unchanged.

- [ ] **Step 1: Write the failing test**

Add to `internal/zonegen/generator_test.go`:

```go
func TestGenerate_SourceAndAppliedAtDoNotAffectHash(t *testing.T) {
	g1 := zonegen.NewGenerator(zonegen.ZoneConfig{
		Origin: "example.org.", PrimaryNS: "ns1.example.org.", AdminEmail: "admin.example.org.",
		TTL: 300, Type: zonegen.ZoneTypeForward,
	}, "")
	content1, changed1, _, err := g1.Generate([]netboxclient.IPRecord{
		{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4},
	})
	require.NoError(t, err)
	assert.True(t, changed1)

	// Same record, but now tagged as webhook-sourced with a timestamp — the
	// generated zone content must be byte-identical and must NOT register as
	// a change (the hash must only reflect DNS-relevant fields).
	content2, changed2, _, err := g1.Generate([]netboxclient.IPRecord{
		{
			DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4,
			Source: netboxclient.SourceWebhook, AppliedAt: time.Now(),
		},
	})
	require.NoError(t, err)
	assert.False(t, changed2, "Source/AppliedAt must not be part of the change-detection hash")
	assert.Empty(t, content2, "unchanged generation returns empty content")
	_ = content1
}
```

Add `"time"` to the test file's imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/zonegen/... -run TestGenerate_SourceAndAppliedAtDoNotAffectHash -v`
Expected: FAIL — `unknown field Source in struct literal of type netboxclient.IPRecord`

- [ ] **Step 3: Add the fields**

In `internal/netboxclient/client.go`, update the `IPRecord` struct and add the source constants:

```go
// RecordTypeCNAME marks an IPRecord as a CNAME alias rather than an
// address record. The zero value of Type means A/AAAA (from Family).
const RecordTypeCNAME = "CNAME"

// SourceWebhook marks an IPRecord as written by the Netbox webhook trigger
// (internal/netboxwebhook), as opposed to the zero value "" which covers
// both Netbox-fetched records and records added manually via the
// DynamicZoneService gRPC API. Only dynamicstore-held records ever set this;
// it is never present on Netbox-fetched records.
const SourceWebhook = "webhook"

// IPRecord is a simplified representation of a Netbox IP address with DNS info.
type IPRecord struct {
	DNSName       string `json:"dns_name"`
	Address       string `json:"address"` // IP address without CIDR prefix
	Family        int    `json:"family"` // 4 or 6
	DeviceName    string `json:"device_name,omitempty"` // Device name from assigned_object.device.name
	InterfaceName string `json:"interface_name,omitempty"` // Interface name from assigned_object.name
	VRF           string `json:"vrf,omitempty"` // VRF name
	TTL           uint32 `json:"ttl,omitempty"`
	// Type is "" for address records (A/AAAA per Family) or RecordTypeCNAME
	// for aliases. CNAME records leave Address/Family empty and carry the
	// canonical FQDN in CNAMETarget.
	Type        string `json:"type,omitempty"`
	CNAMETarget string `json:"cname_target,omitempty"`
	// Source and AppliedAt are set only on dynamicstore-held records written
	// by the Netbox webhook trigger (see internal/netboxwebhook). They are
	// never read by zone generation or hashing (internal/zonegen) — only by
	// the webhook handler's ordering guard and the periodic-poll
	// reconciliation in cmd/sidecar/main.go.
	Source    string    `json:"source,omitempty"`
	AppliedAt time.Time `json:"applied_at,omitempty"`
}
```

Add `"time"` to `internal/netboxclient/client.go`'s import block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/zonegen/... ./internal/netboxclient/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/netboxclient/client.go internal/zonegen/generator_test.go
git commit -m "feat: add Source/AppliedAt metadata fields to IPRecord"
```

---

### Task 3: `DynamicStore.ReconcileWebhookSourced`

Removes webhook-sourced records that are provably superseded by a full Netbox poll, so a missed/duplicate webhook delivery can never cause permanent drift. Manually-added records (`Source == ""`) are never touched.

**Files:**
- Modify: `internal/dynamicstore/store.go` (interface method)
- Modify: `internal/dynamicstore/file_store.go` (implementation)
- Test: `internal/dynamicstore/file_store_test.go`

**Interfaces:**
- Consumes: `netboxclient.IPRecord.Source`, `.AppliedAt` (Task 2).
- Produces: `DynamicStore.ReconcileWebhookSourced(cutoff time.Time) error` — called by Task 8 (`cmd/sidecar/main.go`) after every full Netbox poll.

- [ ] **Step 1: Write the failing test**

Add to `internal/dynamicstore/file_store_test.go`:

```go
func TestReconcileWebhookSourced_DropsOnlyStaleWebhookRecords(t *testing.T) {
	s := newStore(t)
	cutoff := time.Now()

	require.NoError(t, s.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "stale-webhook.example.org", Address: "10.0.0.1", Family: 4,
			Source: netboxclient.SourceWebhook, AppliedAt: cutoff.Add(-time.Minute)},
		{DNSName: "fresh-webhook.example.org", Address: "10.0.0.2", Family: 4,
			Source: netboxclient.SourceWebhook, AppliedAt: cutoff.Add(time.Minute)},
		{DNSName: "manual.example.org", Address: "10.0.0.3", Family: 4},
	}))

	require.NoError(t, s.ReconcileWebhookSourced(cutoff))

	got := s.GetRecords("example.org")
	names := make([]string, 0, len(got))
	for _, r := range got {
		names = append(names, r.DNSName)
	}
	assert.ElementsMatch(t, []string{"fresh-webhook.example.org", "manual.example.org"}, names)
}

func TestReconcileWebhookSourced_NoOpWhenNothingStale(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.UpsertRecords("example.org", []netboxclient.IPRecord{
		{DNSName: "manual.example.org", Address: "10.0.0.1", Family: 4},
	}))
	require.NoError(t, s.ReconcileWebhookSourced(time.Now()))
	assert.Len(t, s.GetRecords("example.org"), 1)
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dynamicstore/... -run TestReconcileWebhookSourced -v`
Expected: FAIL — `s.ReconcileWebhookSourced undefined`

- [ ] **Step 3: Add the interface method and implementation**

In `internal/dynamicstore/store.go`, add to the `DynamicStore` interface:

```go
package dynamicstore

import (
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// DynamicStore persists and retrieves dynamically provisioned DNS zones and records.
type DynamicStore interface {
	CreateZone(zone string) error
	DeleteZone(zone string) error
	ListZones() []string
	GetRecords(zone string) []netboxclient.IPRecord
	UpsertRecords(zone string, records []netboxclient.IPRecord) error
	DeleteRecords(zone string, names []string) error
	BatchUpsert(zoneRecords map[string][]netboxclient.IPRecord) error
	BatchDelete(zone string, names []string) error
	// ReconcileWebhookSourced removes records with Source == netboxclient.SourceWebhook
	// whose AppliedAt predates cutoff. Called after a full Netbox poll completes:
	// the fresh fetch has already proven to include (or supersede) any such
	// record, so the webhook overlay entry is now redundant. Records with
	// Source == "" (manually added, or never touched by the webhook) are
	// never affected.
	ReconcileWebhookSourced(cutoff time.Time) error
}
```

In `internal/dynamicstore/file_store.go`, add the implementation (place after `BatchDelete`, before `upsert`):

```go
// ReconcileWebhookSourced removes records with Source == netboxclient.SourceWebhook
// whose AppliedAt is before cutoff, across all zones. Caller-visible zones with
// no matching records are left untouched (no-op, no persist).
func (f *FileStore) ReconcileWebhookSourced(cutoff time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	changed := false
	for zone, recs := range f.data.Zones {
		out := recs[:0:0]
		for _, r := range recs {
			if r.Source == netboxclient.SourceWebhook && r.AppliedAt.Before(cutoff) {
				changed = true
				continue
			}
			out = append(out, r)
		}
		f.data.Zones[zone] = out
	}

	if !changed {
		return nil
	}
	return f.persist()
}
```

Add `"time"` to `internal/dynamicstore/file_store.go`'s import block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dynamicstore/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/dynamicstore/store.go internal/dynamicstore/file_store.go internal/dynamicstore/file_store_test.go
git commit -m "feat: add ReconcileWebhookSourced to DynamicStore"
```

---

### Task 4: `internal/netboxwebhook` — signature verification

**Files:**
- Create: `internal/netboxwebhook/signature.go`
- Create: `internal/netboxwebhook/signature_test.go`
- Create: `internal/netboxwebhook/testdata/created.json`, `testdata/updated_rename.json`, `testdata/deleted.json` (real captures from a Netbox v4.6.0 instance, secret `test-webhook-secret-123` — see issue #27 comment)

**Interfaces:**
- Produces: `netboxwebhook.verifySignature(secret string, body []byte, header string) bool` — used by Task 6.

- [ ] **Step 1: Create the fixture files**

Create `internal/netboxwebhook/testdata/created.json` (exact capture, secret `test-webhook-secret-123`):

```json
{"event": "created", "timestamp": "2026-07-12T07:59:53.594467+00:00", "object_type": "ipam.ipaddress", "username": "admin", "request_id": "e9c884e8-8fc5-4e4f-8594-335f0c9d0602", "data": {"id": 18005, "url": "/api/ipam/ip-addresses/18005/", "display_url": "/ipam/ip-addresses/18005/", "display": "10.99.99.5/32", "family": {"value": 4, "label": "IPv4"}, "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": {"value": "active", "label": "Active"}, "role": null, "assigned_object_type": null, "assigned_object_id": null, "assigned_object": null, "nat_inside": null, "nat_outside": [], "dns_name": "webhook-test-1.mycompany.com", "description": "", "owner": null, "comments": "", "tags": [], "custom_fields": {}, "created": "2026-07-12T07:59:53.559444Z", "last_updated": "2026-07-12T07:59:53.559456Z"}, "request": {"id": "e9c884e8-8fc5-4e4f-8594-335f0c9d0602", "method": "POST", "path": "/api/ipam/ip-addresses/", "user": "admin"}, "snapshots": {"prechange": null, "postchange": {"created": "2026-07-12T07:59:53.559Z", "owner": null, "description": "", "comments": "", "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": "active", "role": null, "assigned_object_type": null, "assigned_object_id": null, "nat_inside": null, "dns_name": "webhook-test-1.mycompany.com", "custom_fields": {}, "tags": []}}}
```

Create `internal/netboxwebhook/testdata/updated_rename.json`:

```json
{"event": "updated", "timestamp": "2026-07-12T07:59:55.755931+00:00", "object_type": "ipam.ipaddress", "username": "admin", "request_id": "901b32db-3426-445b-ae67-b857811f5313", "data": {"id": 18005, "url": "/api/ipam/ip-addresses/18005/", "display_url": "/ipam/ip-addresses/18005/", "display": "10.99.99.5/32", "family": {"value": 4, "label": "IPv4"}, "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": {"value": "active", "label": "Active"}, "role": null, "assigned_object_type": null, "assigned_object_id": null, "assigned_object": null, "nat_inside": null, "nat_outside": [], "dns_name": "webhook-test-1-renamed.mycompany.com", "description": "", "owner": null, "comments": "", "tags": [], "custom_fields": {}, "created": "2026-07-12T07:59:53.559444Z", "last_updated": "2026-07-12T07:59:55.717069Z"}, "request": {"id": "901b32db-3426-445b-ae67-b857811f5313", "method": "PATCH", "path": "/api/ipam/ip-addresses/18005/", "user": "admin"}, "snapshots": {"prechange": {"created": "2026-07-12T07:59:53.559Z", "owner": null, "description": "", "comments": "", "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": "active", "role": null, "assigned_object_type": null, "assigned_object_id": null, "nat_inside": null, "dns_name": "webhook-test-1.mycompany.com", "custom_fields": {}, "tags": []}, "postchange": {"created": "2026-07-12T07:59:53.559Z", "owner": null, "description": "", "comments": "", "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": "active", "role": null, "assigned_object_type": null, "assigned_object_id": null, "nat_inside": null, "dns_name": "webhook-test-1-renamed.mycompany.com", "custom_fields": {}, "tags": []}}}
```

Create `internal/netboxwebhook/testdata/deleted.json`:

```json
{"event": "deleted", "timestamp": "2026-07-12T07:59:57.923646+00:00", "object_type": "ipam.ipaddress", "username": "admin", "request_id": "701f7a37-7cf8-4c56-a74d-8364efc0afa1", "data": {"id": 18005, "url": "/api/ipam/ip-addresses/18005/", "display_url": "/ipam/ip-addresses/18005/", "display": "10.99.99.5/32", "family": {"value": 4, "label": "IPv4"}, "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": {"value": "active", "label": "Active"}, "role": null, "assigned_object_type": null, "assigned_object_id": null, "assigned_object": null, "nat_inside": null, "nat_outside": [], "dns_name": "webhook-test-1-renamed.mycompany.com", "description": "", "owner": null, "comments": "", "tags": [], "custom_fields": {}, "created": "2026-07-12T07:59:53.559444Z", "last_updated": "2026-07-12T07:59:55.717069Z"}, "request": {"id": "701f7a37-7cf8-4c56-a74d-8364efc0afa1", "method": "DELETE", "path": "/api/ipam/ip-addresses/18005/", "user": "admin"}, "snapshots": {"prechange": {"created": "2026-07-12T07:59:53.559Z", "owner": null, "description": "", "comments": "", "address": "10.99.99.5/32", "vrf": null, "tenant": null, "status": "active", "role": null, "assigned_object_type": null, "assigned_object_id": null, "nat_inside": null, "dns_name": "webhook-test-1-renamed.mycompany.com", "custom_fields": {}, "tags": []}, "postchange": null}}
```

- [ ] **Step 2: Write the failing test**

Create `internal/netboxwebhook/signature_test.go`:

```go
package netboxwebhook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "test-webhook-secret-123"

// realSignature is the exact X-Hook-Signature Netbox sent for testdata/created.json,
// captured live against a Netbox v4.6.0 instance (see issue #27 comment).
const realSignature = "f756f462809c6cb1a7b4d3a47a7ea6216a3f7ca84aa411f5bb2bc95439831868ae7a89675dcbefe9d07aff8647d53a31b6d75a0f9c720c60660e44946e8eb968"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

func TestVerifySignature_RealNetboxCapture(t *testing.T) {
	body := readFixture(t, "created.json")
	assert.True(t, verifySignature(testSecret, body, realSignature))
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	body := readFixture(t, "created.json")
	assert.False(t, verifySignature("wrong-secret", body, realSignature))
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	body := readFixture(t, "created.json")
	body = append(body, ' ') // any mutation invalidates the signature
	assert.False(t, verifySignature(testSecret, body, realSignature))
}

func TestVerifySignature_MissingHeader(t *testing.T) {
	body := readFixture(t, "created.json")
	assert.False(t, verifySignature(testSecret, body, ""))
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/netboxwebhook/... -v`
Expected: FAIL — build error, `verifySignature` undefined (package doesn't exist yet)

- [ ] **Step 4: Implement signature verification**

Create `internal/netboxwebhook/signature.go`:

```go
// Package netboxwebhook implements a direct HTTP webhook receiver for Netbox
// ipam.ipaddress create/update/delete events. It replaces the
// webhook-to-message-broker relay originally proposed in issue #27: Netbox's
// built-in webhook feature posts straight to this handler, HMAC-signed, and
// changes are applied to internal/dynamicstore without a full Netbox re-fetch.
package netboxwebhook

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
)

// verifySignature reports whether header is the correct HMAC-SHA512 hex
// digest of body under secret, matching Netbox's X-Hook-Signature scheme
// (verified against a real Netbox v4.6.0 delivery — see issue #27 comment).
func verifySignature(secret string, body []byte, header string) bool {
	if header == "" {
		return false
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/netboxwebhook/... -v`
Expected: PASS — all 4 tests

- [ ] **Step 6: Commit**

```bash
git add internal/netboxwebhook/
git commit -m "feat: add Netbox webhook HMAC signature verification"
```

---

### Task 5: Payload parsing

**Files:**
- Create: `internal/netboxwebhook/payload.go`
- Create: `internal/netboxwebhook/payload_test.go`

**Interfaces:**
- Consumes: `netboxclient.RecordFromJSON` (Task 1), fixture files from Task 4.
- Produces: `netboxwebhook.payload` struct, `netboxwebhook.parsePayload(body []byte) (payload, error)` — used by Task 6.

- [ ] **Step 1: Write the failing test**

Create `internal/netboxwebhook/payload_test.go`:

```go
package netboxwebhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePayload_Created(t *testing.T) {
	p, err := parsePayload(readFixture(t, "created.json"))
	require.NoError(t, err)
	assert.Equal(t, "created", p.Event)
	assert.Equal(t, "ipam.ipaddress", p.ObjectType)
	assert.Nil(t, p.Snapshots.PreChange)

	rec, err := recordFromPayload(p)
	require.NoError(t, err)
	assert.Equal(t, "webhook-test-1.mycompany.com", rec.DNSName)
	assert.Equal(t, "10.99.99.5", rec.Address)
}

func TestParsePayload_UpdatedRename(t *testing.T) {
	p, err := parsePayload(readFixture(t, "updated_rename.json"))
	require.NoError(t, err)
	assert.Equal(t, "updated", p.Event)
	require.NotNil(t, p.Snapshots.PreChange)
	assert.Equal(t, "webhook-test-1.mycompany.com", p.Snapshots.PreChange.DNSName)

	rec, err := recordFromPayload(p)
	require.NoError(t, err)
	assert.Equal(t, "webhook-test-1-renamed.mycompany.com", rec.DNSName)
}

func TestParsePayload_Deleted(t *testing.T) {
	p, err := parsePayload(readFixture(t, "deleted.json"))
	require.NoError(t, err)
	assert.Equal(t, "deleted", p.Event)
	require.NotNil(t, p.Snapshots.PreChange)
	assert.Equal(t, "webhook-test-1-renamed.mycompany.com", p.Snapshots.PreChange.DNSName)
}

func TestParsePayload_Malformed(t *testing.T) {
	_, err := parsePayload([]byte(`not-json`))
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netboxwebhook/... -run TestParsePayload -v`
Expected: FAIL — `parsePayload undefined`

- [ ] **Step 3: Implement payload parsing**

Create `internal/netboxwebhook/payload.go`:

```go
package netboxwebhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
)

// snapshot is the subset of Netbox's snapshots.prechange/postchange objects
// this package reads. Unlike the top-level "data" object, snapshot fields are
// flat (e.g. assigned_object_id, not a nested assigned_object) — confirmed
// against a real Netbox v4.6.0 capture — but only dns_name is needed here.
type snapshot struct {
	DNSName string `json:"dns_name"`
}

// payload is a Netbox event-rule webhook delivery for an ipam.ipaddress
// create/update/delete event.
type payload struct {
	Event      string          `json:"event"` // "created", "updated", "deleted"
	Timestamp  time.Time       `json:"timestamp"`
	ObjectType string          `json:"object_type"`
	Data       json.RawMessage `json:"data"` // null on "deleted" events
	Snapshots  struct {
		PreChange *snapshot `json:"prechange"`
	} `json:"snapshots"`
}

func parsePayload(body []byte) (payload, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return payload{}, fmt.Errorf("decode webhook payload: %w", err)
	}
	return p, nil
}

// recordFromPayload builds an IPRecord from a created/updated payload's data
// field. Callers must not call this for "deleted" events (data is null).
func recordFromPayload(p payload) (netboxclient.IPRecord, error) {
	rec, err := netboxclient.RecordFromJSON(p.Data)
	if err != nil {
		return netboxclient.IPRecord{}, err
	}
	rec.Source = netboxclient.SourceWebhook
	rec.AppliedAt = p.Timestamp
	return rec, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/netboxwebhook/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/netboxwebhook/payload.go internal/netboxwebhook/payload_test.go
git commit -m "feat: parse Netbox webhook payloads into IPRecords"
```

---

### Task 6: Metrics + HTTP handler + zone routing + ordering guard

**Files:**
- Modify: `internal/metrics/metrics.go` (new metrics)
- Modify: `internal/metrics/metrics_test.go` (if it asserts an exact metric count/list — check before editing; otherwise no change needed)
- Create: `internal/netboxwebhook/handler.go`
- Create: `internal/netboxwebhook/handler_test.go`

**Interfaces:**
- Consumes: `dynamicstore.DynamicStore` (Task 3), `zonediscovery.Discoverer`, `metrics.Sidecar`, `netboxwebhook.verifySignature`/`parsePayload`/`recordFromPayload` (Tasks 4-5).
- Produces: `netboxwebhook.Register(mux *http.ServeMux, secret string, store dynamicstore.DynamicStore, disc zonediscovery.Discoverer, mergeSignal chan<- struct{}, m *metrics.Sidecar)`, `netboxwebhook.Path = "/webhook/netbox"` — used by Task 8.

- [ ] **Step 1: Check whether `metrics_test.go` asserts an exact metric list**

Run: `grep -n "MustRegister\|len(\|WebhookRequestsTotal" internal/metrics/metrics_test.go`

If it enumerates all metric names/count, note the pattern so Step 3 stays consistent with it. If not, no change is needed there.

- [ ] **Step 2: Write the failing test**

Create `internal/netboxwebhook/handler_test.go`:

```go
package netboxwebhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func newTestServer(t *testing.T) (*httptest.Server, dynamicstore.DynamicStore, chan struct{}) {
	t.Helper()
	store, err := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	require.NoError(t, err)
	disc := &zonediscovery.ZoneDepthDiscoverer{Depth: 2}
	m := metrics.NewSidecar(prometheus.NewRegistry())
	mergeSignal := make(chan struct{}, 1)

	mux := http.NewServeMux()
	Register(mux, testSecret, store, disc, mergeSignal, m)
	return httptest.NewServer(mux), store, mergeSignal
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
	srv, store, mergeSignal := newTestServer(t)
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
	srv, store, _ := newTestServer(t)
	defer srv.Close()

	resp := postSigned(t, srv, readFixture(t, "created.json"), "wrong-secret")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))
}

func TestHandler_RenameEvent_DeletesOldName(t *testing.T) {
	srv, store, _ := newTestServer(t)
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
	srv, store, _ := newTestServer(t)
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
	srv, store, _ := newTestServer(t)
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
	srv, store, _ := newTestServer(t)
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
	srv, store, _ := newTestServer(t)
	defer srv.Close()

	body := []byte(`{"event":"created","timestamp":"2026-07-12T08:00:00Z","object_type":"dcim.device","data":{}}`)
	resp := postSigned(t, srv, body, testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))
}

func TestHandler_EmptyDNSName_Ignored(t *testing.T) {
	srv, store, _ := newTestServer(t)
	defer srv.Close()

	body := []byte(`{"event":"created","timestamp":"2026-07-12T08:00:00Z","object_type":"ipam.ipaddress","data":{"address":"10.0.0.9/32","dns_name":""}}`)
	resp := postSigned(t, srv, body, testSecret)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, store.GetRecords("mycompany.com"))
}

func TestRegister_EmptySecretDoesNotRegisterRoute(t *testing.T) {
	store, err := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	require.NoError(t, err)
	disc := &zonediscovery.ZoneDepthDiscoverer{Depth: 2}
	m := metrics.NewSidecar(prometheus.NewRegistry())
	mux := http.NewServeMux()
	Register(mux, "", store, disc, make(chan struct{}, 1), m)

	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+Path, "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/netboxwebhook/... -v`
Expected: FAIL — `Register`/`Path` undefined, and `metrics.Sidecar` has no field `WebhookRequestsTotal`/`WebhookLastEventTimestamp`

- [ ] **Step 4: Add the new metrics**

In `internal/metrics/metrics.go`, add to the `Sidecar` struct:

```go
	CoreDNSReloadDirtyTargets   prometheus.Gauge
	WebhookRequestsTotal        *prometheus.CounterVec
	WebhookLastEventTimestamp   prometheus.Gauge
}
```

Add to `NewSidecar`, after the `CoreDNSReloadDirtyTargets` initializer:

```go
		CoreDNSReloadDirtyTargets: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_coredns_reload_dirty_targets",
			Help: "CoreDNS addresses whose last reload push has not succeeded yet. Non-zero for more than a few poll cycles means a pod is not accepting reloads.",
		}),

		WebhookRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "netbox_sidecar_webhook_requests_total",
			Help: "Total Netbox webhook requests received, partitioned by result.",
		}, []string{"result"}),

		WebhookLastEventTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_webhook_last_event_timestamp_seconds",
			Help: "Unix timestamp of the last successfully applied Netbox webhook event.",
		}),
	}
```

And add both new metrics to the `reg.MustRegister(...)` call:

```go
	reg.MustRegister(
		m.PollTotal,
		m.PollDurationSeconds,
		m.LastSuccessfulPollTimestamp,
		m.NetboxFetchDurationSeconds,
		m.NetboxRecordsFetched,
		m.NetboxEmptyResponseTotal,
		m.ZonesActive,
		m.ZoneWritesTotal,
		m.ZoneWriteErrorsTotal,
		m.NetboxFetchRetriesTotal,
		m.ZoneStalenessSeconds,
		m.CoreDNSReloadTotal,
		m.CoreDNSReloadRetriesTotal,
		m.CoreDNSReloadDirtyTargets,
		m.WebhookRequestsTotal,
		m.WebhookLastEventTimestamp,
	)
```

- [ ] **Step 5: Implement the handler**

Create `internal/netboxwebhook/handler.go`:

```go
package netboxwebhook

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

// Path is the HTTP route the Netbox webhook posts to.
const Path = "/webhook/netbox"

// maxBodyBytes caps the request body Netbox can send us. A single
// ipam.ipaddress webhook payload is a few KB; this leaves generous headroom
// while bounding worst-case memory use per request.
const maxBodyBytes = 64 * 1024

const objectTypeIPAddress = "ipam.ipaddress"

// Register adds the Netbox webhook route to mux. If secret is empty, no
// route is registered at all — there is never an accidental unauthenticated
// trigger endpoint.
func Register(mux *http.ServeMux, secret string, store dynamicstore.DynamicStore, disc zonediscovery.Discoverer, mergeSignal chan<- struct{}, m *metrics.Sidecar) {
	if secret == "" {
		return
	}
	h := &handler{secret: secret, store: store, disc: disc, mergeSignal: mergeSignal, m: m}
	mux.HandleFunc(Path, h.ServeHTTP)
}

type handler struct {
	secret      string
	store       dynamicstore.DynamicStore
	disc        zonediscovery.Discoverer
	mergeSignal chan<- struct{}
	m           *metrics.Sidecar
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.m.WebhookRequestsTotal.WithLabelValues("bad_payload").Inc()
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}

	if !verifySignature(h.secret, body, r.Header.Get("X-Hook-Signature")) {
		h.m.WebhookRequestsTotal.WithLabelValues("invalid_signature").Inc()
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	p, err := parsePayload(body)
	if err != nil {
		h.m.WebhookRequestsTotal.WithLabelValues("bad_payload").Inc()
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	if p.ObjectType != objectTypeIPAddress {
		h.m.WebhookRequestsTotal.WithLabelValues("unsupported_model").Inc()
		slog.Debug("netboxwebhook: ignoring unsupported object type", "object_type", p.ObjectType)
		w.WriteHeader(http.StatusOK)
		return
	}

	applied, err := h.apply(p)
	if err != nil {
		h.m.WebhookRequestsTotal.WithLabelValues("error").Inc()
		slog.Warn("netboxwebhook: failed to apply event", "event", p.Event, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if applied {
		h.m.WebhookRequestsTotal.WithLabelValues("ok").Inc()
		h.m.WebhookLastEventTimestamp.SetToCurrentTime()
		select {
		case h.mergeSignal <- struct{}{}:
		default:
		}
	} else {
		h.m.WebhookRequestsTotal.WithLabelValues("stale").Inc()
	}
	w.WriteHeader(http.StatusOK)
}

// apply routes the event to the right store mutation. Returns applied=true
// only when a dynamicstore write actually happened (used to decide whether to
// signal a re-merge).
func (h *handler) apply(p payload) (applied bool, err error) {
	switch p.Event {
	case "deleted":
		return h.applyDelete(p)
	case "created", "updated":
		return h.applyUpsert(p)
	default:
		slog.Debug("netboxwebhook: ignoring unknown event type", "event", p.Event)
		return false, nil
	}
}

func (h *handler) applyDelete(p payload) (bool, error) {
	if p.Snapshots.PreChange == nil || p.Snapshots.PreChange.DNSName == "" {
		return false, nil
	}
	name := p.Snapshots.PreChange.DNSName
	zone, ok := h.zoneFor(name)
	if !ok {
		return false, nil
	}
	if err := h.store.DeleteRecords(zone, []string{name}); err != nil {
		return false, err
	}
	return true, nil
}

func (h *handler) applyUpsert(p payload) (bool, error) {
	rec, err := recordFromPayload(p)
	if err != nil {
		return false, err
	}
	if rec.DNSName == "" {
		// Device-based DNS name enrichment is not applied via the webhook
		// path (see Global Constraints in the design doc) — the periodic
		// poll will pick this up.
		slog.Debug("netboxwebhook: ignoring event with no dns_name")
		return false, nil
	}

	// Rename: the old name may live in a different zone bucket.
	if p.Event == "updated" && p.Snapshots.PreChange != nil &&
		p.Snapshots.PreChange.DNSName != "" && p.Snapshots.PreChange.DNSName != rec.DNSName {
		if oldZone, ok := h.zoneFor(p.Snapshots.PreChange.DNSName); ok {
			if err := h.store.DeleteRecords(oldZone, []string{p.Snapshots.PreChange.DNSName}); err != nil {
				return false, err
			}
		}
	}

	zone, ok := h.zoneFor(rec.DNSName)
	if !ok {
		slog.Warn("netboxwebhook: dns_name does not map to any configured zone", "dns_name", rec.DNSName)
		return false, nil
	}

	return h.upsertGuarded(zone, rec)
}

// upsertGuarded applies rec unless a newer-or-equal webhook-sourced write
// already exists for the same name, or the name is pinned by a manually-added
// (non-webhook) record — in both cases the write is skipped rather than
// clobbering a more authoritative entry.
func (h *handler) upsertGuarded(zone string, rec netboxclient.IPRecord) (bool, error) {
	for _, existing := range h.store.GetRecords(zone) {
		if existing.DNSName != rec.DNSName {
			continue
		}
		if existing.Source != netboxclient.SourceWebhook {
			slog.Warn("netboxwebhook: skipping write, name is pinned by a manually-added record", "dns_name", rec.DNSName)
			return false, nil
		}
		if !rec.AppliedAt.After(existing.AppliedAt) {
			slog.Debug("netboxwebhook: skipping stale/out-of-order event", "dns_name", rec.DNSName)
			return false, nil
		}
		break
	}
	if err := h.store.UpsertRecords(zone, []netboxclient.IPRecord{rec}); err != nil {
		return false, err
	}
	return true, nil
}

// zoneFor resolves the configured zone a DNS name belongs to, reusing the
// same Discoverer the full Netbox fetch uses.
func (h *handler) zoneFor(dnsName string) (string, bool) {
	zones, err := h.disc.Discover([]netboxclient.IPRecord{{DNSName: dnsName}})
	if err != nil {
		slog.Warn("netboxwebhook: zone discovery failed", "dns_name", dnsName, "err", err)
		return "", false
	}
	for zone := range zones {
		return zone, true
	}
	return "", false
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/netboxwebhook/... ./internal/metrics/... -v`
Expected: PASS — all handler tests plus existing metrics tests

- [ ] **Step 7: Commit**

```bash
git add internal/metrics/metrics.go internal/netboxwebhook/handler.go internal/netboxwebhook/handler_test.go
git commit -m "feat: add Netbox webhook HTTP handler with ordering guard"
```

---

### Task 7: Config — `NETBOX_WEBHOOK_SECRET`

**Files:**
- Modify: `internal/config/config.go`
- Test: check for an existing `internal/config/config_test.go` and follow its pattern

**Interfaces:**
- Produces: `config.Config.NetboxWebhookSecret string` — used by Task 8.

- [ ] **Step 1: Inspect the existing config test pattern**

Run: `grep -n "func Test" internal/config/config_test.go | head -20`

- [ ] **Step 2: Write the failing test**

Add to `internal/config/config_test.go` (following whatever pattern the existing tests use for setting env vars via `t.Setenv` and calling `config.Load()`):

```go
func TestLoad_NetboxWebhookSecret(t *testing.T) {
	t.Setenv("NETBOX_TOKEN", "test-token")
	t.Setenv("NETBOX_WEBHOOK_SECRET", "shh-its-a-secret")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "shh-its-a-secret", cfg.NetboxWebhookSecret)
}

func TestLoad_NetboxWebhookSecret_DefaultsEmpty(t *testing.T) {
	t.Setenv("NETBOX_TOKEN", "test-token")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.NetboxWebhookSecret, "webhook route must be disabled by default")
}
```

(Add `"github.com/stretchr/testify/require"` to imports if the file doesn't already import it.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/config/... -run TestLoad_NetboxWebhookSecret -v`
Expected: FAIL — `cfg.NetboxWebhookSecret undefined`

- [ ] **Step 4: Add the config field**

In `internal/config/config.go`, add to the `Config` struct (near `GRPCAuthToken`):

```go
	// gRPC server configuration
	GRPCAddr      string
	GRPCAuthToken string

	// Netbox webhook trigger: HMAC secret for the /webhook/netbox route.
	// Empty disables the route entirely (see internal/netboxwebhook.Register).
	NetboxWebhookSecret string
```

Add to `Load()`'s struct literal, near `GRPCAuthToken`:

```go
		// gRPC server configuration
		GRPCAddr:      envOrDefault("GRPC_ADDR", ":8083"),
		GRPCAuthToken: os.Getenv("GRPC_AUTH_TOKEN"),

		NetboxWebhookSecret: os.Getenv("NETBOX_WEBHOOK_SECRET"),
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add NETBOX_WEBHOOK_SECRET config option"
```

---

### Task 8: Wire into `cmd/sidecar/main.go`

**Files:**
- Modify: `cmd/sidecar/main.go`
- Test: `cmd/sidecar/main_test.go` (check existing patterns; this task is mostly wiring, verified primarily by Task 6/7's unit tests plus the e2e test in Task 9)

**Interfaces:**
- Consumes: `netboxwebhook.Register` (Task 6), `cfg.NetboxWebhookSecret` (Task 7), `store.ReconcileWebhookSourced` (Task 3).

- [ ] **Step 1: Register the webhook route**

In `cmd/sidecar/main.go`, add the import:

```go
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/netboxwebhook"
	"github.com/pallotron/coredns-netbox/internal/reloader"
```

In the health-server block (around line 199-203), register the route right after `zoneserver.Register`:

```go
		mux := http.NewServeMux()
		registerHealth(mux, &healthy)
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		zoneserver.Register(mux, cfg.ZoneDir)
		netboxwebhook.Register(mux, cfg.NetboxWebhookSecret, store, forwardDisc, mergeSignal, m)
		srv := &http.Server{Addr: cfg.HealthAddr, Handler: mux}
```

- [ ] **Step 2: Add reconciliation to the full-poll path**

In `cmd/sidecar/main.go`'s `run()` function, replace the `doFetchNetbox` closure (originally at lines 296-305) with:

```go
	doFetchNetbox := func() (zonediscovery.ZoneMap, error) {
		fetchStart := time.Now()
		zm, err := fetchNetbox(ctx, client, categorizer, formatter, forwardDisc, reverseDisc, m, cfg.DomainSuffix)
		if err != nil {
			return zm, err
		}
		if cfg.StripDCLabel {
			for zone, recs := range zm {
				zm[zone] = zonediscovery.StripDCLabel(recs, cfg.DomainSuffix)
			}
		}
		// A full Netbox fetch that started at fetchStart has, by definition,
		// already captured any webhook-sourced change applied before that
		// moment — so any such overlay entry is now redundant. This is what
		// keeps a missed/duplicate webhook delivery from causing permanent
		// drift (see internal/netboxwebhook and ReconcileWebhookSourced).
		if err := store.ReconcileWebhookSourced(fetchStart); err != nil {
			slog.Warn("failed to reconcile webhook-sourced dynamic records", "err", err)
		}
		return zm, nil
	}
```

- [ ] **Step 3: Run the full unit test suite**

Run: `make test.unit`
Expected: PASS — all packages, including `cmd/sidecar`

- [ ] **Step 4: Commit**

```bash
git add cmd/sidecar/main.go
git commit -m "feat: wire Netbox webhook route and reconciliation into the sidecar"
```

---

### Task 9: e2e test against the real dev Netbox instance

Validates the whole path end-to-end: a real signed HTTP POST reaches a running sidecar pod, and the DNS answer changes without waiting for `POLL_INTERVAL`.

**Files:**
- Modify: `dev/coredns-netbox-values.yaml` (set `NETBOX_WEBHOOK_SECRET` via `sidecar.env`)
- Create: `tests/e2e/webhook_test.go`

**Interfaces:**
- Consumes: the running dev cluster (`make dev`), the sidecar's `HEALTH_ADDR` port exposed at `127.0.0.1:18082` per `docs/observability.md`.

- [ ] **Step 1: Enable the webhook route in the dev environment**

Read `dev/coredns-netbox-values.yaml` first to see its current `sidecar.env` block (if any), then add:

```yaml
sidecar:
  env:
    - name: NETBOX_WEBHOOK_SECRET
      value: "dev-webhook-secret"
```

(Merge into the existing `sidecar.env` list rather than duplicating the key if one already exists.)

- [ ] **Step 2: Write the e2e test**

Create `tests/e2e/webhook_test.go` (mirrors the `//go:build e2e` pattern in `tests/e2e/grpc_test.go`):

```go
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
```

- [ ] **Step 3: Redeploy the dev environment and run the e2e suite**

Run:
```bash
make dev.cluster dev.netbox dev.token dev.seed
make test.e2e.local
```
Expected: `TestWebhook_CreatedEventUpdatesDNSWithoutPoll` PASSes alongside the existing e2e suite.

- [ ] **Step 4: Tear down**

Run: `make dev.teardown`

- [ ] **Step 5: Commit**

```bash
git add dev/coredns-netbox-values.yaml tests/e2e/webhook_test.go
git commit -m "test: add e2e coverage for the Netbox webhook trigger"
```

---

### Task 10: Documentation

**Files:**
- Create: `docs/netbox-webhook.md`
- Modify: `docs/configuration.md`
- Modify: `docs/observability.md`

- [ ] **Step 1: Write the dedicated doc**

Create `docs/netbox-webhook.md`:

```markdown
# Netbox Webhook Trigger

The sidecar can apply Netbox `ipam.ipaddress` changes immediately, via a webhook Netbox posts directly to the sidecar — no message broker, no full Netbox re-fetch for the common case.

## How it works

```
Netbox (ipam.ipaddress webhook, HMAC-signed)
    → POST /webhook/netbox on the sidecar's HEALTH_ADDR port
    → verify X-Hook-Signature (HMAC-SHA512)
    → parse event (created/updated/deleted)
    → write directly into the dynamic record store
    → trigger the same debounced re-merge + zone write + CoreDNS reload
      path the gRPC DynamicZoneService API already uses
```

The periodic poll (`POLL_INTERVAL`) keeps running unchanged as a reconciliation backstop — it is not required for correctness, only for records outside the webhook's scope (see Limitations) and for recovering from a missed webhook delivery.

## Enabling it

Set `NETBOX_WEBHOOK_SECRET` to a shared secret and configure a matching Netbox webhook + event rule:

- Webhook `payload_url`: `http://<sidecar-service>:8082/webhook/netbox`
- Webhook `secret`: same value as `NETBOX_WEBHOOK_SECRET`
- Event rule `object_types`: `ipam.ipaddress`
- Event rule `event_types`: `object_created`, `object_updated`, `object_deleted`

If `NETBOX_WEBHOOK_SECRET` is unset, the `/webhook/netbox` route is not registered at all — there is no way to accidentally expose an unauthenticated trigger endpoint.

## Limitations

- **Only records with `dns_name` already set in Netbox are handled.** Device-based DNS name generation (`ipcategorizer`/`nameformat`, used when `dns_name` is empty) needs cross-record context — such as CNAME alias collision resolution across an entire zone — that a single webhook event doesn't have. Those records are only refreshed by the periodic poll.
- **Concurrent/out-of-order delivery is handled, not prevented.** Netbox dispatches webhooks via a background worker queue, so a burst of changes (e.g. one bulk edit) can arrive out of order. Each webhook-sourced record carries the originating event's timestamp; an older event can never overwrite an already-applied newer one for the same name.
- **A webhook-sourced record never permanently shadows Netbox.** It's automatically cleared once the next full poll proves it's already reflected there — so a dropped webhook delivery self-heals on the next poll cycle rather than leaving stale data indefinitely.
- **A manually-added record (via the `DynamicZoneService` gRPC API) always wins over a webhook-sourced one for the same name.** The webhook write is skipped and logged at `WARN`.
- In `netbox-dns` discovery mode, zone routing for a webhook event makes its own Netbox API call per event (same `Discoverer.Discover` used by the full fetch) — this does not benefit from the same API-call reduction as `zone-depth`/`common-suffix` mode.
```

- [ ] **Step 2: Add to `docs/configuration.md`**

Add a new row to the environment variable table (after the `GRPC_AUTH_TOKEN` row, around line 205):

```markdown
| `NETBOX_WEBHOOK_SECRET` | `""` | HMAC secret for the `/webhook/netbox` route; empty disables the route entirely. See [Netbox Webhook Trigger](netbox-webhook.md). |
```

- [ ] **Step 3: Add to `docs/observability.md`**

Add two rows to the Prometheus metrics table (after `netbox_sidecar_coredns_reload_dirty_targets`, around line 33):

```markdown
| `netbox_sidecar_webhook_requests_total` | Counter | `result=ok\|invalid_signature\|bad_payload\|stale\|unsupported_model\|error` | Netbox webhook requests received, partitioned by outcome |
| `netbox_sidecar_webhook_last_event_timestamp_seconds` | Gauge | — | Unix timestamp of the last successfully applied webhook event |
```

- [ ] **Step 4: Commit**

```bash
git add docs/netbox-webhook.md docs/configuration.md docs/observability.md
git commit -m "docs: document the Netbox webhook trigger"
```

---

### Task 11: Remove the plan doc before opening the PR

This plan file must not ship in the PR — it's a working document, not project documentation.

- [ ] **Step 1: Confirm everything else is committed**

Run: `git status`
Expected: only `docs/superpowers/plans/2026-07-12-netbox-webhook-trigger.md` is untracked/modified; everything else from Tasks 1-10 is already committed.

- [ ] **Step 2: Remove the plan file**

```bash
git rm docs/superpowers/plans/2026-07-12-netbox-webhook-trigger.md
```

- [ ] **Step 3: Commit as the final commit on the branch**

```bash
git commit -m "chore: remove implementation plan before PR"
```

- [ ] **Step 4: Open the PR**

At this point the branch is ready for `gh pr create` (see project conventions).
