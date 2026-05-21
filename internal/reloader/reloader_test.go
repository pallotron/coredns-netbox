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
	"google.golang.org/grpc/metadata"
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
