# gRPC CRUD Interface for Dynamic DNS Zones and Records

**Date:** 2026-04-19  
**Status:** Approved

## Problem

The sidecar currently derives all DNS records exclusively from Netbox. A second source of records is needed: DNS entries for capacity managed by a Kubernetes platform (nodes, services) that should coexist in the same zones as Netbox-sourced records. Rather than encoding Kubernetes-specific logic into the sidecar, a generic gRPC interface allows any caller to provision dynamic records as an overlay on top of Netbox.

## Goals

- Full CRUD for dynamic zones and records via gRPC
- Batch upsert/delete for bulk provisioning
- Operational control plane (force Netbox poll, force merge+write, status)
- Dynamic records merged with Netbox records at zone-file write time; CoreDNS unchanged
- Bearer token authentication
- Dynamic records persisted to disk alongside zone files; survive pod restarts if PVC is enabled
- Unit and E2E tests

## Non-Goals

- Replacing the Netbox poll loop
- Supporting dynamic PTR (reverse) records in the first iteration
- Multi-tenant access control (single shared token for now)
- TLS on the gRPC listener (in-cluster only; mTLS is a future concern)

## Architecture

```
  cmd/sidecar
  +------------------+   +----------------+   +-------------------+
  |   poll loop      |   |  HTTP server   |   |   gRPC server     |
  |                  |   |  /healthz      |   |  DynamicZoneSvc   |
  |  [1] netbox      |   |  /metrics      |   |  ControlSvc       |
  |      fetch       |   +----------------+   |  (token intercept)|
  |        |         |                        +--------+----------+
  |        v         |                                 |
  |  [2] merge+write |<-- signal (channel) ------------+
  |        |         |
  +--------|----------+
           |   +-------------------------------------------+
           |   |  internal/dynamicstore                    |
           +-->|  DynamicStore interface                   |
               |  FileStore impl (ZONE_DIR/dynamic.json)   |
               +-------------------------------------------+
           |
           v
     RFC 1035 zone files (ZONE_DIR / same PVC)
           |
           v
     CoreDNS (file plugin -- unchanged)
```

### Merge strategy

Netbox records and dynamic records are merged at **zone-file write time** (step 2). The poll loop is decomposed into two independent steps:

- **Step 1 (`fetchNetbox`):** hits the Netbox API, updates an in-memory record cache. Runs on every `POLL_INTERVAL` tick, or when `ForceNetboxPoll` is called.
- **Step 2 (`mergeAndWrite`):** combines the cached Netbox records with the dynamic store contents, then writes zone files. Runs after every step 1, and also when triggered by a gRPC write via a buffered `chan struct{}` (capacity 1, non-blocking send so rapid writes coalesce).

Dynamic records in a zone that also exists in Netbox are additive — they do not shadow Netbox records by default. Dynamic records in a zone that does not exist in Netbox create that zone entirely.

**Conflict policy:** If a dynamic record's FQDN matches a record already present in the last known Netbox cache, `UpsertRecord` and `BatchUpsert` return `codes.AlreadyExists` unless the request includes `force: true`. When `force: true` the dynamic record takes precedence over the Netbox record in the merged zone file. The check is against the in-memory Netbox cache (no extra round-trip). After a `ForceNetboxPoll` the cache is refreshed, so stale-cache false positives are recoverable by the caller.

### Netbox failure behaviour

No change from existing behaviour. Zone files on disk already contain the last merged state (Netbox + dynamic). The existing `runOnceResult` / staleness logic covers this for free.

## New Packages & File Layout

```
internal/
  dynamicstore/
    store.go            # DynamicStore interface
    file_store.go       # FileStore: atomic JSON impl (ZONE_DIR/dynamic.json)
    file_store_test.go
  grpcserver/
    server.go           # listener, interceptors, service registration
    zone_service.go     # DynamicZoneService handler
    control_service.go  # ControlService handler
    server_test.go
proto/
  coredns_netbox/v1/
    zones.proto         # service and message definitions
    zones.pb.go         # generated (committed)
    zones_grpc.pb.go    # generated (committed)
cmd/sidecar/
  main.go               # adds gRPC goroutine, decomposes poll loop
docs/
  grpc-api.md           # dedicated gRPC API reference (new)
docs/superpowers/specs/
  2026-04-19-grpc-crud-design.md  # this document
```

## DynamicStore Interface

```go
type DynamicStore interface {
    ListZones() []string
    GetRecords(zone string) []netboxclient.IPRecord
    UpsertRecords(zone string, records []netboxclient.IPRecord) error
    DeleteRecords(zone string, names []string) error
    DeleteZone(zone string) error
    BatchUpsert(records map[string][]netboxclient.IPRecord) error
    BatchDelete(zone string, names []string) error
}
```

`FileStore` implements this interface, persisting state to `ZONE_DIR/dynamic.json` using atomic rename writes (same pattern as zone files). Protected by a `sync.RWMutex` for concurrent access from the gRPC handlers and the poll loop.

## Proto API

### DynamicZoneService

Owns dynamic-only zones and records. Does not interact with Netbox-sourced zones directly (though records may be merged into the same zone file at write time).

```protobuf
syntax = "proto3";
package coredns_netbox.v1;

service DynamicZoneService {
  rpc CreateZone(CreateZoneRequest)     returns (CreateZoneResponse);
  rpc DeleteZone(DeleteZoneRequest)     returns (DeleteZoneResponse);
  rpc ListZones(ListZonesRequest)       returns (ListZonesResponse);

  rpc UpsertRecord(UpsertRecordRequest) returns (UpsertRecordResponse);
  rpc DeleteRecord(DeleteRecordRequest) returns (DeleteRecordResponse);
  rpc ListRecords(ListRecordsRequest)   returns (ListRecordsResponse);

  rpc BatchUpsert(BatchUpsertRequest)   returns (BatchUpsertResponse);
  rpc BatchDelete(BatchDeleteRequest)   returns (BatchDeleteResponse);
}

message Record {
  string dns_name = 1; // FQDN e.g. "node1.k8s.example.org"
  string address  = 2; // IP address (no CIDR prefix)
  int32  family   = 3; // 4 or 6
  uint32 ttl      = 4; // per-record TTL in seconds; 0 means use global TTL from config
}

// force: if true, allows overriding a record already present in the Netbox cache.
// Default false — conflicts return codes.AlreadyExists.
message UpsertRecordRequest {
  Record record = 1;
  bool   force  = 2;
}

message BatchUpsertRequest {
  repeated Record records = 1;
  bool            force   = 2; // applies to all records in the batch
}
```

Each write RPC (Create, Upsert, Delete, Batch*) triggers step 2 (`mergeAndWrite`) via the signal channel after persisting to `dynamic.json`.

**Per-record TTL:** `Record.ttl = 0` means use the global `TTL` from config. Non-zero values are emitted inline on the record line in the zone file (e.g. `node1 60 IN A 10.0.0.1`), overriding the zone `$TTL`. This requires a small change to `internal/zonegen`: Netbox-sourced records (which carry no TTL today) continue to use `$TTL`; dynamic records with a non-zero TTL emit it inline. Per-record TTL is standard DNS (RFC 1035 §3.2.1).

### ControlService

Operational control plane for the whole sidecar (both Netbox and dynamic sources).

```protobuf
service ControlService {
  rpc ForceNetboxPoll(ForceNetboxPollRequest)   returns (ForceNetboxPollResponse);
  rpc ForceMergeWrite(ForceMergeWriteRequest)   returns (ForceMergeWriteResponse);
  rpc GetStatus(GetStatusRequest)               returns (GetStatusResponse);
}

message GetStatusResponse {
  int64  last_netbox_poll_unix    = 1;
  int64  last_merge_write_unix    = 2;
  int32  active_zones             = 3;
  int32  dynamic_record_count     = 4;
  double zone_staleness_seconds   = 5;
}
```

- `ForceNetboxPoll` — signals the poll loop to run both step 1 and step 2 immediately, bypassing `POLL_INTERVAL`.
- `ForceMergeWrite` — signals step 2 only (no Netbox round-trip).
- `GetStatus` — returns current sidecar state, useful for health checks and debugging.

## Authentication

A single gRPC server interceptor (applied to both unary and streaming calls) validates the `authorization` metadata key on every incoming RPC:

- Expected value: `bearer <token>` where token is the value of `GRPC_AUTH_TOKEN` env var
- Wrong or missing token → `codes.Unauthenticated`
- `GRPC_AUTH_TOKEN` unset or empty → auth disabled (local dev convenience)

Token is loaded from a Kubernetes `Secret` and mounted as an env var, consistent with how `NETBOX_TOKEN` is handled today.

## Configuration

Two new env vars added to `internal/config/config.go`:

| Env var           | Default  | Description                              |
|-------------------|----------|------------------------------------------|
| `GRPC_ADDR`       | `:8083`  | gRPC listen address                      |
| `GRPC_AUTH_TOKEN` | `""`     | Bearer token; empty disables auth        |

## Testing

### Unit tests (`make test.unit`)

**`internal/dynamicstore/file_store_test.go`:**
- CRUD operations (create zone, upsert records, delete records, delete zone)
- Concurrent read/write safety (run with `-race`)
- JSON persistence: store restarts read back the same records
- Atomic write: corrupt/partial writes do not corrupt state
- Missing/corrupt `dynamic.json` on startup is treated as empty store (not an error)

**`internal/grpcserver/server_test.go`:**
- Token interceptor rejects missing token → `codes.Unauthenticated`
- Token interceptor rejects wrong token → `codes.Unauthenticated`
- Token interceptor passes correct token
- Auth disabled when `GRPC_AUTH_TOKEN` is empty
- Each `DynamicZoneService` handler tested in-process (using `google.golang.org/grpc/test/bufconn`) with a mock `DynamicStore`
- `UpsertRecord` with `ttl=0` emits record using zone `$TTL`; non-zero TTL emits inline on the record line
- `UpsertRecord` with conflicting FQDN (in Netbox cache) returns `codes.AlreadyExists`
- `UpsertRecord` with `force: true` on a conflicting FQDN succeeds and dynamic record wins in merge
- `BatchUpsert` with mixed conflicts: without `force` the whole batch is rejected atomically (`codes.AlreadyExists`, conflicting FQDNs listed in error details); with `force` all records are written
- Each write handler sends on the merge signal channel
- `ControlService.GetStatus` returns correct values
- `ControlService.ForceNetboxPoll` signals the poll channel

### E2E tests (`make test.e2e`, `//go:build e2e`)

New file: `tests/e2e/grpc_test.go`

```go
func grpcAddr() string {
    if v := os.Getenv("GRPC_ADDR"); v != "" { return v }
    return "127.0.0.1:18083"
}
```

Test scenarios:
- `TestDynamicZoneCreate` — create zone, query DNS → `NOERROR`
- `TestDynamicRecordUpsert` — upsert A record, query DNS → correct IP
- `TestDynamicRecordDelete` — delete record, query DNS → `NXDOMAIN`
- `TestBatchUpsert` — bulk provision N records, verify all resolve
- `TestDynamicRecordSurvivesPoll` — upsert record, call `ForceNetboxPoll`, verify record still resolves (confirms merge survives a full Netbox poll)
- `TestForceNetboxPoll` — call `ControlService.ForceNetboxPoll`, verify `GetStatus.last_netbox_poll_unix` advances
- `TestForceMergeWrite` — call `ControlService.ForceMergeWrite`, verify `GetStatus.last_merge_write_unix` advances without a Netbox round-trip
- `TestAuthRejectsWrongToken` → `codes.Unauthenticated`
- `TestAuthRejectsMissingToken` → `codes.Unauthenticated`

`make test.e2e` picks up the new file automatically via `./tests/e2e/...`. The dev environment (`make dev`) needs `GRPC_ADDR` and `GRPC_AUTH_TOKEN` added to the sidecar env.

## Documentation

A new `docs/grpc-api.md` is added as part of implementation, covering:

**API reference:**
- How to connect and authenticate
- Full RPC reference with request/response fields
- Example `grpcurl` invocations for each RPC
- Kubernetes Secret setup for `GRPC_AUTH_TOKEN`

**Design decisions (the why, not just the what):**
- Why dynamic records are merged in the sidecar rather than using a different CoreDNS plugin (CoreDNS plugin chain does not support per-record fallback within a zone; `file` plugin owns the zone entirely)
- Why persistence is a flat JSON file in `ZONE_DIR` rather than an external store (consistent with existing PVC design; no new infrastructure dependency; `DynamicStore` interface allows swapping later)
- Why auth is a bearer token rather than mTLS (no cert-manager in the cluster; in-cluster only; mTLS is a future concern)
- Why gRPC write conflicts with Netbox records are rejected atomically with `codes.AlreadyExists` rather than partial success (partial success creates ambiguous state the caller cannot reason about without a follow-up `ListRecords`)
- Why `force: true` exists as an explicit override (legitimate emergency use case; makes intent visible in the API rather than silently shadowing)
- Why step 2 (merge+write) can be triggered independently of step 1 (Netbox fetch) — allows dynamic records to propagate in milliseconds without a Netbox round-trip
- Why per-record TTL is supported (RFC 1035 §3.2.1 standard; k8s use case needs short TTLs for ephemeral pod IPs and longer TTLs for stable service IPs; `ttl=0` means fall back to global config default, keeping it optional for callers that don't care)
