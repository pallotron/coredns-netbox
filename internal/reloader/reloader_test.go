package reloader_test

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/reloader"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeReloadServer records calls and can fail the first failFirst of them.
type fakeReloadServer struct {
	pb.UnimplementedZoneReloadServiceServer
	called    int
	failFirst int
}

func (f *fakeReloadServer) Reload(_ context.Context, _ *pb.ZoneReloadRequest) (*pb.ZoneReloadResponse, error) {
	f.called++
	if f.called <= f.failFirst {
		return nil, status.Error(codes.Unavailable, "not ready yet")
	}
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

// counterValue reads the current value of a counter without pulling in the
// testutil module (which would add two test-only dependencies to go.mod).
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

func TestReloader_RetriesFailedPush(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.failFirst = 2

	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)
	r := reloader.New([]string{addr}, "")
	r.ResultCounter = m.CoreDNSReloadTotal
	r.RetryCounter = m.CoreDNSReloadRetriesTotal
	r.RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	r.Reload(context.Background())

	assert.Equal(t, 3, srv.called, "two failures then a successful retry")
	assert.Equal(t, 1.0, counterValue(t, m.CoreDNSReloadTotal.WithLabelValues("success")))
	assert.Equal(t, 0.0, counterValue(t, m.CoreDNSReloadTotal.WithLabelValues("error")))
	assert.Equal(t, 2.0, counterValue(t, m.CoreDNSReloadRetriesTotal))
}

func TestReloader_GivesUpAfterRetries(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.failFirst = 10

	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)
	r := reloader.New([]string{addr}, "")
	r.ResultCounter = m.CoreDNSReloadTotal
	r.RetryCounter = m.CoreDNSReloadRetriesTotal
	r.RetryDelays = []time.Duration{time.Millisecond}
	r.Reload(context.Background()) // must not hang or panic

	assert.Equal(t, 2, srv.called, "initial attempt plus one retry")
	assert.Equal(t, 1.0, counterValue(t, m.CoreDNSReloadTotal.WithLabelValues("error")))
	assert.Equal(t, 1.0, counterValue(t, m.CoreDNSReloadRetriesTotal))
}

func TestReloader_NilCountersAreSafe(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.failFirst = 1

	r := reloader.New([]string{addr}, "")
	r.RetryDelays = []time.Duration{time.Millisecond}
	r.Reload(context.Background()) // no counters wired — must not panic

	assert.Equal(t, 2, srv.called)
}

func TestReloader_SendsAuthToken(t *testing.T) {
	var gotAuth string
	authInterceptor := func(
		ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
	) (any, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			vals := md.Get("authorization")
			if len(vals) > 0 {
				gotAuth = vals[0]
			}
		}
		return handler(ctx, req)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &fakeReloadServer{}
	gs := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor))
	pb.RegisterZoneReloadServiceServer(gs, srv)
	go gs.Serve(lis) //nolint:errcheck
	t.Cleanup(gs.Stop)

	r := reloader.New([]string{lis.Addr().String()}, "testtoken")
	r.Reload(context.Background())

	assert.Equal(t, 1, srv.called)
	assert.Equal(t, "bearer testtoken", gotAuth)
}

// gaugeValue reads the current value of a gauge without the testutil module.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, g.Write(&m))
	return m.GetGauge().GetValue()
}

// A push that exhausts its retries must leave the address dirty so the next
// Reconcile call retries it; a successful push clears the dirty state and
// makes further Reconcile calls a no-op.
func TestReloader_ReconcileRetriesDirtyAddress(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.failFirst = 2

	reg := prometheus.NewRegistry()
	m := metrics.NewSidecar(reg)
	r := reloader.New([]string{addr}, "")
	r.ResultCounter = m.CoreDNSReloadTotal
	r.DirtyGauge = m.CoreDNSReloadDirtyTargets
	r.RetryDelays = nil // one attempt per cycle

	r.Reload(context.Background()) // fails, addr stays dirty
	assert.Equal(t, 1, srv.called)
	assert.Equal(t, 1.0, gaugeValue(t, m.CoreDNSReloadDirtyTargets))

	r.Reconcile(context.Background()) // fails again, still dirty
	assert.Equal(t, 2, srv.called)
	assert.Equal(t, 1.0, gaugeValue(t, m.CoreDNSReloadDirtyTargets))

	r.Reconcile(context.Background()) // succeeds, clears dirty
	assert.Equal(t, 3, srv.called)
	assert.Equal(t, 0.0, gaugeValue(t, m.CoreDNSReloadDirtyTargets))

	r.Reconcile(context.Background())
	assert.Equal(t, 3, srv.called, "reconcile on a clean reloader must not push")

	assert.Equal(t, 2.0, counterValue(t, m.CoreDNSReloadTotal.WithLabelValues("error")))
	assert.Equal(t, 1.0, counterValue(t, m.CoreDNSReloadTotal.WithLabelValues("success")))
}

// Reload represents a zone change: it must push to every address, including
// ones already clean from a previous cycle.
func TestReloader_ReloadRemarksCleanAddrs(t *testing.T) {
	addr, srv := startFakeServer(t)

	r := reloader.New([]string{addr}, "")
	r.Reload(context.Background())
	assert.Equal(t, 1, srv.called)

	r.Reload(context.Background())
	assert.Equal(t, 2, srv.called, "a new zone change must push to clean addrs too")
}

// Reconcile before any Reload must be a no-op: nothing is dirty yet.
func TestReloader_ReconcileBeforeReloadIsNoOp(t *testing.T) {
	addr, srv := startFakeServer(t)

	r := reloader.New([]string{addr}, "")
	r.Reconcile(context.Background())
	assert.Equal(t, 0, srv.called)
}
