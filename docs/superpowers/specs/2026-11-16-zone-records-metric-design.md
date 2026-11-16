# Design: `netbox_sidecar_zone_records` — published record counts per zone and type

**Issue:** [#111](https://github.com/pallotron/coredns-netbox/issues/111)
**Date:** 2026-11-16
**Status:** approved

## Problem

The sidecar exposes `netbox_sidecar_netbox_records_fetched` (IP objects returned by
NetBox) and `netbox_sidecar_interface_records`, but nothing that counts what was
actually **published**. Enrichment adds device records, per-interface records, and
CNAME aliases, and reverse discovery adds PTRs, so the published set can be several
times larger than the fetched count. Answering "how many records does this instance
serve, including PTRs?" today requires an AXFR and counting by hand. The per-zone
counts already exist internally — `countZoneRecords(combined)` feeds the shrink
guard — they are just not exported.

## Design

### Metric

One new `GaugeVec` in `internal/metrics/metrics.go`:

- **Name:** `netbox_sidecar_zone_records`
- **Labels:** `zone` (published zone origin, e.g. `example.com`,
  `10.in-addr.arpa`), `type` (`A`, `AAAA`, `CNAME`, `PTR`)
- **Help:** "Data records in each published zone by type, measured after a
  successful zone write. Zone-infrastructure records (SOA, apex NS, and the
  primary NS glue address) are not counted."

Cardinality is zones × ≤4 types — a handful of series.

`sum(netbox_sidecar_zone_records)` answers the total; `sum by (zone)` and
`sum by (type)` give the breakdowns.

### Classification and counting

Counting the merged ZoneMap directly would overcount: `zonegen.Generate`
filters records at emission time — `dropUnsafeNames` (G8) and
`resolveCNAMECollisions` — and CNAME collisions occur in real NetBox data, so
a map-level count would disagree with the zone files (and with AXFR) exactly
when it matters. The count therefore happens inside `zonegen`, after the
filters, and flows out through `zonemanager`:

- `zonegen` gains an exported `RecordCounts struct { A, AAAA, CNAME, PTR int }`
  with a `Total() int` method, and an unexported
  `countRecords(records []netboxclient.IPRecord, zoneType ZoneType) RecordCounts`:
  reverse zones count everything as `PTR`; forward zones classify per record
  (`Type == RecordTypeCNAME` → `CNAME`, `Family == 6` → `AAAA`, else `A`).
- `Generate`'s signature changes from `(string, bool, int, error)` to
  `(string, bool, RecordCounts, error)`; the counts are computed on the
  filtered record slice, so they are returned on the unchanged
  (hash-match) path too.
- `zonemanager.UpdateStats` gains `RecordCounts map[string]zonegen.RecordCounts`,
  populated for every zone in the ZoneMap, unchanged zones included.

### Wiring and lifecycle

In `internal/merge/merge.go` `Write()`, after `mgr.Update(merged)` succeeds —
next to the existing `m.ZonesActive.Set(...)`:

```go
m.ZoneRecords.Reset()
for zone, c := range stats.RecordCounts {
    for rtype, n := range map[string]int{"A": c.A, "AAAA": c.AAAA, "CNAME": c.CNAME, "PTR": c.PTR} {
        if n > 0 {
            m.ZoneRecords.WithLabelValues(zone, rtype).Set(float64(n))
        }
    }
}
```

Properties this buys:

- **Single choke point.** `merge.Write` is the only publish path for every
  trigger (periodic poll, gRPC dynamic-record merge, webhook-signaled poll), so
  the gauge covers dynamic-store records and shadowing, not just the NetBox poll.
- **Describes disk truth.** `merged` is exactly the ZoneMap handed to
  `zonemanager.Update`; counting it after a successful write means the gauge
  always describes the zones being served. On a failed write `Write` returns
  before the set, so the gauge keeps describing the zones still on disk — the
  same lifecycle as `netbox_sidecar_zones_active`.
- **No stale series.** `Reset()` before re-setting drops zones and types that
  disappeared instead of flatlining at their last value.
- **Absence = 0.** Only observed (zone, type) pairs are emitted. A zone that is
  active but holds no data records contributes no series;
  `netbox_sidecar_zones_active` still counts it.

Counts cover emitted data records only: the `IPRecord`s that survive the
emission filters. The SOA, apex NS, and primary-NS glue address that zonegen
adds per zone are excluded and the help text says so. Note this is slightly
stricter than the shrink guard's baseline (`countZoneRecords` counts
pre-filter): the gauge describes disk truth, the guard describes intent.

### Help-text fix

Reword `netbox_sidecar_netbox_records_fetched`'s help to: "IP address objects
returned by Netbox in the last poll (input to record generation, not the number
of published records — see netbox_sidecar_zone_records)."

### Documentation

`docs/observability.md`:

- Add the `netbox_sidecar_zone_records` row to the metric table, including the
  `sum(netbox_sidecar_zone_records)` recipe for "total records served".
- Amend the `netbox_sidecar_netbox_records_fetched` row to match the new help
  text.

## Testing

TDD throughout; testify `assert`/`require` per house style.

### Unit

- `zonegen.Generate` counts: forward A, AAAA, CNAME; reverse-zone PTR
  (reverse records keep `Family: 4` but count as PTR); counts reflect the
  emission filters (an unsafe dns_name and a collision-dropped CNAME are not
  counted); counts returned on the unchanged (hash-match) path.
- `zonemanager.Update`: `UpdateStats.RecordCounts` carries every zone in the
  map, unchanged zones included.
- `merge.Write`: after a write with mixed forward/reverse zones, the gauge holds
  the expected (zone, type) values (read via `prometheus/client_golang`'s
  testutil); a second write with a zone removed drops that zone's series
  (Reset behavior); a failed `mgr.Update` leaves the previous gauge values
  untouched.
- Existing merge tests keep passing untouched.

### End-to-end (`tests/e2e`, build tag `e2e`)

- Add `netbox_sidecar_zone_records` to the `want` list in
  `TestMetricsContainExpectedSeries` (`tests/e2e/metrics_test.go`).
- New `TestZoneRecordsMatchAXFR`: for each active zone, parse the
  `netbox_sidecar_zone_records{zone,type}` series from `/metrics` and compare
  against per-type counts from the existing `axfrRecords(t, zone)` helper
  (`tests/e2e/dns_test.go`), excluding SOA, apex NS, and the address record
  whose owner is the SOA MNAME (the G3 primary-NS glue record
  `zonegen.writePrimaryNSAddress` emits, which is not an `IPRecord` and so is
  not counted by the gauge). Wrapped in
  `require.Eventually` so a background poll landing between the scrape and the
  transfer cannot flake it. This asserts the exact invariant the issue exists
  for: the metric agrees with what AXFR serves.

## Out of scope

- Grafana dashboard changes (live in the internal values repo).
- Per-zone shrink-guard alerting rules (enabled by this metric, defined
  elsewhere).
- Any change to the webhook direct-write path beyond what `merge.Write`
  already covers.
