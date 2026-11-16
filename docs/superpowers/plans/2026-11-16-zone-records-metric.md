# Zone Record Count Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Export `netbox_sidecar_zone_records{zone,type}` — per-zone, per-type published DNS record counts — so "how many records does this instance serve, including PTRs?" is answerable from metrics instead of an AXFR (issue #111).

**Architecture:** `zonegen.Generate` counts the data records it emits (post `dropUnsafeNames`/`resolveCNAMECollisions` filtering) into a new `RecordCounts` struct; `zonemanager.Update` collects them per zone into `UpdateStats.RecordCounts`; `merge.Write` — the single publish choke point for poll, gRPC, and webhook triggers — resets and sets the gauge after a successful write, so the metric always describes the zones on disk.

**Tech Stack:** Go, prometheus/client_golang (+ its `testutil` for assertions), testify, miekg/dns (e2e only).

**Spec:** `docs/superpowers/specs/2026-11-16-zone-records-metric-design.md`

## Global Constraints

- **VCS is jj (Jujutsu), NOT git.** Never run git mutation commands. Per task: run `jj st`; if `@` has changes, run `jj new`; then `jj desc -m "<message>"` BEFORE coding; verify with `jj st` after. Do not run `jj new` at task end.
- Tests use testify: `require` for preconditions/setup, `assert` for test conditions. Import both `github.com/stretchr/testify/assert` and `github.com/stretchr/testify/require`.
- Metric name: `netbox_sidecar_zone_records`; labels exactly `zone`, `type`; type values exactly `A`, `AAAA`, `CNAME`, `PTR`.
- The gauge counts data records only — never SOA, apex NS, or the G3 primary-NS glue address.
- Before each commit, run `golangci-lint run ./...` (gofmt is enforced via its formatters). If `golangci-lint` is missing from PATH, run it via `mise exec -- golangci-lint run ./...` as its own Bash call (do not chain with `&&` — mise needs to be the top-level command).
- Run unit tests with `go test ./internal/<pkg>/... -count=1` (add `-race` for final verification via `make test.unit`).

---

### Task 1: `zonegen.RecordCounts` — count emitted records in `Generate`

**Files:**
- Modify: `internal/zonegen/generator.go` (Generate signature at line ~164; add RecordCounts + countRecords)
- Modify: `internal/zonemanager/manager.go:91` (the one non-test caller that binds the count)
- Test: `internal/zonegen/generator_test.go` (append new tests; existing call sites discard the count with `_`, so they compile unchanged)

**Interfaces:**
- Consumes: existing `Generate(records []netboxclient.IPRecord) (string, bool, int, error)`, `ZoneType` / `ZoneTypeForward` / `ZoneTypeReverse`, `netboxclient.RecordTypeCNAME`, `IPRecord.Family`.
- Produces (Task 2 and 3 rely on these exact names):
  - `type RecordCounts struct { A, AAAA, CNAME, PTR int }`
  - `func (c RecordCounts) Total() int`
  - `func (g *Generator) Generate(records []netboxclient.IPRecord) (string, bool, RecordCounts, error)`

- [ ] **Step 1: Start the jj change**

```bash
jj st   # if @ has changes: jj new
jj desc -m "Count emitted data records per type in zonegen.Generate"
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/zonegen/generator_test.go` (package `zonegen`; it already imports `netboxclient`, `assert`, `require`):

```go
func TestGenerate_RecordCountsForward(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
		Type:       ZoneTypeForward,
	}, "")

	records := []netboxclient.IPRecord{
		{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		{DNSName: "a2.example.org", Address: "10.0.0.2", Family: 4},
		{DNSName: "v6.example.org", Address: "fd00::1", Family: 6},
		{DNSName: "alias.example.org", Type: netboxclient.RecordTypeCNAME, CNAMETarget: "a1.example.org"},
		// G8: unsafe dns_name is dropped at emission and must not be counted.
		{DNSName: "bad name.example.org", Address: "10.0.0.3", Family: 4},
		// CNAME colliding with an address record is dropped by
		// resolveCNAMECollisions and must not be counted.
		{DNSName: "a1.example.org", Type: netboxclient.RecordTypeCNAME, CNAMETarget: "a2.example.org"},
	}

	_, changed, counts, err := gen.Generate(records)
	require.NoError(t, err)
	require.True(t, changed)

	assert.Equal(t, RecordCounts{A: 2, AAAA: 1, CNAME: 1}, counts)
	assert.Equal(t, 4, counts.Total())
}

func TestGenerate_RecordCountsReverse(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "10.in-addr.arpa",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
		Type:       ZoneTypeReverse,
	}, "")

	// Reverse records keep Family: 4 — they must count as PTR, not A.
	records := []netboxclient.IPRecord{
		{DNSName: "a1.example.org", Address: "1.0.0", Family: 4},
		{DNSName: "a2.example.org", Address: "2.0.0", Family: 4},
	}

	_, changed, counts, err := gen.Generate(records)
	require.NoError(t, err)
	require.True(t, changed)

	assert.Equal(t, RecordCounts{PTR: 2}, counts)
}

func TestGenerate_RecordCountsReturnedOnUnchangedPath(t *testing.T) {
	gen := NewGenerator(ZoneConfig{
		Origin:     "example.org",
		PrimaryNS:  "ns1.example.org.",
		AdminEmail: "admin.example.org.",
		TTL:        300,
		Type:       ZoneTypeForward,
	}, "")

	records := []netboxclient.IPRecord{
		{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
	}

	_, changed, _, err := gen.Generate(records)
	require.NoError(t, err)
	require.True(t, changed)

	// Same records again: hash matches, changed=false, counts still returned.
	_, changed, counts, err := gen.Generate(records)
	require.NoError(t, err)
	require.False(t, changed)
	assert.Equal(t, RecordCounts{A: 1}, counts)
}
```

Note: if `NewGenerator`'s actual signature in `generator.go` differs from the above (check how existing tests in this file construct generators and copy that pattern), follow the existing tests.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/zonegen/... -run 'TestGenerate_RecordCounts' -count=1`
Expected: FAIL — compile error: `undefined: RecordCounts` (and assignment-count mismatch once defined).

- [ ] **Step 4: Implement**

In `internal/zonegen/generator.go`:

Add near the top of the file (after the imports or next to `Generate`):

```go
// RecordCounts tallies the data records emitted into one zone, by RR type,
// after the emission filters (dropUnsafeNames, resolveCNAMECollisions).
// Zone-infrastructure records — the SOA, the apex NS, and the G3 primary-NS
// glue address — are not data records and are not counted.
type RecordCounts struct {
	A, AAAA, CNAME, PTR int
}

// Total returns the number of data records across all types.
func (c RecordCounts) Total() int { return c.A + c.AAAA + c.CNAME + c.PTR }

// countRecords classifies the filtered record slice. Reverse zones emit
// every record as a PTR regardless of its Family; forward zones classify
// per record.
func countRecords(records []netboxclient.IPRecord, zoneType ZoneType) RecordCounts {
	var c RecordCounts
	if zoneType == ZoneTypeReverse {
		c.PTR = len(records)
		return c
	}
	for _, r := range records {
		switch {
		case r.Type == netboxclient.RecordTypeCNAME:
			c.CNAME++
		case r.Family == 6:
			c.AAAA++
		default:
			c.A++
		}
	}
	return c
}
```

Change `Generate` (line ~164):

```go
// Generate creates a zone file string from the given records.
// Returns the zone content, whether it changed from the last generation, the
// per-type counts of the records in the zone after emission filtering, and
// any error.
func (g *Generator) Generate(records []netboxclient.IPRecord) (string, bool, RecordCounts, error) {
	records = dropUnsafeNames(records, g.config.Origin)
	if g.config.Type == ZoneTypeForward {
		records = resolveCNAMECollisions(records, g.config.Origin)
	}
	counts := countRecords(records, g.config.Type)
	hash := g.fingerprint(records)
	if hash == g.lastHash {
		slog.Debug("zone unchanged (hash match)", "zone", g.config.Origin, "hash", hash[:8], "records", counts.Total())
		return "", false, counts, nil
	}
	...
```

Then in the same function: replace the remaining two `return` statements' third value — the early-return above returns `counts`, and the final `return withFingerprints(...), true, len(records), nil` becomes `return withFingerprints(b.String(), hash), true, counts, nil`. The two `slog.Info` calls that log `"records", len(records)` keep working as-is (don't change them; `len(records)` equals `counts.Total()` here).

In `internal/zonemanager/manager.go:91`, rename the bound variable and fix the log line at 111:

```go
		content, changed, counts, err := gen.Generate(records)
```

and

```go
		slog.Info("zone updated", "zone", zone, "records", counts.Total())
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/zonegen/... ./internal/zonemanager/... -count=1`
Expected: PASS (all existing tests plus the three new ones).

- [ ] **Step 6: Lint and commit**

Run: `golangci-lint run ./...` — expected: no issues (use `mise exec -- golangci-lint run ./...` as a standalone Bash call if not on PATH).
Run: `jj st` — expected: the two modified files plus the test file listed under the described change.

---

### Task 2: `zonemanager.UpdateStats.RecordCounts` — per-zone counts out of `Update`

**Files:**
- Modify: `internal/zonemanager/manager.go` (UpdateStats struct at line ~30, Update loop at ~64-112)
- Test: `internal/zonemanager/manager_test.go` (append)

**Interfaces:**
- Consumes (from Task 1): `zonegen.RecordCounts`, `Generate(...) (string, bool, zonegen.RecordCounts, error)`.
- Produces (Task 3 relies on this exact shape): `UpdateStats.RecordCounts map[string]zonegen.RecordCounts`, keyed by zone origin (e.g. `"example.org"`, `"10.in-addr.arpa"`), populated for EVERY zone in the ZoneMap passed to `Update`, including zones whose files were unchanged.

- [ ] **Step 1: Start the jj change**

```bash
jj st   # if @ has changes: jj new
jj desc -m "Return per-zone record counts from zonemanager.Update"
```

- [ ] **Step 2: Write the failing test**

Append to `internal/zonemanager/manager_test.go` (package `zonemanager`; copy the construction pattern of existing tests in that file — they build a `Manager` with `New(t.TempDir(), ...)` and call `Update` with a `zonediscovery.ZoneMap`):

```go
func TestUpdate_RecordCountsPopulated(t *testing.T) {
	mgr := New(t.TempDir(), "ns1.example.org.", "", "admin.example.org.", 300, zonegen.SOATimers{})

	zm := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
			{DNSName: "alias.example.org", Type: netboxclient.RecordTypeCNAME, CNAMETarget: "a1.example.org"},
		},
		"10.in-addr.arpa": {
			{DNSName: "a1.example.org", Address: "1.0.0", Family: 4},
		},
	}

	stats, err := mgr.Update(zm)
	require.NoError(t, err)

	assert.Equal(t, zonegen.RecordCounts{A: 1, CNAME: 1}, stats.RecordCounts["example.org"])
	assert.Equal(t, zonegen.RecordCounts{PTR: 1}, stats.RecordCounts["10.in-addr.arpa"])

	// Second update with identical records: zones are unchanged on disk,
	// but the counts must still be reported for every zone in the map.
	stats, err = mgr.Update(zm)
	require.NoError(t, err)
	assert.Equal(t, zonegen.RecordCounts{A: 1, CNAME: 1}, stats.RecordCounts["example.org"])
	assert.Equal(t, zonegen.RecordCounts{PTR: 1}, stats.RecordCounts["10.in-addr.arpa"])
	assert.Len(t, stats.RecordCounts, 2)
}
```

Add any missing imports (`zonegen`, `zonediscovery`, `netboxclient`, `assert`, `require`) following the file's existing import block.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/zonemanager/... -run TestUpdate_RecordCountsPopulated -count=1`
Expected: FAIL — compile error: `stats.RecordCounts undefined`.

- [ ] **Step 4: Implement**

In `internal/zonemanager/manager.go`:

```go
// UpdateStats holds counters for zone file operations performed during an Update call.
type UpdateStats struct {
	Created     int
	Updated     int
	Deleted     int
	WriteErrors int
	// RecordCounts holds the per-type data-record counts each generator
	// reported, keyed by zone origin — every zone in the ZoneMap, unchanged
	// zones included. On an error return the map covers only the zones
	// processed before the failure.
	RecordCounts map[string]zonegen.RecordCounts
}
```

In `Update`, after `var stats UpdateStats` (line ~54):

```go
	stats.RecordCounts = make(map[string]zonegen.RecordCounts, len(zoneMap))
```

In the zone loop, right after the `Generate` call's error check (after line ~94):

```go
		stats.RecordCounts[zone] = counts
```

(Place it BEFORE the `if !changed { continue }` so unchanged zones are counted.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/zonemanager/... -count=1`
Expected: PASS.

- [ ] **Step 6: Lint and commit**

Run: `golangci-lint run ./...` — expected: no issues.
Run: `jj st` — expected: `internal/zonemanager/manager.go` and `manager_test.go` under the described change.

---

### Task 3: `netbox_sidecar_zone_records` gauge — declare, wire into `merge.Write`, fix fetched help text

**Files:**
- Modify: `internal/metrics/metrics.go` (struct at ~line 29, constructor at ~line 159-163, registration at ~line 202, help text at ~line 72)
- Modify: `internal/merge/merge.go` (after `mgr.Update` success, ~line 97)
- Test: `internal/merge/merge_test.go` (append)

**Interfaces:**
- Consumes (from Task 2): `stats.RecordCounts map[string]zonegen.RecordCounts` returned by `mgr.Update(merged)`.
- Produces: `metrics.Sidecar.ZoneRecords *prometheus.GaugeVec` (labels `zone`, `type`), reset-then-set on every successful `merge.Write`.

- [ ] **Step 1: Start the jj change**

```bash
jj st   # if @ has changes: jj new
jj desc -m "Export published record counts as netbox_sidecar_zone_records"
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/merge/merge_test.go` (package `merge_test`; uses the existing `newComponents(t)` helper). Add `"os"` (already imported) and `"github.com/prometheus/client_golang/prometheus/testutil"` to the imports.

```go
func TestWrite_ZoneRecordsGauge(t *testing.T) {
	_, store, mgr, m := newComponents(t)

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	netboxZones := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
			{DNSName: "a2.example.org", Address: "10.0.0.2", Family: 4},
			{DNSName: "v6.example.org", Address: "fd00::1", Family: 6},
			{DNSName: "alias.example.org", Type: netboxclient.RecordTypeCNAME, CNAMETarget: "a1.example.org"},
		},
		"10.in-addr.arpa": {
			{DNSName: "a1.example.org", Address: "1.0.0", Family: 4},
			{DNSName: "a2.example.org", Address: "2.0.0", Family: 4},
		},
	}

	_, err := merge.Write(netboxZones, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	assert.Equal(t, 2.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "AAAA")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "CNAME")))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("10.in-addr.arpa", "PTR")))
	// Only observed (zone, type) pairs are emitted: 4 series total.
	// (The WithLabelValues reads above create the series they touch, so
	// count BEFORE asserting absence in any new assertions you add.)
}

func TestWrite_ZoneRecordsGaugeResetsRemovedZones(t *testing.T) {
	_, store, mgr, m := newComponents(t)

	reverseDisc := zonediscovery.NewReverseZoneDiscoverer([]string{"10.in-addr.arpa"}, nil)
	withReverse := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"10.in-addr.arpa": {
			{DNSName: "a1.example.org", Address: "1.0.0", Family: 4},
		},
	}
	_, err := merge.Write(withReverse, store, mgr, m, reverseDisc)
	require.NoError(t, err)
	require.Equal(t, 2, testutil.CollectAndCount(m.ZoneRecords), "expected A + PTR series after first write")

	// Second write without the reverse zone: its PTR series must disappear,
	// not flatline at the old value.
	forwardOnly := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	_, err = merge.Write(forwardOnly, store, mgr, m, reverseDisc)
	require.NoError(t, err)

	assert.Equal(t, 1, testutil.CollectAndCount(m.ZoneRecords), "stale zone series must be reset")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))
}

func TestWrite_ZoneRecordsGaugeKeptOnFailedUpdate(t *testing.T) {
	dir, store, mgr, m := newComponents(t)

	netboxZones := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
	}
	_, err := merge.Write(netboxZones, store, mgr, m, nil)
	require.NoError(t, err)
	require.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))

	// Make the zone dir unwritable so the next write (a new zone file) fails.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	grown := zonediscovery.ZoneMap{
		"example.org": {
			{DNSName: "a1.example.org", Address: "10.0.0.1", Family: 4},
		},
		"other.example.net": {
			{DNSName: "b1.other.example.net", Address: "10.0.0.9", Family: 4},
		},
	}
	_, err = merge.Write(grown, store, mgr, m, nil)
	require.Error(t, err, "write into a read-only zone dir must fail")

	// The gauge still describes the zones actually on disk.
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("example.org", "A")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ZoneRecords.WithLabelValues("other.example.net", "A")))
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/merge/... -count=1`
Expected: FAIL — compile error: `m.ZoneRecords undefined`.

- [ ] **Step 4: Implement**

In `internal/metrics/metrics.go`:

1. Struct field, after `PTRUncoveredIPs *prometheus.GaugeVec` (line ~29):

```go
	ZoneRecords *prometheus.GaugeVec
```

2. Constructor entry, after the `PTRUncoveredIPs` literal (line ~159-163):

```go
		ZoneRecords: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "netbox_sidecar_zone_records",
			Help: "Data records in each published zone by type (A, AAAA, CNAME, PTR), measured after a successful zone write. Zone-infrastructure records (SOA, apex NS, and the primary NS glue address) are not counted.",
		}, []string{"zone", "type"}),
```

3. Registration: add `m.ZoneRecords,` after `m.PTRUncoveredIPs,` in the `reg.MustRegister(...)` call (line ~202).

4. Help-text fix (line ~72):

```go
		NetboxRecordsFetched: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "netbox_sidecar_netbox_records_fetched",
			Help: "IP address objects returned by Netbox in the last poll (input to record generation, not the number of published records - see netbox_sidecar_zone_records).",
		}),
```

Do NOT pre-create ZoneRecords series in the pre-create loop (zones aren't known at construction time; the gauge legitimately starts empty).

In `internal/merge/merge.go`, after the `if err != nil { ... return ... }` block that follows `mgr.Update(merged)`, next to `m.ZonesActive.Set(...)` (line ~97):

```go
	m.ZonesActive.Set(float64(len(mgr.Zones())))
	m.ZoneRecords.Reset()
	for zone, c := range stats.RecordCounts {
		for rtype, n := range map[string]int{"A": c.A, "AAAA": c.AAAA, "CNAME": c.CNAME, "PTR": c.PTR} {
			if n > 0 {
				m.ZoneRecords.WithLabelValues(zone, rtype).Set(float64(n))
			}
		}
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/merge/... -count=1`
Expected: PASS (the three new tests and all existing `TestWrite_*` tests).

- [ ] **Step 6: Run the full unit suite**

Run: `make test.unit`
Expected: PASS across `./internal/...` with `-race`.

- [ ] **Step 7: Lint and commit**

Run: `golangci-lint run ./...` — expected: no issues.
Run: `jj st` — expected: `internal/metrics/metrics.go`, `internal/merge/merge.go`, `internal/merge/merge_test.go` under the described change.

---

### Task 4: Document the metric in `docs/observability.md`

**Files:**
- Modify: `docs/observability.md` (metric table rows at lines ~24 and ~32)

**Interfaces:**
- Consumes: final metric name/labels from Task 3. No code.

- [ ] **Step 1: Start the jj change**

```bash
jj st   # if @ has changes: jj new
jj desc -m "Document netbox_sidecar_zone_records in observability.md"
```

- [ ] **Step 2: Edit the metric table**

In `docs/observability.md`:

1. Replace the `netbox_sidecar_netbox_records_fetched` row (line ~24):

```markdown
| `netbox_sidecar_netbox_records_fetched` | Gauge | — | IP address objects returned by Netbox in the last poll — the input to record generation, not the number of published records (that is `netbox_sidecar_zone_records`) |
```

2. Insert a new row directly after the `netbox_sidecar_zones_active` row (line ~32):

```markdown
| `netbox_sidecar_zone_records` | Gauge | `zone`, `type=A\|AAAA\|CNAME\|PTR` | Data records in each published zone by type, measured after a successful zone write. `sum(netbox_sidecar_zone_records)` is the total record count served, PTRs included; `sum by (type)` and `sum by (zone)` give the breakdowns. Zone-infrastructure records (SOA, apex NS, primary-NS glue address) are not counted. Only observed `(zone, type)` pairs are emitted — absence means zero |
```

- [ ] **Step 3: Verify table renders**

Run: `grep -n "zone_records" docs/observability.md`
Expected: both rows present; pipe-escape `\|` used inside the type column so the Markdown table doesn't break.

- [ ] **Step 4: Commit**

Run: `jj st` — expected: only `docs/observability.md` under the described change.

---

### Task 5: e2e coverage — series presence + AXFR invariant

**Files:**
- Modify: `tests/e2e/metrics_test.go` (add to `want` list at line ~50-68; append new test)

**Interfaces:**
- Consumes: `fetchMetrics(t)` and `sidecarMetricsURL()` (`tests/e2e/metrics_test.go`), `axfrRecords(t, zone)` and `waitForPrimary(t)` (`tests/e2e/dns_test.go`, same package), `github.com/miekg/dns`.
- Produces: `TestZoneRecordsMatchAXFR` — the invariant that the gauge agrees with what AXFR serves.

- [ ] **Step 1: Start the jj change**

```bash
jj st   # if @ has changes: jj new
jj desc -m "Assert netbox_sidecar_zone_records matches AXFR in e2e"
```

- [ ] **Step 2: Add the metric to the presence list**

In `tests/e2e/metrics_test.go`, `TestMetricsContainExpectedSeries`, append to `want` after `"netbox_sidecar_zones_active"`:

```go
		"netbox_sidecar_zone_records",
```

Note: this makes the presence test require at least one published data record in the e2e environment (only observed series are emitted). The e2e fixtures always seed records, so this holds; if it ever flakes on a cold start, the AXFR test's `require.Eventually` pattern below is the fix, not removing the entry.

- [ ] **Step 3: Append the AXFR invariant test**

Append to `tests/e2e/metrics_test.go`. Add imports: `"maps"`, `"regexp"`, `"strconv"`, and `"github.com/miekg/dns"` (keep the existing ones).

```go
// zoneRecordsLineRe matches one netbox_sidecar_zone_records sample in the
// Prometheus text exposition format. Labels are emitted alphabetically, so
// type precedes zone.
var zoneRecordsLineRe = regexp.MustCompile(`^netbox_sidecar_zone_records\{type="([^"]+)",zone="([^"]+)"\} ([0-9.e+]+)$`)

// scrapeZoneRecords parses the gauge into zone -> type -> count.
func scrapeZoneRecords(t *testing.T) map[string]map[string]int {
	t.Helper()
	out := make(map[string]map[string]int)
	for _, line := range strings.Split(fetchMetrics(t), "\n") {
		matches := zoneRecordsLineRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		rtype, zone := matches[1], matches[2]
		v, err := strconv.ParseFloat(matches[3], 64)
		require.NoError(t, err, "parse value in %q", line)
		if out[zone] == nil {
			out[zone] = make(map[string]int)
		}
		out[zone][rtype] = int(v)
	}
	return out
}

// axfrTypeCounts counts the data records a zone transfer serves, excluding
// zone infrastructure: the SOA (AXFR envelopes carry it twice), the apex NS,
// and the G3 primary-NS glue address — an A record owned by the SOA MNAME
// that zonegen emits from PRIMARY_NS_IP and that the gauge does not count.
// (Fixture assumption: NetBox does not itself hold a record named like the
// primary NS; if it ever does, zonegen suppresses the glue and this exclusion
// would undercount A by one.)
func axfrTypeCounts(t *testing.T, zone string) map[string]int {
	t.Helper()
	rrs := axfrRecords(t, zone)

	var mname string
	for _, rr := range rrs {
		if soa, ok := rr.(*dns.SOA); ok {
			mname = soa.Ns
			break
		}
	}

	counts := make(map[string]int)
	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.SOA, *dns.NS:
			continue
		case *dns.A:
			if v.Hdr.Name == mname {
				continue // primary-NS glue
			}
			counts["A"]++
		case *dns.AAAA:
			if v.Hdr.Name == mname {
				continue
			}
			counts["AAAA"]++
		case *dns.CNAME:
			counts["CNAME"]++
		case *dns.PTR:
			counts["PTR"]++
		}
	}
	return counts
}

// TestZoneRecordsMatchAXFR asserts the invariant issue #111 exists for: the
// published-record gauge agrees, per zone and per type, with what an AXFR of
// that zone actually serves. Eventually-wrapped because a background poll can
// land between the metrics scrape and the zone transfer.
func TestZoneRecordsMatchAXFR(t *testing.T) {
	waitForPrimary(t)

	var lastDiff string
	ok := assert.Eventually(t, func() bool {
		metricCounts := scrapeZoneRecords(t)
		if len(metricCounts) == 0 {
			lastDiff = "no netbox_sidecar_zone_records series scraped yet"
			return false
		}
		for zone, want := range metricCounts {
			got := axfrTypeCounts(t, zone)
			if !maps.Equal(want, got) {
				lastDiff = fmt.Sprintf("zone %s: metrics=%v axfr=%v", zone, want, got)
				return false
			}
		}
		return true
	}, 60*time.Second, 2*time.Second, "gauge and AXFR disagree")
	if !ok {
		t.Logf("last disagreement: %s", lastDiff)
	}
}
```

Also add `"fmt"` and keep `"strings"`/`"time"` (already imported) — follow the compiler.

- [ ] **Step 4: Compile-check the e2e package**

Run: `go vet -tags=e2e ./tests/e2e/...`
Expected: no output (exit 0).

- [ ] **Step 5: Run the e2e suite if the local environment is available**

Run: `make test.e2e.local` — NOTE: this needs Docker; run it as a standalone Bash call (not chained) so Docker can reach its socket outside the sandbox. If the local stack cannot run, state that explicitly and leave e2e verification to CI — do not claim the e2e tests pass.
Expected (when run): `TestMetricsContainExpectedSeries` and `TestZoneRecordsMatchAXFR` PASS.

- [ ] **Step 6: Lint and commit**

Run: `golangci-lint run ./...` — expected: no issues (the e2e build tag keeps these files out of the default build; if the linter skips them, that is fine — `go vet -tags=e2e` in Step 4 is the compile gate).
Run: `jj st` — expected: only `tests/e2e/metrics_test.go` under the described change.

---

## Final verification (after all tasks)

- [ ] `make test.unit` — PASS with `-race`.
- [ ] `go vet -tags=e2e ./tests/e2e/...` — clean.
- [ ] `golangci-lint run ./...` — clean.
- [ ] `jj log` — one described change per task, spec and plan committed, nothing undescribed.
- [ ] Spot-check: `go doc ./internal/zonegen RecordCounts` prints the type with its doc comment.
