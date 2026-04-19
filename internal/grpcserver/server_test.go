package grpcserver_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/grpcserver"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1 << 20

func newTestConn(t *testing.T, token string) (*grpc.ClientConn, *grpcserver.NetboxCache, *grpcserver.StatusTracker) {
	t.Helper()
	store, err := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	require.NoError(t, err)

	cache := &grpcserver.NetboxCache{}
	st := &grpcserver.StatusTracker{}
	mergeSignal := make(chan struct{}, 1)
	netboxSignal := make(chan struct{}, 1)

	lis := bufconn.Listen(bufSize)
	srv := grpcserver.New(token, store, cache, st, mergeSignal, netboxSignal, nil)
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, cache, st
}

func authCtx(token string) context.Context {
	md := metadata.Pairs("authorization", "bearer "+token)
	return metadata.NewOutgoingContext(context.Background(), md)
}

// --- Auth interceptor tests ---

func TestAuthInterceptor_MissingToken(t *testing.T) {
	conn, _, _ := newTestConn(t, "secret")
	client := pb.NewDynamicZoneServiceClient(conn)
	_, err := client.ListZones(context.Background(), &pb.ListZonesRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, grpcstatus.Code(err))
}

func TestAuthInterceptor_WrongToken(t *testing.T) {
	conn, _, _ := newTestConn(t, "secret")
	client := pb.NewDynamicZoneServiceClient(conn)
	_, err := client.ListZones(authCtx("wrong"), &pb.ListZonesRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, grpcstatus.Code(err))
}

func TestAuthInterceptor_CorrectToken(t *testing.T) {
	conn, _, _ := newTestConn(t, "secret")
	client := pb.NewDynamicZoneServiceClient(conn)
	resp, err := client.ListZones(authCtx("secret"), &pb.ListZonesRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.Names)
}

func TestAuthInterceptor_DisabledWhenTokenEmpty(t *testing.T) {
	conn, _, _ := newTestConn(t, "")
	client := pb.NewDynamicZoneServiceClient(conn)
	resp, err := client.ListZones(context.Background(), &pb.ListZonesRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.Names)
}

// --- DynamicZoneService handler tests ---

func TestCreateAndListZones(t *testing.T) {
	conn, _, _ := newTestConn(t, "")
	client := pb.NewDynamicZoneServiceClient(conn)
	ctx := context.Background()

	_, err := client.CreateZone(ctx, &pb.CreateZoneRequest{Name: "k8s.example.org"})
	require.NoError(t, err)

	resp, err := client.ListZones(ctx, &pb.ListZonesRequest{})
	require.NoError(t, err)
	assert.Contains(t, resp.Names, "k8s.example.org")
}

func TestUpsertAndListRecords(t *testing.T) {
	conn, _, _ := newTestConn(t, "")
	client := pb.NewDynamicZoneServiceClient(conn)
	ctx := context.Background()

	_, err := client.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   "k8s.example.org",
		Record: &pb.Record{DnsName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4, Ttl: 60},
	})
	require.NoError(t, err)

	resp, err := client.ListRecords(ctx, &pb.ListRecordsRequest{Zone: "k8s.example.org"})
	require.NoError(t, err)
	require.Len(t, resp.Records, 1)
	assert.Equal(t, "node1.k8s.example.org", resp.Records[0].DnsName)
	assert.Equal(t, uint32(60), resp.Records[0].Ttl)
}

func TestUpsertRecord_ConflictRejected(t *testing.T) {
	conn, cache, _ := newTestConn(t, "")
	cache.Update(map[string][]netboxclient.IPRecord{
		"example.org": {{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4}},
	})
	client := pb.NewDynamicZoneServiceClient(conn)

	_, err := client.UpsertRecord(context.Background(), &pb.UpsertRecordRequest{
		Zone:   "example.org",
		Record: &pb.Record{DnsName: "host1.example.org", Address: "10.0.0.9", Family: 4},
		Force:  false,
	})
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, grpcstatus.Code(err))
}

func TestUpsertRecord_ForceOverridesNetbox(t *testing.T) {
	conn, cache, _ := newTestConn(t, "")
	cache.Update(map[string][]netboxclient.IPRecord{
		"example.org": {{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4}},
	})
	client := pb.NewDynamicZoneServiceClient(conn)

	_, err := client.UpsertRecord(context.Background(), &pb.UpsertRecordRequest{
		Zone:   "example.org",
		Record: &pb.Record{DnsName: "host1.example.org", Address: "10.0.0.9", Family: 4},
		Force:  true,
	})
	require.NoError(t, err)
}

func TestBatchUpsert_AtomicConflictRejection(t *testing.T) {
	conn, cache, _ := newTestConn(t, "")
	cache.Update(map[string][]netboxclient.IPRecord{
		"example.org": {{DNSName: "host1.example.org", Address: "10.0.0.1", Family: 4}},
	})
	client := pb.NewDynamicZoneServiceClient(conn)

	_, err := client.BatchUpsert(context.Background(), &pb.BatchUpsertRequest{
		ZoneRecords: []*pb.ZoneRecords{{
			Zone: "example.org",
			Records: []*pb.Record{
				{DnsName: "host1.example.org", Address: "10.0.0.9", Family: 4},
				{DnsName: "host2.example.org", Address: "10.0.0.2", Family: 4},
			},
		}},
		Force: false,
	})
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, grpcstatus.Code(err))
	assert.Contains(t, err.Error(), "host1.example.org")
}

func TestDeleteRecord(t *testing.T) {
	conn, _, _ := newTestConn(t, "")
	client := pb.NewDynamicZoneServiceClient(conn)
	ctx := context.Background()

	_, _ = client.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   "k8s.example.org",
		Record: &pb.Record{DnsName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4},
	})
	_, err := client.DeleteRecord(ctx, &pb.DeleteRecordRequest{
		Zone: "k8s.example.org", DnsName: "node1.k8s.example.org",
	})
	require.NoError(t, err)

	resp, err := client.ListRecords(ctx, &pb.ListRecordsRequest{Zone: "k8s.example.org"})
	require.NoError(t, err)
	assert.Empty(t, resp.Records)
}

func TestUpsertRecord_SignalsSent(t *testing.T) {
	store, _ := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	cache := &grpcserver.NetboxCache{}
	st := &grpcserver.StatusTracker{}
	mergeSignal := make(chan struct{}, 1)
	netboxSignal := make(chan struct{}, 1)

	lis := bufconn.Listen(bufSize)
	srv := grpcserver.New("", store, cache, st, mergeSignal, netboxSignal, nil)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, _ := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	defer func() { _ = conn.Close() }()

	client := pb.NewDynamicZoneServiceClient(conn)
	_, err := client.UpsertRecord(context.Background(), &pb.UpsertRecordRequest{
		Zone:   "k8s.example.org",
		Record: &pb.Record{DnsName: "node1.k8s.example.org", Address: "10.0.0.1", Family: 4},
	})
	require.NoError(t, err)

	select {
	case <-mergeSignal:
		// good
	default:
		t.Fatal("expected merge signal to be sent")
	}
}

// --- ControlService tests ---

func TestGetStatus(t *testing.T) {
	conn, _, st := newTestConn(t, "")
	now := time.Now()
	st.SetNetboxPoll(now)
	st.SetMergeWrite(now.Add(time.Second))
	st.SetStaleness(42.5)

	client := pb.NewControlServiceClient(conn)
	resp, err := client.GetStatus(context.Background(), &pb.GetStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, now.Unix(), resp.LastNetboxPollUnix)
	assert.Equal(t, now.Add(time.Second).Unix(), resp.LastMergeWriteUnix)
	assert.InDelta(t, 42.5, resp.ZoneStalenessSeconds, 0.01)
	assert.Equal(t, int32(0), resp.ActiveZones)
	assert.Equal(t, int32(0), resp.DynamicRecordCount)
}

func TestForceNetboxPoll_SendsSignal(t *testing.T) {
	store, _ := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	cache := &grpcserver.NetboxCache{}
	st := &grpcserver.StatusTracker{}
	mergeSignal := make(chan struct{}, 1)
	netboxSignal := make(chan struct{}, 1)

	lis := bufconn.Listen(bufSize)
	srv := grpcserver.New("", store, cache, st, mergeSignal, netboxSignal, nil)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, _ := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	defer func() { _ = conn.Close() }()

	client := pb.NewControlServiceClient(conn)
	_, err := client.ForceNetboxPoll(context.Background(), &pb.ForceNetboxPollRequest{})
	require.NoError(t, err)

	select {
	case <-netboxSignal:
	default:
		t.Fatal("expected netbox signal")
	}
}

func TestForceMergeWrite_SendsSignal(t *testing.T) {
	store, _ := dynamicstore.NewFileStore(filepath.Join(t.TempDir(), "dynamic.json"))
	cache := &grpcserver.NetboxCache{}
	st := &grpcserver.StatusTracker{}
	mergeSignal := make(chan struct{}, 1)
	netboxSignal := make(chan struct{}, 1)

	lis := bufconn.Listen(bufSize)
	srv := grpcserver.New("", store, cache, st, mergeSignal, netboxSignal, nil)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, _ := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	defer func() { _ = conn.Close() }()

	client := pb.NewControlServiceClient(conn)
	_, err := client.ForceMergeWrite(context.Background(), &pb.ForceMergeWriteRequest{})
	require.NoError(t, err)

	select {
	case <-mergeSignal:
	default:
		t.Fatal("expected merge signal")
	}
}
