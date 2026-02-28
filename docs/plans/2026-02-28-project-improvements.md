# Project-Wide Improvements Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Address 10 identified gaps across test coverage, correctness, docs, and CI hygiene.

**Architecture:** All changes are independent — each task touches a distinct set of files. Tasks are ordered so tests come first (TDD where applicable), then bug fixes, then docs/config polish.

**Tech Stack:** Go 1.25, testify (assert/require), Helm, GitHub Actions

---

### Task 1: Add unit tests for `internal/ipcategorizer`

**Files:**
- Create: `internal/ipcategorizer/categorizer_test.go`

**Step 1: Write tests for `NewCategorizer` (valid + invalid regex)**

```go
package ipcategorizer

import (
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCategorizer(t *testing.T) *Categorizer {
	t.Helper()
	c, err := NewCategorizer(
		"(?i)bmc|ipmi|ilo|idrac",
		"^lo$|^lo0|^Loopback",
		"(?i)storage|vtep|vsan",
		"(?i)mgmt|oob",
		"(?i)mgmt|Management|fxp0|eth[01]|mgt|NET",
		"example.com",
	)
	require.NoError(t, err)
	return c
}

func TestNewCategorizer_InvalidRegex(t *testing.T) {
	_, err := NewCategorizer("[invalid", "^lo$", "ok", "ok", "ok", "example.com")
	assert.Error(t, err, "expected error for invalid BMC regex")
}
```

**Step 2: Write tests for `Categorize` — all priority paths**

```go
func TestCategorize(t *testing.T) {
	c := newTestCategorizer(t)

	tests := []struct {
		name     string
		record   netboxclient.IPRecord
		expected InterfaceCategory
	}{
		{"BMC interface", netboxclient.IPRecord{InterfaceName: "bmc0"}, CategoryBMC},
		{"IPMI interface", netboxclient.IPRecord{InterfaceName: "ipmi"}, CategoryBMC},
		{"iDRAC interface", netboxclient.IPRecord{InterfaceName: "iDRAC"}, CategoryBMC},
		{"Loopback lo", netboxclient.IPRecord{InterfaceName: "lo"}, CategoryLoopback},
		{"Loopback lo0", netboxclient.IPRecord{InterfaceName: "lo0"}, CategoryLoopback},
		{"Loopback Loopback0", netboxclient.IPRecord{InterfaceName: "Loopback0"}, CategoryLoopback},
		{"Dataplane storage", netboxclient.IPRecord{InterfaceName: "storage0"}, CategoryDataplane},
		{"Dataplane vtep", netboxclient.IPRecord{InterfaceName: "vtep1"}, CategoryDataplane},
		{"MgmtVRF", netboxclient.IPRecord{InterfaceName: "eth5", VRF: "mgmt"}, CategoryMgmtVRF},
		{"MgmtVRF oob", netboxclient.IPRecord{InterfaceName: "eth5", VRF: "oob"}, CategoryMgmtVRF},
		{"MgmtInterface fxp0", netboxclient.IPRecord{InterfaceName: "fxp0"}, CategoryMgmtInterface},
		{"MgmtInterface eth0", netboxclient.IPRecord{InterfaceName: "eth0"}, CategoryMgmtInterface},
		{"Unknown", netboxclient.IPRecord{InterfaceName: "ge-0/0/0"}, CategoryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, c.Categorize(tt.record))
		})
	}
}
```

**Step 3: Write tests for `Categorize` priority — BMC beats loopback, etc.**

```go
func TestCategorize_Priority(t *testing.T) {
	// BMC pattern also matches "bmc" which starts with lowercase b — test that
	// an interface named "bmc" wins over a VRF match
	c := newTestCategorizer(t)
	r := netboxclient.IPRecord{InterfaceName: "bmc0", VRF: "mgmt"}
	assert.Equal(t, CategoryBMC, c.Categorize(r), "BMC should win over MgmtVRF")
}
```

**Step 4: Write tests for `InterfaceCategory.String()`**

```go
func TestInterfaceCategory_String(t *testing.T) {
	assert.Equal(t, "bmc", CategoryBMC.String())
	assert.Equal(t, "loopback", CategoryLoopback.String())
	assert.Equal(t, "dataplane", CategoryDataplane.String())
	assert.Equal(t, "mgmt-vrf", CategoryMgmtVRF.String())
	assert.Equal(t, "mgmt-interface", CategoryMgmtInterface.String())
	assert.Equal(t, "unknown", CategoryUnknown.String())
}
```

**Step 5: Write tests for `SelectDeviceIPs`**

```go
func TestSelectDeviceIPs(t *testing.T) {
	c := newTestCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "dc1-site1-sw01", DNSName: "dc1-site1-sw01.example.com", Address: "10.0.0.1", InterfaceName: "mgmt0", VRF: "mgmt", Family: 4},
		{DeviceName: "dc1-site1-sw01", DNSName: "dc1-site1-sw01-bmc.example.com", Address: "10.0.0.2", InterfaceName: "bmc0", Family: 4},
		{DeviceName: "dc1-site1-sw01", DNSName: "dc1-site1-sw01.example.com", Address: "10.0.0.3", InterfaceName: "ge-0/0/0", Family: 4},
	}

	result := c.SelectDeviceIPs(records)

	require.Contains(t, result, "dc1-site1-sw01")
	dev := result["dc1-site1-sw01"]
	require.NotNil(t, dev.PrimaryIP, "should have primary management IP")
	assert.Equal(t, "10.0.0.1", dev.PrimaryIP.Address)
	require.NotNil(t, dev.BMCIP, "should have BMC IP")
	assert.Equal(t, "10.0.0.2", dev.BMCIP.Address)
}

func TestSelectDeviceIPs_SkipsEmptyDeviceName(t *testing.T) {
	c := newTestCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "", DNSName: "orphan.example.com", Address: "10.0.0.1", InterfaceName: "eth0", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	assert.Empty(t, result, "records with empty device name should be skipped")
}

func TestSelectDeviceIPs_PrefersVRFOverInterfaceName(t *testing.T) {
	c := newTestCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "sw01", DNSName: "sw01.example.com", Address: "10.0.0.1", InterfaceName: "eth0", VRF: "mgmt", Family: 4},
		{DeviceName: "sw01", DNSName: "sw01.example.com", Address: "10.0.0.2", InterfaceName: "Management1", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	require.Contains(t, result, "sw01")
	assert.Equal(t, "10.0.0.1", result["sw01"].PrimaryIP.Address, "VRF-based IP should be preferred")
}

func TestSelectDeviceIPs_NoMgmtOrBMC(t *testing.T) {
	c := newTestCategorizer(t)

	records := []netboxclient.IPRecord{
		{DeviceName: "sw01", DNSName: "sw01.example.com", Address: "10.0.0.1", InterfaceName: "ge-0/0/0", Family: 4},
	}

	result := c.SelectDeviceIPs(records)
	assert.Empty(t, result, "device with only unknown interfaces should not appear")
}
```

**Step 6: Write tests for `extractZone`**

```go
func TestExtractZone(t *testing.T) {
	c := newTestCategorizer(t)

	tests := []struct {
		name       string
		deviceName string
		expected   string
	}{
		{"standard naming", "dc1-site13a-r101-prod-hv-01", "dc1-site.example.com"},
		{"short site", "dc2-m21-r101-prod-hv-01", "dc2-m.example.com"},
		{"no hyphen", "standalone", "example.com"},
		{"single component site", "dc1-123-r01", "dc1-123.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, c.extractZone(tt.deviceName))
		})
	}
}
```

**Step 7: Run tests**

Run: `make test.unit`
Expected: All new tests PASS

**Step 8: Commit**

```
jj new && jj describe -m "test: add unit tests for ipcategorizer package"
```

---

### Task 2: Add unit tests for `internal/config`

**Files:**
- Create: `internal/config/config_test.go`

**Step 1: Write tests covering defaults, valid overrides, and invalid values**

```go
package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearEnv unsets all config env vars for test isolation.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"NETBOX_TOKEN", "NETBOX_URL", "DISCOVERY_MODE", "ZONE_DEPTH",
		"ZONE_DIR", "POLL_INTERVAL", "TTL", "NETBOX_PAGE_SIZE",
		"NETBOX_MAX_CONCURRENCY", "NETBOX_RETRY_COUNT",
		"NETBOX_RETRY_BASE_DELAY", "NETBOX_RETRY_MAX_DELAY",
		"HEALTH_ADDR", "PRIMARY_NS", "ADMIN_EMAIL",
		"BMC_INTERFACE_PATTERN", "LOOPBACK_PATTERN", "DATAPLANE_PATTERN",
		"MGMT_VRF_PATTERN", "MGMT_INTERFACE_PATTERN",
		"ENABLE_REVERSE_ZONES", "REVERSE_ZONES_IPV4", "REVERSE_ZONES_IPV6",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func TestLoad_RequiresToken(t *testing.T) {
	clearEnv(t)
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NETBOX_TOKEN")
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("NETBOX_TOKEN", "test-token")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "http://netbox.netbox.svc.cluster.local", cfg.NetboxURL)
	assert.Equal(t, "zone-depth", cfg.DiscoveryMode)
	assert.Equal(t, 2, cfg.ZoneDepth)
	assert.Equal(t, "/zones", cfg.ZoneDir)
	assert.Equal(t, 60*time.Second, cfg.PollInterval)
	assert.Equal(t, uint32(300), cfg.TTL)
	assert.Equal(t, 1000, cfg.PageSize)
	assert.Equal(t, 10, cfg.MaxConcurrency)
	assert.Equal(t, 3, cfg.NetboxRetryCount)
	assert.Equal(t, 1*time.Second, cfg.NetboxRetryBaseDelay)
	assert.Equal(t, 30*time.Second, cfg.NetboxRetryMaxDelay)
	assert.True(t, cfg.EnableReverseZones)
	assert.Equal(t, []string{"10.in-addr.arpa", "172.16.in-addr.arpa"}, cfg.ReverseZonesIPv4)
	assert.Nil(t, cfg.ReverseZonesIPv6)
}

func TestLoad_OverrideNumericFields(t *testing.T) {
	clearEnv(t)
	t.Setenv("NETBOX_TOKEN", "test-token")
	t.Setenv("ZONE_DEPTH", "3")
	t.Setenv("TTL", "600")
	t.Setenv("POLL_INTERVAL", "30s")
	t.Setenv("NETBOX_PAGE_SIZE", "500")
	t.Setenv("NETBOX_MAX_CONCURRENCY", "5")
	t.Setenv("NETBOX_RETRY_COUNT", "5")
	t.Setenv("NETBOX_RETRY_BASE_DELAY", "2s")
	t.Setenv("NETBOX_RETRY_MAX_DELAY", "1m")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 3, cfg.ZoneDepth)
	assert.Equal(t, uint32(600), cfg.TTL)
	assert.Equal(t, 30*time.Second, cfg.PollInterval)
	assert.Equal(t, 500, cfg.PageSize)
	assert.Equal(t, 5, cfg.MaxConcurrency)
	assert.Equal(t, 5, cfg.NetboxRetryCount)
	assert.Equal(t, 2*time.Second, cfg.NetboxRetryBaseDelay)
	assert.Equal(t, 1*time.Minute, cfg.NetboxRetryMaxDelay)
}

func TestLoad_InvalidValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"bad ZONE_DEPTH", "ZONE_DEPTH", "abc"},
		{"bad TTL", "TTL", "not-a-number"},
		{"bad POLL_INTERVAL", "POLL_INTERVAL", "xyz"},
		{"bad NETBOX_PAGE_SIZE", "NETBOX_PAGE_SIZE", "big"},
		{"bad NETBOX_MAX_CONCURRENCY", "NETBOX_MAX_CONCURRENCY", "nope"},
		{"bad NETBOX_RETRY_COUNT", "NETBOX_RETRY_COUNT", "no"},
		{"bad NETBOX_RETRY_BASE_DELAY", "NETBOX_RETRY_BASE_DELAY", "bad"},
		{"bad NETBOX_RETRY_MAX_DELAY", "NETBOX_RETRY_MAX_DELAY", "bad"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NETBOX_TOKEN", "test-token")
			t.Setenv(tt.key, tt.val)

			_, err := Load()
			assert.Error(t, err, "expected error for %s=%s", tt.key, tt.val)
		})
	}
}

func TestLoad_ReverseZonesParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("NETBOX_TOKEN", "test-token")
	t.Setenv("REVERSE_ZONES_IPV4", "10.in-addr.arpa, 172.16.in-addr.arpa")
	t.Setenv("REVERSE_ZONES_IPV6", "8.b.d.0.1.0.0.2.ip6.arpa")
	t.Setenv("ENABLE_REVERSE_ZONES", "false")

	cfg, err := Load()
	require.NoError(t, err)

	assert.False(t, cfg.EnableReverseZones)
	assert.Equal(t, []string{"10.in-addr.arpa", "172.16.in-addr.arpa"}, cfg.ReverseZonesIPv4)
	assert.Equal(t, []string{"8.b.d.0.1.0.0.2.ip6.arpa"}, cfg.ReverseZonesIPv6)
}
```

**Step 2: Run tests**

Run: `make test.unit`
Expected: All PASS

**Step 3: Commit**

```
jj new && jj describe -m "test: add unit tests for config package"
```

---

### Task 3: Paginate `NetboxDNSDiscoverer.FetchZones`

The current code uses `limit=1000` and never follows `next` pages. The Netbox API returns a `next` URL when more results exist.

**Files:**
- Modify: `internal/zonediscovery/netbox_dns.go:38-78`
- Modify: `internal/zonediscovery/discoverer_test.go` (add pagination test)

**Step 1: Write a failing test for pagination**

Add to `internal/zonediscovery/discoverer_test.go`:

```go
func TestNetboxDNS_FetchZones_Pagination(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First page: return next URL pointing to page 2
			json.NewEncoder(w).Encode(map[string]interface{}{
				"count": 3,
				"next":  "http://" + r.Host + "/api/plugins/netbox-dns/zones/?status=active&limit=2&offset=2",
				"results": []map[string]interface{}{
					{"name": "zone1.example.com", "status": map[string]string{"value": "active"}},
					{"name": "zone2.example.com", "status": map[string]string{"value": "active"}},
				},
			})
		} else {
			// Second page: no next
			json.NewEncoder(w).Encode(map[string]interface{}{
				"count":   3,
				"next":    nil,
				"results": []map[string]interface{}{
					{"name": "zone3.example.com", "status": map[string]string{"value": "active"}},
				},
			})
		}
	}))
	defer srv.Close()

	d := NewNetboxDNSDiscoverer(srv.URL, "test-token")
	zones, err := d.FetchZones(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"zone1.example.com", "zone2.example.com", "zone3.example.com"}, zones)
	assert.Equal(t, 2, callCount, "should have fetched 2 pages")
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/zonediscovery/ -run TestNetboxDNS_FetchZones_Pagination -v`
Expected: FAIL (only 2 zones returned, not 3)

**Step 3: Implement pagination in `FetchZones`**

Update the `netboxDNSZoneList` struct to include `Next`:

```go
type netboxDNSZoneList struct {
	Count   int              `json:"count"`
	Next    *string          `json:"next"`
	Results []netboxDNSZone  `json:"results"`
}
```

Rewrite `FetchZones` to follow `next`:

```go
func (d *NetboxDNSDiscoverer) FetchZones(ctx context.Context) ([]string, error) {
	url := d.baseURL + "/api/plugins/netbox-dns/zones/?status=active&limit=1000"

	var zones []string
	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		req.Header.Set("Authorization", netboxclient.AuthHeader(d.token))
		req.Header.Set("Accept", "application/json")

		resp, err := d.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch zones: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("netbox-dns zones API returned %d: %s", resp.StatusCode, string(body))
		}

		var result netboxDNSZoneList
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode zones response: %w", err)
		}
		resp.Body.Close()

		for _, z := range result.Results {
			if z.Name != "" {
				zones = append(zones, z.Name)
			}
		}

		if result.Next != nil {
			url = *result.Next
		} else {
			url = ""
		}
	}
	return zones, nil
}
```

**Step 4: Run tests**

Run: `make test.unit`
Expected: All PASS including the new pagination test

**Step 5: Commit**

```
jj new && jj describe -m "fix: paginate NetboxDNSDiscoverer.FetchZones to handle >1000 zones"
```

---

### Task 4: Update CoreDNS Dockerfile Go version

**Files:**
- Modify: `coredns/Dockerfile:1`

**Step 1: Update the builder image**

Change line 1 from:
```dockerfile
FROM golang:1.23-alpine AS builder
```
to:
```dockerfile
FROM golang:1.25-alpine AS builder
```

**Step 2: Verify it builds**

Run: `docker build -t coredns-netbox:test -f coredns/Dockerfile coredns/`
Expected: Build succeeds (this step can be skipped if Docker is not available locally — CI will verify)

**Step 3: Commit**

```
jj new && jj describe -m "chore: update CoreDNS Dockerfile to Go 1.25 to match go.mod"
```

---

### Task 5: Add missing metrics to E2E test

**Files:**
- Modify: `tests/e2e/metrics_test.go:49-59`

**Step 1: Add the two missing metric names to the `want` slice**

Add these two entries to the `want` slice in `TestMetricsContainExpectedSeries`:

```go
"netbox_sidecar_netbox_fetch_retries_total",
"netbox_sidecar_zone_staleness_seconds",
```

Also update the comment on line 44 from "9" to "11".

**Step 2: Commit**

```
jj new && jj describe -m "test: add missing metrics to e2e TestMetricsContainExpectedSeries"
```

---

### Task 6: Migrate `zonemanager` tests to testify assert

**Files:**
- Modify: `internal/zonemanager/manager_test.go`

**Step 1: Add `assert` import and replace all bare `t.Error`/`t.Errorf` calls**

Replace the import block to include `assert`:

```go
import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

Then replace all occurrences:

| Old | New |
|-----|-----|
| `if mgr.HasExistingZones() { t.Error("expected false...") }` | `assert.False(t, mgr.HasExistingZones(), "expected false...")` |
| `if !mgr.HasExistingZones() { t.Error("expected true...") }` | `assert.True(t, mgr.HasExistingZones(), "expected true...")` |
| `if !strings.Contains(string(content), "host1") { t.Error(...) }` | `assert.Contains(t, string(content), "host1", ...)` |
| `if _, err := os.Stat(...); err != nil { t.Errorf(...) }` | `assert.FileExists(t, filepath.Join(dir, "db."+zone), ...)` |
| `if _, err := os.Stat(...); !os.IsNotExist(err) { t.Error(...) }` | `assert.NoFileExists(t, filepath.Join(dir, "db.old.example.org"), ...)` |
| `if info1.ModTime() != info2.ModTime() { t.Error(...) }` | `assert.Equal(t, info1.ModTime(), info2.ModTime(), ...)` |
| `if stats.Created != 2 { t.Errorf(...) }` | `assert.Equal(t, 2, stats.Created, ...)` |
| (same pattern for all `stats.Updated`, `stats.Deleted` checks) | `assert.Equal(t, N, stats.Field, ...)` |

**Step 2: Run tests**

Run: `make test.unit`
Expected: All PASS

**Step 3: Commit**

```
jj new && jj describe -m "refactor: migrate zonemanager tests to testify assert"
```

---

### Task 7: Migrate E2E metrics tests to testify assert

**Files:**
- Modify: `tests/e2e/metrics_test.go`

**Step 1: Replace `t.Errorf`/`t.Error` with `assert` calls**

Add `assert` import. Replace:

| Line | Old | New |
|------|-----|-----|
| 63 | `t.Errorf("metric %q not found...", name)` | `assert.Contains(t, body, name, "metric %q not found in /metrics output", name)` |
| 76 | `t.Error(...)` | `assert.Contains(t, body, \`netbox_sidecar_poll_total{result="success"}\`, ...)` |
| 90 | `t.Errorf(...)` | `assert.NotEqual(t, "0", parts[1], ...)` |
| 95 | `t.Error(...)` | `require.Fail(t, "netbox_sidecar_zones_active line not found...")` |

**Step 2: Commit**

```
jj new && jj describe -m "refactor: migrate e2e metrics tests to testify assert"
```

---

### Task 8: Fix documentation inconsistencies

**Files:**
- Modify: `README.md:6` — Go badge version
- Modify: `docs/configuration.md:104` — hostPort default
- Create: `cmd/analyzer/README.md` — referenced but missing

**Step 1: Fix Go badge**

Change line 6 of `README.md` from:
```
[![Go Version](https://img.shields.io/badge/Go-1.25.6-00ADD8?logo=go)](go.mod)
```
to:
```
[![Go Version](https://img.shields.io/badge/Go-1.25.7-00ADD8?logo=go)](go.mod)
```

**Step 2: Fix hostPort default in docs**

Change line 104 of `docs/configuration.md` from:
```
| `hostPort.enabled` | `true` | Expose primary CoreDNS on host port 53 |
```
to:
```
| `hostPort.enabled` | `false` | Expose primary CoreDNS on host port 53 |
```

**Step 3: Create `cmd/analyzer/README.md`**

Read `cmd/analyzer/main.go` first, then write a brief README covering:
- What the analyzer does (preview DNS records from a Netbox JSON dump)
- Usage: `go run ./cmd/analyzer/ --input dump.json`
- How to generate a dump: `./scripts/fetch_netbox_ips.sh`

Keep it short (20-30 lines).

**Step 4: Commit**

```
jj new && jj describe -m "docs: fix badge version, hostPort default, add analyzer README"
```

---

### Task 9: Add `.golangci.yaml` configuration

**Files:**
- Create: `.golangci.yaml`

**Step 1: Create a minimal but explicit lint config**

```yaml
run:
  timeout: 5m

linters:
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
    - gosimple
    - gocritic
    - gofmt
    - misspell

issues:
  exclude-dirs:
    - vendor
  # Don't limit the number of issues per linter
  max-issues-per-linter: 0
  max-same-issues: 0
```

**Step 2: Run lint and fix any new findings**

Run: `golangci-lint run ./...`
Expected: PASS (or fix any findings)

**Step 3: Commit**

```
jj new && jj describe -m "chore: add explicit .golangci.yaml configuration"
```

---

### Task 10: Add note about E2E on PRs to CI docs

This is a documentation-only task — actually running E2E on PRs requires CI infra changes that are out of scope.

**Files:**
- Modify: `docs/development.md` — add a note about E2E coverage gap

**Step 1: Read `docs/development.md` and add a "Known Limitations" section**

Add at the end of the doc:

```markdown
## Known Limitations

- **E2E tests only run on version tag pushes** (`v*.*.*`) and manual workflow dispatch, not on PRs. This means E2E regressions can reach `main` undetected. To run E2E locally before merging, use `make dev && make dev.wait && make test.e2e`.
```

**Step 2: Commit**

```
jj new && jj describe -m "docs: note that E2E tests do not run on PRs"
```

---

## Summary

| Task | Type | Risk | Files |
|------|------|------|-------|
| 1. ipcategorizer tests | test | low | 1 new |
| 2. config tests | test | low | 1 new |
| 3. FetchZones pagination | bugfix | medium | 2 modified |
| 4. CoreDNS Dockerfile Go version | chore | low | 1 modified |
| 5. E2E missing metrics | test | low | 1 modified |
| 6. zonemanager testify migration | refactor | low | 1 modified |
| 7. E2E testify migration | refactor | low | 1 modified |
| 8. Doc inconsistencies | docs | low | 3 modified/new |
| 9. golangci config | chore | low | 1 new |
| 10. E2E-on-PRs note | docs | low | 1 modified |
