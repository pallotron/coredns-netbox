//go:build e2e

package e2e

import (
	"context"
	"fmt"
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

// coreDNSReloadWait is how long we sleep after a confirmed zone write before
// making DNS assertions. CoreDNS reloads zone files every 10s; sleeping 11s
// guarantees the reload has fired. Combined with cacheTTL=5 in dev values,
// this also ensures any previously cached answers (positive or negative) have
// expired before we query, making DNS checks deterministic.
const coreDNSReloadWait = 11 * time.Second

// forceMerge triggers a zone merge, waits for the zone file to be written,
// then sleeps for coreDNSReloadWait so CoreDNS has time to reload and any
// cached DNS answers have expired. Call it before any DNS assertion that
// depends on the current dynamic store state.
func forceMerge(t *testing.T, conn *grpc.ClientConn, ctx context.Context) {
	t.Helper()
	cc := pb.NewControlServiceClient(conn)

	before, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	require.NoError(t, err, "GetStatus before ForceMergeWrite")

	_, err = cc.ForceMergeWrite(ctx, &pb.ForceMergeWriteRequest{})
	require.NoError(t, err, "ForceMergeWrite")

	require.Eventually(t, func() bool {
		status, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
		return err == nil && status.LastMergeWriteUnix > before.LastMergeWriteUnix
	}, 15*time.Second, 100*time.Millisecond, "zone write not confirmed within 15s")

	time.Sleep(coreDNSReloadWait)
}

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

// waitForDNS polls until the DNS record resolves or times out. When called
// after forceMerge, CoreDNS has already reloaded and cached answers have
// expired, so a short timeout (3-5s) is sufficient.
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

// waitForPTR polls until the PTR query for addr returns wantFQDN or times out.
func waitForPTR(t *testing.T, addr, wantFQDN string, timeout time.Duration) {
	t.Helper()
	ptrName, err := dns.ReverseAddr(addr)
	require.NoError(t, err)
	want := dns.Fqdn(wantFQDN)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := queryServer(t, ptrName, dns.TypePTR, dnsServer())
		for _, ans := range r.Answer {
			if ptr, ok := ans.(*dns.PTR); ok && ptr.Ptr == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("PTR %s -> %s did not appear within %s", addr, wantFQDN, timeout)
}

// dynamicFQDN returns a test FQDN for a dynamic record, using the correct zone.
func dynamicFQDN(name string) string {
	return fmt.Sprintf("%s.%s", name, forwardZone("dc1"))
}

func TestDynamicZoneCreate(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	zoneName := fmt.Sprintf("e2e-test.%s", forwardZone("dc1"))
	_, err := zc.CreateZone(ctx, &pb.CreateZoneRequest{Name: zoneName})
	require.NoError(t, err)

	resp, err := zc.ListZones(ctx, &pb.ListZonesRequest{})
	require.NoError(t, err)
	assert.Contains(t, resp.Names, zoneName)

	t.Cleanup(func() {
		_, _ = zc.DeleteZone(authCtx(t), &pb.DeleteZoneRequest{Name: zoneName})
	})
}

func TestDynamicRecordUpsertAndResolve(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	zone := forwardZone("dc1")
	fqdn := dynamicFQDN("e2e-dynamic")
	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   zone,
		Record: &pb.Record{DnsName: fqdn, Address: "10.99.99.1", Family: 4},
	})
	require.NoError(t, err)
	forceMerge(t, conn, ctx)

	waitForDNS(t, fqdn, dns.TypeA, "10.99.99.1", 5*time.Second)

	t.Cleanup(func() {
		_, _ = zc.DeleteRecord(authCtx(t), &pb.DeleteRecordRequest{
			Zone: zone, DnsName: fqdn,
		})
	})
}

func TestDynamicRecordDelete(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	zone := forwardZone("dc1")
	fqdn := dynamicFQDN("e2e-todelete")
	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   zone,
		Record: &pb.Record{DnsName: fqdn, Address: "10.99.99.2", Family: 4},
	})
	require.NoError(t, err)
	forceMerge(t, conn, ctx)
	waitForDNS(t, fqdn, dns.TypeA, "10.99.99.2", 5*time.Second)

	_, err = zc.DeleteRecord(ctx, &pb.DeleteRecordRequest{
		Zone: zone, DnsName: fqdn,
	})
	require.NoError(t, err)
	forceMerge(t, conn, ctx)

	require.Eventually(t, func() bool {
		r := queryServer(t, fqdn, dns.TypeA, dnsServer())
		return r.Rcode == dns.RcodeNameError || len(r.Answer) == 0
	}, 5*time.Second, 200*time.Millisecond, "expected NXDOMAIN after delete")
}

func TestBatchUpsert(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	zone := forwardZone("dc1")
	records := []*pb.Record{
		{DnsName: dynamicFQDN("e2e-batch1"), Address: "10.99.98.1", Family: 4},
		{DnsName: dynamicFQDN("e2e-batch2"), Address: "10.99.98.2", Family: 4},
		{DnsName: dynamicFQDN("e2e-batch3"), Address: "10.99.98.3", Family: 4},
	}
	_, err := zc.BatchUpsert(ctx, &pb.BatchUpsertRequest{
		ZoneRecords: []*pb.ZoneRecords{{Zone: zone, Records: records}},
	})
	require.NoError(t, err)
	forceMerge(t, conn, ctx)

	for _, r := range records {
		waitForDNS(t, r.DnsName, dns.TypeA, r.Address, 5*time.Second)
	}

	t.Cleanup(func() {
		names := make([]string, len(records))
		for i, r := range records {
			names[i] = r.DnsName
		}
		_, _ = zc.BatchDelete(authCtx(t), &pb.BatchDeleteRequest{
			Zone: zone, DnsNames: names,
		})
	})
}

func TestDynamicRecordSurvivesNetboxPoll(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)
	cc := pb.NewControlServiceClient(conn)

	zone := forwardZone("dc1")
	fqdn := dynamicFQDN("e2e-survives")
	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   zone,
		Record: &pb.Record{DnsName: fqdn, Address: "10.99.97.1", Family: 4},
	})
	require.NoError(t, err)
	forceMerge(t, conn, ctx)
	waitForDNS(t, fqdn, dns.TypeA, "10.99.97.1", 5*time.Second)

	// ForceNetboxPoll rewrites the zone merging Netbox + dynamic records.
	// The dynamic record must survive (remain in DNS after the poll).
	// No forceMerge here — the poll drives its own zone write. Allow up to
	// 5s (Netbox fetch) + 10s (CoreDNS reload) + 5s (cache expiry) + buffer.
	_, err = cc.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{})
	require.NoError(t, err)
	waitForDNS(t, fqdn, dns.TypeA, "10.99.97.1", 25*time.Second)

	t.Cleanup(func() {
		_, _ = zc.DeleteRecord(authCtx(t), &pb.DeleteRecordRequest{
			Zone: zone, DnsName: fqdn,
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

	require.Eventually(t, func() bool {
		after, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
		return err == nil && after.LastNetboxPollUnix > before.LastNetboxPollUnix
	}, 30*time.Second, 500*time.Millisecond, "LastNetboxPollUnix not updated after force poll")
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

	require.Eventually(t, func() bool {
		after, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
		return err == nil && after.LastMergeWriteUnix > before.LastMergeWriteUnix
	}, 10*time.Second, 200*time.Millisecond, "LastMergeWriteUnix not updated after force merge")
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

func TestZoneReloadService(t *testing.T) {
	// Verify that Reload() RPC is reachable and returns no error.
	conn := grpcConn(t)
	ctx := authCtx(t)

	// Force sidecar to rewrite zones via a Netbox poll.
	ctrl := pb.NewControlServiceClient(conn)
	_, err := ctrl.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{})
	require.NoError(t, err)

	// Wait for zone write to complete (staleness < 5s).
	require.Eventually(t, func() bool {
		s, err := ctrl.GetStatus(authCtx(t), &pb.GetStatusRequest{})
		return err == nil && s.ZoneStalenessSeconds < 5
	}, 30*time.Second, 500*time.Millisecond, "zone write did not complete")

	// Call Reload directly on CoreDNS.
	reloadAddr := os.Getenv("COREDNS_RELOAD_ADDR")
	if reloadAddr == "" {
		t.Skip("COREDNS_RELOAD_ADDR not set")
	}
	rconn, err := grpc.NewClient(reloadAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer rconn.Close()

	_, err = pb.NewZoneReloadServiceClient(rconn).Reload(authCtx(t), &pb.ZoneReloadRequest{})
	require.NoError(t, err)
}

func TestDynamicRecordGetsPTR(t *testing.T) {
	conn := grpcConn(t)
	ctx := authCtx(t)
	zc := pb.NewDynamicZoneServiceClient(conn)

	zone := forwardZone("dc1")
	const ip = "10.99.96.1"
	fqdn := dynamicFQDN("e2e-ptr")

	_, err := zc.UpsertRecord(ctx, &pb.UpsertRecordRequest{
		Zone:   zone,
		Record: &pb.Record{DnsName: fqdn, Address: ip, Family: 4},
	})
	require.NoError(t, err)
	forceMerge(t, conn, ctx)

	// Forward lookup must resolve first.
	waitForDNS(t, fqdn, dns.TypeA, ip, 5*time.Second)
	// Reverse lookup must return the FQDN.
	waitForPTR(t, ip, fqdn, 5*time.Second)

	t.Cleanup(func() {
		_, _ = zc.DeleteRecord(authCtx(t), &pb.DeleteRecordRequest{
			Zone: zone, DnsName: fqdn,
		})
	})
}
