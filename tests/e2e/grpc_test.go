//go:build e2e

package e2e

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

func grpcAddr() string {
	if v := os.Getenv("GRPC_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:18083"
}

func grpcToken() string {
	return os.Getenv("GRPC_AUTH_TOKEN")
}

func grpcConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func authCtx(t *testing.T) context.Context {
	t.Helper()
	token := grpcToken()
	if token == "" {
		return context.Background()
	}
	md := metadata.Pairs("authorization", "bearer "+token)
	return metadata.NewOutgoingContext(context.Background(), md)
}

// waitForDNS polls until the DNS record resolves or times out.
func waitForDNS(t *testing.T, name string, qtype uint16, wantIP string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := queryServer(t, name, qtype, dnsServer())
		for _, ans := range r.Answer {
			if a, ok := ans.(*dns.A); ok && a.A.Equal(net.ParseIP(wantIP)) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("DNS record %s -> %s did not appear within %s", name, wantIP, timeout)
}

func TestDynamicZoneCreate(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	_, err := zc.CreateZone(ctx, &pb.CreateZoneRequest{Name: "e2e-test.mycompany.com"})
	require.NoError(t, err)

	resp, err := zc.ListZones(ctx, &pb.ListZonesRequest{})
	require.NoError(t, err)
	assert.Contains(t, resp.Names, "e2e-test.mycompany.com")

	t.Cleanup(func() {
		_, _ = zc.DeleteZone(authCtx(t), &pb.DeleteZoneRequest{Name: "e2e-test.mycompany.com"})
	})
}

func TestDynamicRecordUpsertAndResolve(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   "dc1.mycompany.com",
		Record: &pb.Record{DnsName: "e2e-dynamic.dc1.mycompany.com", Address: "10.99.99.1", Family: 4},
	})
	require.NoError(t, err)

	waitForDNS(t, "e2e-dynamic.dc1.mycompany.com", dns.TypeA, "10.99.99.1", 10*time.Second)

	t.Cleanup(func() {
		_, _ = zc.DeleteRecord(authCtx(t), &pb.DeleteRecordRequest{
			Zone: "dc1.mycompany.com", DnsName: "e2e-dynamic.dc1.mycompany.com",
		})
	})
}

func TestDynamicRecordDelete(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   "dc1.mycompany.com",
		Record: &pb.Record{DnsName: "e2e-todelete.dc1.mycompany.com", Address: "10.99.99.2", Family: 4},
	})
	require.NoError(t, err)
	waitForDNS(t, "e2e-todelete.dc1.mycompany.com", dns.TypeA, "10.99.99.2", 10*time.Second)

	_, err = zc.DeleteRecord(ctx, &pb.DeleteRecordRequest{
		Zone: "dc1.mycompany.com", DnsName: "e2e-todelete.dc1.mycompany.com",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		r := queryServer(t, "e2e-todelete.dc1.mycompany.com", dns.TypeA, dnsServer())
		return r.Rcode == dns.RcodeNameError || len(r.Answer) == 0
	}, 10*time.Second, 200*time.Millisecond, "expected NXDOMAIN after delete")
}

func TestBatchUpsert(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	records := []*pb.Record{
		{DnsName: "e2e-batch1.dc1.mycompany.com", Address: "10.99.98.1", Family: 4},
		{DnsName: "e2e-batch2.dc1.mycompany.com", Address: "10.99.98.2", Family: 4},
		{DnsName: "e2e-batch3.dc1.mycompany.com", Address: "10.99.98.3", Family: 4},
	}
	_, err := zc.BatchUpsert(ctx, &pb.BatchUpsertRequest{
		ZoneRecords: []*pb.ZoneRecords{{Zone: "dc1.mycompany.com", Records: records}},
	})
	require.NoError(t, err)

	for _, r := range records {
		waitForDNS(t, r.DnsName, dns.TypeA, r.Address, 10*time.Second)
	}

	t.Cleanup(func() {
		names := make([]string, len(records))
		for i, r := range records {
			names[i] = r.DnsName
		}
		_, _ = zc.BatchDelete(authCtx(t), &pb.BatchDeleteRequest{
			Zone: "dc1.mycompany.com", DnsNames: names,
		})
	})
}

func TestDynamicRecordSurvivesNetboxPoll(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)
	cc := pb.NewControlServiceClient(conn)

	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   "dc1.mycompany.com",
		Record: &pb.Record{DnsName: "e2e-survives.dc1.mycompany.com", Address: "10.99.97.1", Family: 4},
	})
	require.NoError(t, err)
	waitForDNS(t, "e2e-survives.dc1.mycompany.com", dns.TypeA, "10.99.97.1", 10*time.Second)

	_, err = cc.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{})
	require.NoError(t, err)
	time.Sleep(2 * time.Second)

	waitForDNS(t, "e2e-survives.dc1.mycompany.com", dns.TypeA, "10.99.97.1", 5*time.Second)

	t.Cleanup(func() {
		_, _ = zc.DeleteRecord(authCtx(t), &pb.DeleteRecordRequest{
			Zone: "dc1.mycompany.com", DnsName: "e2e-survives.dc1.mycompany.com",
		})
	})
}

func TestForceNetboxPollUpdatesStatus(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	cc := pb.NewControlServiceClient(conn)

	before, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	require.NoError(t, err)

	time.Sleep(1 * time.Second)
	_, err = cc.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{})
	require.NoError(t, err)
	time.Sleep(2 * time.Second)

	after, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	require.NoError(t, err)
	assert.Greater(t, after.LastNetboxPollUnix, before.LastNetboxPollUnix)
}

func TestForceMergeWriteUpdatesStatus(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	cc := pb.NewControlServiceClient(conn)

	before, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	require.NoError(t, err)

	time.Sleep(1 * time.Second)
	_, err = cc.ForceMergeWrite(ctx, &pb.ForceMergeWriteRequest{})
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	after, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	require.NoError(t, err)
	assert.Greater(t, after.LastMergeWriteUnix, before.LastMergeWriteUnix)
}

func TestAuthRejectsWrongToken(t *testing.T) {
	if grpcToken() == "" {
		t.Skip("auth disabled (GRPC_AUTH_TOKEN not set)")
	}
	conn := grpcConn(t)
	md := metadata.Pairs("authorization", "bearer wrong-token")
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	zc := pb.NewDynamicZoneServiceClient(conn)
	_, err := zc.ListZones(ctx, &pb.ListZonesRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, grpcstatus.Code(err))
}

func TestAuthRejectsMissingToken(t *testing.T) {
	if grpcToken() == "" {
		t.Skip("auth disabled (GRPC_AUTH_TOKEN not set)")
	}
	conn := grpcConn(t)
	zc := pb.NewDynamicZoneServiceClient(conn)
	_, err := zc.ListZones(context.Background(), &pb.ListZonesRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, grpcstatus.Code(err))
}
