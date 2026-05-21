# gRPC Zone Reload Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Augment CoreDNS zone serving with an explicit gRPC `Reload()` endpoint so the sidecar can trigger immediate zone refreshes across all replicas, while retaining periodic polling as a fallback.

**Architecture:** A new CoreDNS plugin (`netboxreload`) replaces `auto`. It loads zone files from a directory at startup, serves DNS from in-memory zones, and runs two reload paths: (1) a gRPC `ZoneReloadService.Reload()` endpoint for fast notification after each sidecar zone write, and (2) a background poll loop (configurable interval, default 60s) as a safety net if gRPC delivery fails. Both paths call the same `reloadZones()` function. After each successful zone write, the sidecar calls `Reload()` on each CoreDNS pod in parallel via their per-pod headless-service DNS names. The headless Service (already present) provides stable per-pod addresses for StatefulSet pods.

**Tech Stack:** Go 1.23, `github.com/miekg/dns` (zone parsing/serving, already in go.mod), `google.golang.org/grpc` (already in go.mod), `protoc` (proto codegen via `make proto`), Helm 3.

---

## Context for the implementer

- Repo: `~/src/coredns-netbox`
- CoreDNS is built from a custom `coredns/Dockerfile` that replaces `plugin.cfg` and builds a binary; the binary is a standard CoreDNS build with custom plugin ordering.
- Zone files live in `/zones` (configurable), named `db.<zonename>` (e.g. `db.infra.cx`).
- The sidecar writes zones atomically, incrementing the SOA serial on each write. Currently CoreDNS's `auto` plugin polls every 10s for serial changes.
- The gRPC infrastructure, auth interceptor, and proto toolchain already exist — follow existing patterns.
- `make proto` regenerates Go from proto. `make test.unit` runs unit tests. `make test.helm` validates Helm templates.

---

## Task 1: Add ZoneReloadService to proto

**Files:**
- Modify: `proto/coredns_netbox/v1/zones.proto`

**Step 1: Add the service and messages**

Append to the end of `zones.proto` (after the existing `GetStatusResponse` message):

```proto
service ZoneReloadService {
  rpc Reload(ZoneReloadRequest) returns (ZoneReloadResponse);
}

message ZoneReloadRequest  {}
message ZoneReloadResponse {}
```

**Step 2: Regenerate Go code**

```bash
cd ~/src/coredns-netbox
make proto
```

Expected: regenerates `proto/coredns_netbox/v1/zones.pb.go` and `zones_grpc.pb.go`, adding `ZoneReloadServiceClient`, `ZoneReloadServiceServer`, and `UnimplementedZoneReloadServiceServer`.

**Step 3: Verify it compiles**

```bash
go build ./proto/...
```

Expected: exits 0 with no output.

**Step 4: Commit**

```bash
jj new
jj describe -m "proto: add ZoneReloadService.Reload()"
```

---

## Task 2: CoreDNS plugin — zone loading

**Files:**
- Create: `coredns/plugins/netboxreload/zones.go`
- Create: `coredns/plugins/netboxreload/zones_test.go`

**Step 1: Write the failing test**

Create `coredns/plugins/netboxreload/zones_test.go`:

```go
package netboxreload

import (
    "os"
    "path/filepath"
    "testing"

    "github.com/miekg/dns"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestLoadZoneFile(t *testing.T) {
    dir := t.TempDir()
    content := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (
    2026052101 3600 900 604800 86400
)
@ IN NS ns1.infra.cx.
server1 IN A 10.0.0.1
server1 IN AAAA 2001:db8::1
`
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(content), 0o644))

    z, err := loadZoneFile(filepath.Join(dir, "db.infra.cx"))
    require.NoError(t, err)

    assert.Equal(t, "infra.cx.", z.origin)
    aRecs := z.records["server1.infra.cx."]
    require.Len(t, aRecs, 2)
    assert.Equal(t, dns.TypeA, aRecs[0].Header().Rrtype)
    assert.Equal(t, dns.TypeAAAA, aRecs[1].Header().Rrtype)
    apexRecs := z.records["infra.cx."]
    require.NotEmpty(t, apexRecs)
    types := make(map[uint16]bool)
    for _, rr := range apexRecs {
        types[rr.Header().Rrtype] = true
    }
    assert.True(t, types[dns.TypeSOA])
    assert.True(t, types[dns.TypeNS])
}

func TestLoadZoneDir(t *testing.T) {
    dir := t.TempDir()
    zone1 := `$ORIGIN a.cx.
$TTL 300
@ IN SOA ns1.a.cx. admin.a.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.a.cx.
host1 IN A 10.1.0.1
`
    zone2 := `$ORIGIN b.cx.
$TTL 300
@ IN SOA ns1.b.cx. admin.b.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.b.cx.
host2 IN A 10.2.0.1
`
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.a.cx"), []byte(zone1), 0o644))
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.b.cx"), []byte(zone2), 0o644))
    // non-zone file should be ignored
    require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.json"), []byte("{}"), 0o644))

    zones, err := loadZoneDir(dir)
    require.NoError(t, err)
    assert.Len(t, zones, 2)
    assert.Contains(t, zones, "a.cx.")
    assert.Contains(t, zones, "b.cx.")
}
```

**Step 2: Run test to verify it fails**

```bash
cd ~/src/coredns-netbox
go test ./coredns/plugins/netboxreload/ -run TestLoadZone -v 2>&1 | head -20
```

Expected: `cannot find package` or `no Go files`.

**Step 3: Implement zones.go**

Create `coredns/plugins/netboxreload/zones.go`:

```go
package netboxreload

import (
    "fmt"
    "os"
    "path/filepath"
    "strings"

    "github.com/miekg/dns"
)

// zone holds all DNS records for a single zone, keyed by lowercased owner name.
type zone struct {
    origin  string              // e.g. "infra.cx."
    records map[string][]dns.RR // lowercased FQDN -> RRs
}

// loadZoneFile parses a zone file at path. The zone origin is derived from the
// filename: "db.infra.cx" → origin "infra.cx.".
func loadZoneFile(path string) (*zone, error) {
    base := filepath.Base(path)
    if !strings.HasPrefix(base, "db.") {
        return nil, fmt.Errorf("unexpected zone filename %q: must start with db.", base)
    }
    origin := dns.Fqdn(strings.TrimPrefix(base, "db."))

    f, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    z := &zone{
        origin:  origin,
        records: make(map[string][]dns.RR),
    }

    zp := dns.NewZoneParser(f, origin, path)
    for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
        name := strings.ToLower(rr.Header().Name)
        z.records[name] = append(z.records[name], rr)
    }
    if err := zp.Err(); err != nil {
        return nil, fmt.Errorf("parse %s: %w", path, err)
    }
    return z, nil
}

// loadZoneDir scans dir for files named db.* and loads each as a zone.
// Returns a map from zone origin (e.g. "infra.cx.") to *zone.
func loadZoneDir(dir string) (map[string]*zone, error) {
    entries, err := os.ReadDir(dir)
    if err != nil {
        return nil, fmt.Errorf("read zone dir %s: %w", dir, err)
    }

    zones := make(map[string]*zone)
    for _, e := range entries {
        if e.IsDir() || !strings.HasPrefix(e.Name(), "db.") {
            continue
        }
        z, err := loadZoneFile(filepath.Join(dir, e.Name()))
        if err != nil {
            return nil, err
        }
        zones[z.origin] = z
    }
    return zones, nil
}
```

**Step 4: Run test to verify it passes**

```bash
go test ./coredns/plugins/netboxreload/ -run TestLoadZone -v
```

Expected: both tests PASS.

**Step 5: Commit**

```bash
jj new
jj describe -m "netboxreload: zone file loading"
```

---

## Task 3: CoreDNS plugin — DNS handler

**Files:**
- Create: `coredns/plugins/netboxreload/plugin.go`
- Create: `coredns/plugins/netboxreload/plugin_test.go`

**Step 1: Write the failing tests**

Create `coredns/plugins/netboxreload/plugin_test.go`:

```go
package netboxreload

import (
    "context"
    "os"
    "path/filepath"
    "testing"

    "github.com/miekg/dns"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// responseRecorder captures the DNS message written by ServeDNS.
type responseRecorder struct {
    dns.ResponseWriter
    msg *dns.Msg
}

func (r *responseRecorder) WriteMsg(m *dns.Msg) error {
    r.msg = m
    return nil
}

func newTestPlugin(t *testing.T, zoneContent, zoneName string) *Plugin {
    t.Helper()
    dir := t.TempDir()
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db."+zoneName), []byte(zoneContent), 0o644))
    zones, err := loadZoneDir(dir)
    require.NoError(t, err)
    return &Plugin{Dir: dir, zones: zones}
}

const testZone = `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
server1 IN A 10.0.0.1
server2 IN AAAA 2001:db8::2
`

func TestServeDNS_A(t *testing.T) {
    p := newTestPlugin(t, testZone, "infra.cx")
    req := new(dns.Msg)
    req.SetQuestion("server1.infra.cx.", dns.TypeA)
    rw := &responseRecorder{ResponseWriter: &dns.Conn{}}
    code, err := p.ServeDNS(context.Background(), rw, req)
    require.NoError(t, err)
    assert.Equal(t, dns.RcodeSuccess, code)
    require.Len(t, rw.msg.Answer, 1)
    a := rw.msg.Answer[0].(*dns.A)
    assert.Equal(t, "10.0.0.1", a.A.String())
}

func TestServeDNS_AAAA(t *testing.T) {
    p := newTestPlugin(t, testZone, "infra.cx")
    req := new(dns.Msg)
    req.SetQuestion("server2.infra.cx.", dns.TypeAAAA)
    rw := &responseRecorder{ResponseWriter: &dns.Conn{}}
    code, err := p.ServeDNS(context.Background(), rw, req)
    require.NoError(t, err)
    assert.Equal(t, dns.RcodeSuccess, code)
    require.Len(t, rw.msg.Answer, 1)
}

func TestServeDNS_NXDOMAIN(t *testing.T) {
    p := newTestPlugin(t, testZone, "infra.cx")
    req := new(dns.Msg)
    req.SetQuestion("notexist.infra.cx.", dns.TypeA)
    rw := &responseRecorder{ResponseWriter: &dns.Conn{}}
    code, err := p.ServeDNS(context.Background(), rw, req)
    require.NoError(t, err)
    assert.Equal(t, dns.RcodeSuccess, code)
    assert.Equal(t, dns.RcodeNameError, rw.msg.Rcode)
    // SOA in authority section
    assert.NotEmpty(t, rw.msg.Ns)
}

func TestServeDNS_NoZonePassesThrough(t *testing.T) {
    p := newTestPlugin(t, testZone, "infra.cx")
    // query for a name outside any loaded zone
    req := new(dns.Msg)
    req.SetQuestion("example.com.", dns.TypeA)
    rw := &responseRecorder{ResponseWriter: &dns.Conn{}}
    // Next is nil → NextOrFailure returns SERVFAIL
    code, _ := p.ServeDNS(context.Background(), rw, req)
    assert.Equal(t, dns.RcodeServerFailure, code)
}

func TestReloadZones(t *testing.T) {
    dir := t.TempDir()
    v1 := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
old IN A 10.0.0.1
`
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(v1), 0o644))
    zones, err := loadZoneDir(dir)
    require.NoError(t, err)
    p := &Plugin{Dir: dir, zones: zones}

    // overwrite zone with new record
    v2 := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052102 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
new IN A 10.0.0.2
`
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(v2), 0o644))
    require.NoError(t, p.reloadZones())

    p.mu.RLock()
    defer p.mu.RUnlock()
    assert.NotContains(t, p.zones["infra.cx."].records, "old.infra.cx.")
    assert.Contains(t, p.zones["infra.cx."].records, "new.infra.cx.")
}
```

**Step 2: Run tests to verify they fail**

```bash
go test ./coredns/plugins/netboxreload/ -run TestServeDNS -v 2>&1 | head -10
```

Expected: compilation error — Plugin not defined.

**Step 3: Implement plugin.go**

Create `coredns/plugins/netboxreload/plugin.go`:

```go
package netboxreload

import (
    "context"
    "log/slog"
    "strings"
    "sync"
    "time"

    "github.com/coredns/coredns/plugin"
    "github.com/miekg/dns"
)

const pluginName = "netboxreload"

// Plugin serves DNS from zone files loaded from Dir. It exposes a gRPC
// ZoneReloadService.Reload() endpoint for fast reload notification and runs
// a background poll loop as a fallback safety net.
type Plugin struct {
    Next         plugin.Handler
    Dir          string
    Port         string        // gRPC listen port, e.g. ":8054"
    PollInterval time.Duration // fallback poll interval; 0 disables polling

    mu    sync.RWMutex
    zones map[string]*zone // origin (e.g. "infra.cx.") -> loaded zone
}

func (p *Plugin) Name() string { return pluginName }

// ServeDNS answers the query from in-memory zones. If no zone matches the
// query name the request is passed to the next plugin.
func (p *Plugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    if len(r.Question) == 0 {
        return plugin.NextOrFailure(pluginName, p.Next, ctx, w, r)
    }

    q := r.Question[0]
    name := strings.ToLower(q.Name)

    p.mu.RLock()
    z := p.findZone(name)
    p.mu.RUnlock()

    if z == nil {
        return plugin.NextOrFailure(pluginName, p.Next, ctx, w, r)
    }

    m := new(dns.Msg)
    m.SetReply(r)
    m.Authoritative = true
    m.RecursionAvailable = false

    p.mu.RLock()
    recs := z.records[name]
    for _, rr := range recs {
        if rr.Header().Rrtype == q.Qtype || q.Qtype == dns.TypeANY {
            m.Answer = append(m.Answer, dns.Copy(rr))
        }
    }
    p.mu.RUnlock()

    if len(m.Answer) == 0 {
        m.Rcode = dns.RcodeNameError
        p.mu.RLock()
        for _, rr := range z.records[z.origin] {
            if rr.Header().Rrtype == dns.TypeSOA {
                m.Ns = append(m.Ns, dns.Copy(rr))
                break
            }
        }
        p.mu.RUnlock()
    }

    _ = w.WriteMsg(m)
    return dns.RcodeSuccess, nil
}

// findZone returns the best-matching zone for name by walking up labels.
// Caller must hold p.mu.RLock.
func (p *Plugin) findZone(name string) *zone {
    for n := name; ; {
        if z, ok := p.zones[n]; ok {
            return z
        }
        idx := strings.Index(n, ".")
        if idx < 0 || idx == len(n)-1 {
            break
        }
        n = n[idx+1:]
    }
    return nil
}

// pollLoop periodically calls reloadZones as a fallback in case gRPC delivery
// fails. Runs until ctx is cancelled. No-op if PollInterval is zero.
func (p *Plugin) pollLoop(ctx context.Context) {
    if p.PollInterval == 0 {
        return
    }
    ticker := time.NewTicker(p.PollInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := p.reloadZones(); err != nil {
                slog.Warn("netboxreload: poll reload failed", "err", err)
            }
        }
    }
}

// reloadZones re-reads Dir and atomically swaps the in-memory zone map.
func (p *Plugin) reloadZones() error {
    newZones, err := loadZoneDir(p.Dir)
    if err != nil {
        return err
    }
    p.mu.Lock()
    p.zones = newZones
    p.mu.Unlock()
    return nil
}
```

**Step 4: Run tests to verify they pass**

```bash
go test ./coredns/plugins/netboxreload/ -run "TestServeDNS|TestReloadZones" -v
```

Expected: all pass.

**Step 5: Commit**

```bash
jj new
jj describe -m "netboxreload: DNS handler and reload logic"
```

---

## Task 4: CoreDNS plugin — gRPC server

**Files:**
- Create: `coredns/plugins/netboxreload/server.go`
- Create: `coredns/plugins/netboxreload/server_test.go`

**Step 1: Write the failing test**

Create `coredns/plugins/netboxreload/server_test.go`:

```go
package netboxreload

import (
    "context"
    "net"
    "os"
    "path/filepath"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

func TestGRPCReload(t *testing.T) {
    dir := t.TempDir()
    v1 := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052101 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
host1 IN A 10.0.0.1
`
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(v1), 0o644))
    zones, err := loadZoneDir(dir)
    require.NoError(t, err)
    p := &Plugin{Dir: dir, zones: zones}

    lis, err := net.Listen("tcp", "127.0.0.1:0")
    require.NoError(t, err)
    srv := newGRPCServer(p)
    go srv.Serve(lis) //nolint:errcheck
    t.Cleanup(srv.Stop)

    conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
    require.NoError(t, err)
    defer conn.Close()

    client := pb.NewZoneReloadServiceClient(conn)

    // overwrite zone with new content before calling Reload
    v2 := `$ORIGIN infra.cx.
$TTL 300
@ IN SOA ns1.infra.cx. admin.infra.cx. (2026052102 3600 900 604800 86400)
@ IN NS ns1.infra.cx.
host2 IN A 10.0.0.2
`
    require.NoError(t, os.WriteFile(filepath.Join(dir, "db.infra.cx"), []byte(v2), 0o644))

    _, err = client.Reload(context.Background(), &pb.ZoneReloadRequest{})
    require.NoError(t, err)

    p.mu.RLock()
    defer p.mu.RUnlock()
    assert.Contains(t, p.zones["infra.cx."].records, "host2.infra.cx.")
    assert.NotContains(t, p.zones["infra.cx."].records, "host1.infra.cx.")
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./coredns/plugins/netboxreload/ -run TestGRPCReload -v 2>&1 | head -10
```

Expected: compilation error — `newGRPCServer` undefined.

**Step 3: Implement server.go**

Create `coredns/plugins/netboxreload/server.go`:

```go
package netboxreload

import (
    "context"
    "log/slog"
    "net"

    pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
    "google.golang.org/grpc"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/reflection"
    "google.golang.org/grpc/status"
)

type grpcServer struct {
    pb.UnimplementedZoneReloadServiceServer
    plugin *Plugin
}

func (s *grpcServer) Reload(_ context.Context, _ *pb.ZoneReloadRequest) (*pb.ZoneReloadResponse, error) {
    if err := s.plugin.reloadZones(); err != nil {
        slog.Error("netboxreload: zone reload failed", "err", err)
        return nil, status.Errorf(codes.Internal, "reload: %v", err)
    }
    slog.Info("netboxreload: zones reloaded via gRPC")
    return &pb.ZoneReloadResponse{}, nil
}

func newGRPCServer(p *Plugin) *grpc.Server {
    gs := grpc.NewServer()
    pb.RegisterZoneReloadServiceServer(gs, &grpcServer{plugin: p})
    reflection.Register(gs)
    return gs
}

// startGRPC starts the gRPC server on p.Port. Logs and returns on error.
func (p *Plugin) startGRPC() {
    lis, err := net.Listen("tcp", p.Port)
    if err != nil {
        slog.Error("netboxreload: gRPC listen failed", "port", p.Port, "err", err)
        return
    }
    slog.Info("netboxreload: gRPC server listening", "addr", lis.Addr())
    if err := newGRPCServer(p).Serve(lis); err != nil {
        slog.Error("netboxreload: gRPC server exited", "err", err)
    }
}
```

**Step 4: Run tests to verify they pass**

```bash
go test ./coredns/plugins/netboxreload/ -v
```

Expected: all tests pass.

**Step 5: Commit**

```bash
jj new
jj describe -m "netboxreload: gRPC ZoneReloadService server"
```

---

## Task 5: CoreDNS plugin — Corefile setup

**Files:**
- Create: `coredns/plugins/netboxreload/setup.go`
- Create: `coredns/plugins/netboxreload/setup_test.go`

**Step 1: Write the failing test**

Create `coredns/plugins/netboxreload/setup_test.go`:

```go
package netboxreload

import (
    "testing"

    "github.com/coredns/caddy"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestParseConfig(t *testing.T) {
    tests := []struct {
        name         string
        input        string
        wantDir      string
        wantPort     string
        wantInterval time.Duration
    }{
        {
            name:         "explicit dir, port, and reload interval",
            input:        `netboxreload { directory /zones grpc_port :9054 reload 30s }`,
            wantDir:      "/zones",
            wantPort:     ":9054",
            wantInterval: 30 * time.Second,
        },
        {
            name:         "disable polling with reload 0s",
            input:        `netboxreload { directory /zones reload 0s }`,
            wantDir:      "/zones",
            wantPort:     ":8054",
            wantInterval: 0,
        },
        {
            name:         "defaults",
            input:        `netboxreload`,
            wantDir:      "/zones",
            wantPort:     ":8054",
            wantInterval: 60 * time.Second,
        },
    }
    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            c := caddy.NewTestController("dns", tc.input)
            p, err := parseConfig(c)
            require.NoError(t, err)
            assert.Equal(t, tc.wantDir, p.Dir)
            assert.Equal(t, tc.wantPort, p.Port)
            assert.Equal(t, tc.wantInterval, p.PollInterval)
        })
    }
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./coredns/plugins/netboxreload/ -run TestParseConfig -v 2>&1 | head -10
```

Expected: compilation error — `parseConfig` undefined.

**Step 3: Implement setup.go**

Create `coredns/plugins/netboxreload/setup.go`:

```go
package netboxreload

import (
    "github.com/coredns/caddy"
    "github.com/coredns/coredns/core/dnsserver"
    "github.com/coredns/coredns/plugin"
)

func init() {
    plugin.Register(pluginName, setup)
}

func setup(c *caddy.Controller) error {
    p, err := parseConfig(c)
    if err != nil {
        return plugin.Error(pluginName, err)
    }

    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        p.Next = next
        return p
    })

    c.OnStartup(func() error {
        if err := p.reloadZones(); err != nil {
            return plugin.Error(pluginName, err)
        }
        go p.startGRPC()
        ctx := context.TODO() // CoreDNS manages plugin lifecycle via OnShutdown
        go p.pollLoop(ctx)
        return nil
    })

    return nil
}

func parseConfig(c *caddy.Controller) (*Plugin, error) {
    p := &Plugin{
        Dir:          "/zones",
        Port:         ":8054",
        PollInterval: 60 * time.Second,
    }

    for c.Next() {
        for c.NextBlock() {
            switch c.Val() {
            case "directory":
                if !c.NextArg() {
                    return nil, c.ArgErr()
                }
                p.Dir = c.Val()
            case "grpc_port":
                if !c.NextArg() {
                    return nil, c.ArgErr()
                }
                p.Port = c.Val()
            case "reload":
                if !c.NextArg() {
                    return nil, c.ArgErr()
                }
                d, err := time.ParseDuration(c.Val())
                if err != nil {
                    return nil, c.Errf("invalid reload duration %q: %v", c.Val(), err)
                }
                p.PollInterval = d
            default:
                return nil, c.Errf("unknown option %q", c.Val())
            }
        }
    }
    return p, nil
}
```

**Step 4: Run tests to verify they pass**

```bash
go test ./coredns/plugins/netboxreload/ -v
```

Expected: all tests pass.

**Step 5: Commit**

```bash
jj new
jj describe -m "netboxreload: Corefile setup and plugin registration"
```

---

## Task 6: Register plugin in CoreDNS build

**Files:**
- Modify: `coredns/plugin.cfg`
- Modify: `coredns/Dockerfile` (verify the module import is wired)

**Step 1: Add plugin to plugin.cfg**

In `coredns/plugin.cfg`, add one line **before** `auto:auto`:

```
netboxreload:github.com/pallotron/coredns-netbox/coredns/plugins/netboxreload
```

The relevant section after the change:
```
...
file:file
netboxreload:github.com/pallotron/coredns-netbox/coredns/plugins/netboxreload
auto:auto
secondary:secondary
...
```

**Step 2: Verify the CoreDNS Docker build**

Check that `coredns/Dockerfile` uses `go work` or has the module available. Open it:

```bash
cat ~/src/coredns-netbox/coredns/Dockerfile
```

The Dockerfile likely builds CoreDNS by copying `plugin.cfg` over the upstream one. The `coredns-netbox` module must be available in the build context. Look for how other local plugins are referenced; add the `coredns/plugins/netboxreload` package accordingly.

If the Dockerfile uses `go get` to pull the module, and the plugin is in the same repo, ensure the build stage has the full source. Typical pattern: the Dockerfile `COPY`s the local source tree and adds a `replace` directive to `go.mod`.

**Step 3: Verify module compiles in the context of CoreDNS**

```bash
cd ~/src/coredns-netbox
go build ./coredns/... 2>&1 || true
```

Note: The CoreDNS main binary is built inside Docker, so compilation here only checks the plugin package itself, not the full CoreDNS build. The Docker build in a later task validates the full binary.

**Step 4: Commit**

```bash
jj new
jj describe -m "coredns: register netboxreload plugin in plugin.cfg"
```

---

## Task 7: Sidecar — CoreDNS reload notification

**Files:**
- Create: `internal/reloader/reloader.go`
- Create: `internal/reloader/reloader_test.go`
- Modify: `internal/config/config.go` (add `CoreDNSReloadAddrs`)
- Modify: `cmd/sidecar/main.go` (wire reloader into poll loop)

**Step 1: Write the failing test**

Create `internal/reloader/reloader_test.go`:

```go
package reloader_test

import (
    "context"
    "net"
    "testing"

    pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
    "github.com/pallotron/coredns-netbox/internal/reloader"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "google.golang.org/grpc"
)

// fakeReloadServer records calls.
type fakeReloadServer struct {
    pb.UnimplementedZoneReloadServiceServer
    called int
}

func (f *fakeReloadServer) Reload(_ context.Context, _ *pb.ZoneReloadRequest) (*pb.ZoneReloadResponse, error) {
    f.called++
    return &pb.ZoneReloadResponse{}, nil
}

func startFakeServer(t *testing.T) (addr string, srv *fakeReloadServer) {
    t.Helper()
    lis, err := net.Listen("tcp", "127.0.0.1:0")
    require.NoError(t, err)
    srv = &fakeReloadServer{}
    gs := grpc.NewServer()
    pb.RegisterZoneReloadServiceServer(gs, srv)
    go gs.Serve(lis) //nolint:errcheck
    t.Cleanup(gs.Stop)
    return lis.Addr().String(), srv
}

func TestReloader_NotifiesAllAddrs(t *testing.T) {
    addr1, srv1 := startFakeServer(t)
    addr2, srv2 := startFakeServer(t)

    r := reloader.New([]string{addr1, addr2}, "")
    r.Reload(context.Background())

    assert.Equal(t, 1, srv1.called)
    assert.Equal(t, 1, srv2.called)
}

func TestReloader_SkipsEmptyAddrs(t *testing.T) {
    // No addrs configured — Reload() is a no-op
    r := reloader.New(nil, "")
    r.Reload(context.Background()) // must not panic
}
```

**Step 2: Run test to verify it fails**

```bash
go test ./internal/reloader/... -v 2>&1 | head -10
```

Expected: `cannot find package`.

**Step 3: Implement reloader.go**

Create `internal/reloader/reloader.go`:

```go
package reloader

import (
    "context"
    "log/slog"
    "sync"
    "time"

    pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/grpc/metadata"
)

// Reloader calls ZoneReloadService.Reload() on each configured CoreDNS pod.
type Reloader struct {
    addrs []string
    token string
}

// New creates a Reloader that will call Reload() on all addrs (host:port).
// token is the bearer token for gRPC auth; empty means no auth header sent.
func New(addrs []string, token string) *Reloader {
    return &Reloader{addrs: addrs, token: token}
}

// Reload fans out Reload() RPCs to all configured addresses in parallel.
// Errors are logged but do not propagate — zone files are on disk and CoreDNS
// will pick them up on its fallback poll interval regardless.
func (r *Reloader) Reload(ctx context.Context) {
    if len(r.addrs) == 0 {
        return
    }
    var wg sync.WaitGroup
    for _, addr := range r.addrs {
        wg.Add(1)
        go func(addr string) {
            defer wg.Done()
            if err := r.reloadOne(ctx, addr); err != nil {
                slog.Warn("coredns reload failed", "addr", addr, "err", err)
            }
        }(addr)
    }
    wg.Wait()
}

func (r *Reloader) reloadOne(ctx context.Context, addr string) error {
    conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil {
        return err
    }
    defer conn.Close()

    callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    if r.token != "" {
        md := metadata.New(map[string]string{"authorization": "bearer " + r.token})
        callCtx = metadata.NewOutgoingContext(callCtx, md)
    }

    _, err = pb.NewZoneReloadServiceClient(conn).Reload(callCtx, &pb.ZoneReloadRequest{})
    return err
}
```

**Step 4: Run tests to verify they pass**

```bash
go test ./internal/reloader/... -v
```

Expected: both tests pass.

**Step 5: Add CoreDNSReloadAddrs to config**

In `internal/config/config.go`, add to the `Config` struct:

```go
// CoreDNS reload configuration
CoreDNSReloadAddrs []string // host:port addresses to call Reload() on after zone write
CoreDNSReloadToken string   // bearer token for CoreDNS gRPC reload endpoint
```

In `Load()`, add after the `GRPCAuthToken` line:

```go
CoreDNSReloadAddrs: parseZoneList(os.Getenv("COREDNS_RELOAD_ADDRS")),
CoreDNSReloadToken: os.Getenv("COREDNS_RELOAD_TOKEN"),
```

**Step 6: Wire reloader in main.go**

In `cmd/sidecar/main.go`, add import:

```go
"github.com/pallotron/coredns-netbox/internal/reloader"
```

After the `grpcSrv` setup block (around line 184), create the reloader:

```go
rl := reloader.New(cfg.CoreDNSReloadAddrs, cfg.CoreDNSReloadToken)
```

Pass `rl` to the `run()` function. Update the `run()` signature:

```go
func run(ctx context.Context, cfg *config.Config, ..., rl *reloader.Reloader) error {
```

Inside `run()`, update `doMergeAndWrite` to call reload on success. Change every site where `doMergeAndWrite` result is checked to:

```go
if mergeErr := doMergeAndWrite(lastNetboxZones); mergeErr == nil {
    rl.Reload(ctx)   // notify CoreDNS pods immediately
    lastSuccessTime = time.Now()
    // ... rest of existing success handling
}
```

Apply this to all three call sites: initial poll, ticker case, netboxSignal case, and mergeSignal case.

**Step 7: Run unit tests**

```bash
go test ./internal/... -v -count=1 -race
```

Expected: all pass.

**Step 8: Commit**

```bash
jj new
jj describe -m "sidecar: notify CoreDNS pods via gRPC Reload() after zone write"
```

---

## Task 8: Helm chart updates

**Files:**
- Modify: `helm/coredns-netbox/templates/configmap.yaml`
- Modify: `helm/coredns-netbox/templates/deployment.yaml`
- Modify: `helm/coredns-netbox/values.yaml`

**Step 1: Update Corefile in configmap.yaml**

Replace the `auto` block:

```yaml
        auto {
            directory {{ .Values.zoneDir }} db\.(.*) {1}
            reload 10s
        }
```

With `netboxreload`:

```yaml
        netboxreload {
            directory {{ .Values.zoneDir }}
            grpc_port {{ .Values.coredns.reloadGRPCPort | default ":8054" }}
            reload    {{ .Values.coredns.reloadPollInterval | default "60s" }}
        }
```

**Step 2: Expose gRPC reload port on CoreDNS container in deployment.yaml**

In the `coredns` container ports list, add:

```yaml
            - name: grpc-reload
              containerPort: 8054
              protocol: TCP
```

**Step 3: Add COREDNS_RELOAD_ADDRS to sidecar env in deployment.yaml**

In the `sidecar` container env section, add:

```yaml
            - name: COREDNS_RELOAD_ADDRS
              value: {{ include "coredns-netbox.reloadAddrs" . | quote }}
            - name: COREDNS_RELOAD_TOKEN
              value: ""
```

**Step 4: Add the reloadAddrs helper in _helpers.tpl**

Add at the bottom of `helm/coredns-netbox/templates/_helpers.tpl`:

```
{{/*
Compute comma-separated CoreDNS pod reload addresses for the sidecar.
When sidecar is a sidecar container (same pod), localhost suffices.
When sidecar.standalone is true, enumerate StatefulSet pod DNS names.
*/}}
{{- define "coredns-netbox.reloadAddrs" -}}
{{- if .Values.sidecar.standalone -}}
{{- $addrs := list -}}
{{- $name := include "coredns-netbox.fullname" . -}}
{{- $ns := .Release.Namespace -}}
{{- $port := .Values.coredns.reloadGRPCPort | default ":8054" | trimPrefix ":" -}}
{{- range $i := until (.Values.replicaCount | int) -}}
{{- $addrs = append $addrs (printf "%s-%d.%s-headless.%s.svc.cluster.local:%s" $name $i $name $ns $port) -}}
{{- end -}}
{{- join "," $addrs -}}
{{- else -}}
{{- printf "localhost%s" (.Values.coredns.reloadGRPCPort | default ":8054") -}}
{{- end -}}
{{- end -}}
```

**Step 5: Add values for new options**

In `helm/coredns-netbox/values.yaml`, add under the `coredns:` key:

```yaml
coredns:
  # ... existing values ...
  reloadGRPCPort: ":8054"   # gRPC port for ZoneReloadService
```

And under `sidecar:`:

```yaml
sidecar:
  # ... existing values ...
  standalone: false   # set true to run sidecar as a separate Deployment (requires RWX storage)
```

**Step 6: Validate Helm templates**

```bash
cd ~/src/coredns-netbox
make test.helm
```

Expected: all existing checks pass. Add a new check for the `netboxreload` directive if desired.

**Step 7: Commit**

```bash
jj new
jj describe -m "helm: use netboxreload plugin, expose gRPC reload port, wire COREDNS_RELOAD_ADDRS"
```

---

## Task 9: Build and smoke-test Docker image

**Step 1: Build the CoreDNS image**

```bash
cd ~/src/coredns-netbox
make dev.images.coredns
```

Expected: image builds successfully. Watch for any plugin registration errors.

**Step 2: Start dev environment (if not already running)**

```bash
make dev.cluster
make dev.netbox
make dev.token
make dev.seed
```

**Step 3: Deploy and wait**

```bash
make dev.deploy dev.wait
```

Expected: primary DNS serves `server1-mgmt.mycompany.com`.

**Step 4: Verify gRPC reload works manually**

```bash
grpcurl -plaintext 127.0.0.1:18054 coredns_netbox.v1.ZoneReloadService/Reload
```

Expected: `{}` response with no error.

**Step 5: Commit**

```bash
jj new
jj describe -m "chore: verify dev environment works with netboxreload plugin"
```

---

## Task 10: E2E test — gRPC reload reduces propagation delay

**Files:**
- Modify: `tests/e2e/grpc_test.go`

**Step 1: Add reload test**

In `tests/e2e/grpc_test.go`, add:

```go
func TestZoneReloadService(t *testing.T) {
    // Verify that Reload() RPC is reachable and returns no error.
    // After a forced Netbox poll+write, calling Reload() should return immediately
    // and subsequent DNS queries should reflect the current zone state.
    conn, err := grpc.NewClient(
        grpcAddr(),
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
    require.NoError(t, err)
    defer conn.Close()

    // First trigger sidecar to rewrite zones
    ctrl := pb.NewControlServiceClient(conn)
    ctx := grpcContext(t)
    _, err = ctrl.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{})
    require.NoError(t, err)

    // Wait for write to complete
    require.Eventually(t, func() bool {
        s, err := ctrl.GetStatus(grpcContext(t), &pb.GetStatusRequest{})
        return err == nil && s.ZoneStalenessSeconds < 5
    }, 30*time.Second, 500*time.Millisecond, "zone write did not complete")

    // Call Reload directly on CoreDNS
    reloadAddr := os.Getenv("COREDNS_RELOAD_ADDR") // e.g. 127.0.0.1:18054
    if reloadAddr == "" {
        t.Skip("COREDNS_RELOAD_ADDR not set")
    }
    rconn, err := grpc.NewClient(reloadAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
    require.NoError(t, err)
    defer rconn.Close()

    _, err = pb.NewZoneReloadServiceClient(rconn).Reload(grpcContext(t), &pb.ZoneReloadRequest{})
    require.NoError(t, err)
}
```

**Step 2: Run existing e2e suite to confirm no regression**

```bash
STRIP_DC_LABEL=true DC_LABEL_REWRITE=true GRPC_AUTH_TOKEN=devtoken \
COREDNS_RELOAD_ADDR=127.0.0.1:18054 \
go test ./tests/e2e/... -v -count=1 -tags=e2e -run TestZoneReloadService
```

Expected: PASS.

**Step 3: Commit**

```bash
jj new
jj describe -m "e2e: add TestZoneReloadService for gRPC reload endpoint"
```

---

## Deferred: Standalone sidecar Deployment + RWX storage

The Helm `sidecar.standalone: true` path (separate sidecar Deployment using RWX storage) is wired in Task 8 but not implemented. When `standalone: true`:

1. Remove the `sidecar` container from `deployment.yaml`
2. Create `helm/coredns-netbox/templates/deployment-sidecar.yaml` — a standalone Deployment with 1 replica
3. Switch `volumeClaimTemplates` to `accessModes: [ReadWriteMany]` when `zoneStorage.accessMode` is set
4. The init container (`zone-init`) needs access to zones — with RWX, any pod on any node can mount

This is a follow-up PR after the core gRPC reload is merged and validated.

---

**Plan complete and saved to `docs/plans/2026-05-21-grpc-zone-reload.md`.**

**Two execution options:**

**1. Subagent-Driven (this session)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Parallel Session (separate)** — Open a new session in the repo dir with `superpowers:executing-plans`, batch execution with checkpoints

**Which approach?**
