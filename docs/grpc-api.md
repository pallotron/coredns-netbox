# gRPC API Reference

## Overview

The `coredns-netbox` sidecar exposes a gRPC API that lets operators inject DNS records and zones without waiting for a Netbox poll cycle. Dynamic records are merged with Netbox-sourced records before being written to zone files, so CoreDNS serves them without any plugin configuration changes.

The merge model works as follows:

1. On each Netbox poll (or when `ForceNetboxPoll` is called), the sidecar fetches zones and records from Netbox.
2. Dynamic records held in the `DynamicStore` are merged on top of the Netbox data.
3. The merged result is written atomically to zone files on disk — this is the step triggered by `ForceMergeWrite`.
4. CoreDNS reloads the zone file automatically via the `file` plugin's `reload` interval.

Dynamic records survive Netbox polls: the store is never cleared by a poll cycle; only explicit `DeleteRecord` / `BatchDelete` / `DeleteZone` calls remove dynamic data.

## Connection

The gRPC server listens on the address configured by the `GRPC_ADDR` environment variable (default `0.0.0.0:8083`).

No TLS is configured — the server is intended for in-cluster use only, where pod-to-pod traffic is already isolated by Kubernetes network policies. Enabling mTLS is a future concern.

**Example dial address:** `<pod-ip>:8083` or `<service-name>:8083`

## Authentication

If the `GRPC_AUTH_TOKEN` environment variable is set, all RPCs require a bearer token in the request metadata:

```
authorization: bearer <token>
```

Requests with a missing or incorrect token receive `codes.Unauthenticated`.

If `GRPC_AUTH_TOKEN` is not set, authentication is disabled and all requests are accepted.

> **Limitations:**
> - Only a single shared token is supported. There is no per-caller token or RBAC — any holder of the token has full read/write access to all RPCs.
> - Token rotation requires restarting the sidecar. The token is read once at startup from `GRPC_AUTH_TOKEN`; there is no mechanism to reload it from a Kubernetes Secret without a pod restart.
> - Multi-token support and zero-downtime token rotation are planned future improvements.

### Kubernetes Secret setup

Create a Secret with the token value and reference it from the sidecar's environment:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: coredns-netbox-grpc-token
type: Opaque
stringData:
  token: "my-secret-token"
```

Reference it in the StatefulSet pod spec:

```yaml
env:
  - name: GRPC_AUTH_TOKEN
    valueFrom:
      secretKeyRef:
        name: coredns-netbox-grpc-token
        key: token
```

## Design decisions

**Why merge happens in the sidecar, not CoreDNS**

CoreDNS's `file` plugin owns the zone data as a flat file on disk. There is no per-record fallback mechanism between plugins: a zone is either served by `file` or by another plugin — not both simultaneously. Merging in the sidecar before writing the zone file is the only way to serve both Netbox records and dynamic records from the same zone without modifying CoreDNS itself.

**Why a flat JSON file for persistence**

Dynamic records must survive sidecar restarts. A flat JSON file on the same PVC used for zone files is the simplest approach that is consistent with the existing storage model. The `DynamicStore` interface abstracts the storage backend, so a future implementation (e.g., etcd, Redis) can be swapped in without changing the gRPC API or the merge logic.

**Why bearer token, not mTLS**

mTLS requires a certificate authority and cert rotation infrastructure (e.g., cert-manager). The target deployment environment does not mandate cert-manager, and in-cluster traffic is considered adequately isolated by Kubernetes network policies. A static bearer token stored in a Kubernetes Secret meets the security requirement with zero additional dependencies. mTLS remains a future option enabled by the `DynamicStore` abstraction.

**Why conflicts are rejected atomically with `codes.AlreadyExists`**

Allowing partial success in a `BatchUpsert` call creates an ambiguous state: the caller cannot know which records were written without a follow-up `ListRecords` call. Atomic rejection forces the caller to reason about the conflict before retrying, which is always the correct behavior when inserting records that must be authoritative. Use `force: true` when you explicitly want to overwrite.

**Why `force: true` exists**

There are legitimate emergency use cases — for example, temporarily overriding a Netbox record whose IP is wrong while the Netbox entry is being corrected. Making the intent visible in the API (`force: true`) is preferable to silently shadowing existing records, because it makes auditing and code review easier and prevents accidental overwrites.

**Why rapid gRPC trigger calls do not queue up**

`ForceNetboxPoll` and `ForceMergeWrite` signal the poll loop via buffered channels of size 1. A buffered channel of size 1 is a natural debounce: if a signal is already waiting in the buffer when a second call arrives, the non-blocking send is silently dropped. Rapid-fire calls therefore coalesce into a single poll or merge cycle rather than accumulating a backlog of redundant work. The poll loop is never blocked on an empty channel when idle, so no signal is lost in the common case — only duplicates during bursts are discarded. Callers that need confirmation of completion should poll `GetStatus` after calling `ForceNetboxPoll` or `ForceMergeWrite`.

**Why `ForceMergeWrite` is independently triggerable**

After a `UpsertRecord` call, the dynamic record is stored immediately, but it does not appear in DNS until the next merge+write cycle (which normally coincides with a Netbox poll). `ForceMergeWrite` triggers only the merge+write step — no Netbox round-trip — so dynamic records can propagate to DNS in milliseconds. This is especially useful for time-sensitive operations such as adding a pod IP during a rolling deployment.

**Why per-record TTL**

RFC 1035 §3.2.1 permits each resource record to carry its own TTL. Kubernetes use cases need this flexibility: ephemeral pod IPs benefit from short TTLs (e.g., 30 s) so stale entries expire quickly after pod termination, while stable service IPs can use longer TTLs (e.g., 300 s) to reduce resolver load. A `ttl` value of `0` in a `Record` message falls back to the global TTL configured in the sidecar (via the `TTL` environment variable or `ttl` Helm value).

## Known limitations

**Dynamic zones are not propagated to secondaries automatically**

CoreDNS secondaries pull zones via AXFR using a static zone list baked into the Corefile at deploy time (the `secondary { ... }` block). When a new zone is created via `CreateZone`, it materialises on the primary as a zone file on disk, but the secondary has no mechanism to discover it — it only tracks the zones it was configured with at startup.

This does not affect the common case of **adding dynamic records to an existing zone**: the zone is already listed in the secondary's Corefile, and the secondary picks up new records on the next AXFR triggered by the SOA serial bump that `ForceMergeWrite` produces.

The gap only applies to **new zones created purely via gRPC** (zones that have no corresponding Netbox entry and were never part of the initial Corefile). In that scenario, secondary CoreDNS instances will not serve the new zone until they are redeployed with an updated Corefile that includes it.

**Workarounds for multi-zone deployments:**

- Pre-declare all zones expected to be created dynamically in the secondary Corefile, even before they contain any records. An AXFR of an empty zone succeeds and costs nothing.
- Push records to both primary and secondary gRPC endpoints from the caller, rather than relying on zone transfer for dynamic-only zones.
- Accept the gap and treat dynamically created zones as primary-only until the next Helm rollout includes the new zone name.

**This is not a concern for single-zone deployments.** If all records live in one zone, the zone is already being transferred to secondaries; dynamic records appear on secondaries after the next AXFR triggered by the serial change.

---

## `DynamicZoneService` RPC reference

### `CreateZone`

Creates a new dynamic zone. The zone is immediately available for record insertion and will be included in the next merge+write cycle.

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Zone name (e.g. `dc2.example.org`) |

**Errors:**
- `codes.AlreadyExists` — zone already exists in the dynamic store
- `codes.InvalidArgument` — `name` is empty

---

### `DeleteZone`

Deletes a dynamic zone and all records within it.

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Zone name |

**Errors:**
- `codes.NotFound` — zone does not exist

---

### `ListZones`

Returns the names of all dynamic zones currently held in the store.

**Request:** no fields

**Response:**

| Field | Type | Description |
|-------|------|-------------|
| `names` | repeated string | Zone names |

---

### `UpsertRecord`

Inserts or updates a single DNS record in the specified zone.

**Request:**

| Field | Type | Description |
|-------|------|-------------|
| `zone` | string | Target zone name |
| `record` | Record | The DNS record to upsert |
| `force` | bool | If true, overwrite a conflicting Netbox record |

**Record fields:**

| Field | Type | Description |
|-------|------|-------------|
| `dns_name` | string | Fully-qualified DNS name |
| `address` | string | IPv4 or IPv6 address |
| `family` | int32 | Address family: `4` for IPv4, `6` for IPv6 |
| `ttl` | uint32 | TTL in seconds; `0` falls back to global default |

**Errors:**
- `codes.AlreadyExists` — a Netbox record with the same name exists and `force` is false
- `codes.InvalidArgument` — missing or malformed fields

---

### `DeleteRecord`

Removes a single dynamic record from a zone.

**Request:**

| Field | Type | Description |
|-------|------|-------------|
| `zone` | string | Zone name |
| `dns_name` | string | Fully-qualified DNS name |

**Errors:**
- `codes.NotFound` — record does not exist in the dynamic store

---

### `ListRecords`

Returns all dynamic records in a zone.

**Request:**

| Field | Type | Description |
|-------|------|-------------|
| `zone` | string | Zone name |

**Response:**

| Field | Type | Description |
|-------|------|-------------|
| `records` | repeated Record | Dynamic records in the zone |

**Errors:**
- `codes.NotFound` — zone does not exist

---

### `BatchUpsert`

Atomically upserts multiple records across one or more zones. If any record conflicts (and `force` is false), the entire batch is rejected.

**Request:**

| Field | Type | Description |
|-------|------|-------------|
| `zone_records` | repeated ZoneRecords | Per-zone record lists |
| `force` | bool | If true, overwrite conflicting Netbox records |

**ZoneRecords:**

| Field | Type | Description |
|-------|------|-------------|
| `zone` | string | Target zone name |
| `records` | repeated Record | Records to upsert |

**Errors:**
- `codes.AlreadyExists` — one or more records conflict with Netbox data and `force` is false (entire batch rejected)

---

### `BatchDelete`

Removes multiple dynamic records from a single zone.

**Request:**

| Field | Type | Description |
|-------|------|-------------|
| `zone` | string | Zone name |
| `dns_names` | repeated string | Fully-qualified DNS names to delete |

**Errors:**
- `codes.NotFound` — one or more names do not exist in the dynamic store

## `ControlService` RPC reference

### `ForceNetboxPoll`

Triggers an immediate Netbox poll outside the normal poll interval. The sidecar fetches all zones and records from Netbox, merges dynamic records, and writes zone files. Blocks until the poll completes.

**Request / Response:** no fields

---

### `ForceMergeWrite`

Triggers only the merge+write step — no Netbox round-trip. Dynamic records already in the store are merged with the last cached Netbox data and written to zone files. Use this to make freshly upserted records visible in DNS within milliseconds.

**Request / Response:** no fields

---

### `GetStatus`

Returns a snapshot of the sidecar's current state.

**Response:**

| Field | Type | Description |
|-------|------|-------------|
| `last_netbox_poll_unix` | int64 | Unix timestamp of the last successful Netbox poll |
| `last_merge_write_unix` | int64 | Unix timestamp of the last successful merge+write |
| `active_zones` | int32 | Number of zones currently loaded |
| `dynamic_record_count` | int32 | Total number of dynamic records across all zones |
| `zone_staleness_seconds` | double | Seconds since the last Netbox poll |

## Conflict handling

### The `force` flag

When `UpsertRecord` or `BatchUpsert` is called without `force: true`, the server checks whether any of the requested DNS names already exist in the current Netbox-sourced data. If a conflict is found, the call fails with `codes.AlreadyExists` and no records are written.

Setting `force: true` bypasses the conflict check and overwrites the Netbox record in the merged output. The Netbox record is not modified — it will reappear if the dynamic record is deleted.

### Atomic batch rejection

`BatchUpsert` validates all records before writing any of them. If a single conflict is detected (and `force` is false), the entire batch is rejected atomically. This prevents partial-write states where the caller cannot determine which records were committed without a follow-up `ListRecords` call.

### `codes.AlreadyExists` semantics

`codes.AlreadyExists` is returned specifically when a dynamic upsert collides with a Netbox-sourced record and `force` is false. It is distinct from `codes.Internal` (unexpected server error) and `codes.InvalidArgument` (malformed request). Callers should inspect the error code to decide whether to retry with `force: true` or to correct the Netbox record instead.

## `grpcurl` examples

```bash
# List zones
grpcurl -plaintext -H 'authorization: bearer <token>' \
  <host>:8083 coredns_netbox.v1.DynamicZoneService/ListZones

# Upsert a record
grpcurl -plaintext -H 'authorization: bearer <token>' \
  -d '{"zone":"dc1.example.org","record":{"dns_name":"node1.k8s.dc1.example.org","address":"10.0.0.5","family":4,"ttl":60}}' \
  <host>:8083 coredns_netbox.v1.DynamicZoneService/UpsertRecord

# Force a Netbox poll
grpcurl -plaintext -H 'authorization: bearer <token>' \
  <host>:8083 coredns_netbox.v1.ControlService/ForceNetboxPoll

# Get status
grpcurl -plaintext -H 'authorization: bearer <token>' \
  <host>:8083 coredns_netbox.v1.ControlService/GetStatus
```
