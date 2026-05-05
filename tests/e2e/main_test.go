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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// TestMain forces a fresh Netbox poll before any test runs, so DNS tests
// always start with an up-to-date zone rather than state left by a prior run.
func TestMain(m *testing.M) {
	if err := refreshZone(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup warning: %v\n", err)
	}
	os.Exit(m.Run())
}

// refreshZone ensures the primary DNS server is serving a zone built from
// current Netbox data. It is a no-op when the zone is already correct.
//
// Strategy:
//  1. Fast path: if server1-mgmt already returns 10.1.0.1 we're done.
//  2. Call ForceNetboxPoll and poll the status endpoint until the poll
//     timestamp advances (confirming the fetch + zone write completed).
//  3. Wait up to 12s for CoreDNS to reload the zone and serve 10.1.0.1.
func refreshZone() error {
	probe := hostFQDN("server1-mgmt", "dc1")
	wantIP := net.ParseIP("10.1.0.1")

	checkDNS := func() bool {
		c := &dns.Client{Timeout: 5 * time.Second, Net: "tcp"}
		msg := new(dns.Msg)
		msg.SetQuestion(dns.Fqdn(probe), dns.TypeA)
		r, _, err := c.Exchange(msg, dnsServer())
		if err != nil || r.Rcode != dns.RcodeSuccess {
			return false
		}
		for _, ans := range r.Answer {
			if a, ok := ans.(*dns.A); ok && a.A.Equal(wantIP) {
				return true
			}
		}
		return false
	}

	// Fast path.
	if checkDNS() {
		return nil
	}

	conn, err := grpc.NewClient(grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc connect: %w", err)
	}
	defer conn.Close()

	var ctx context.Context
	if token := grpcToken(); token != "" {
		md := metadata.Pairs("authorization", "bearer "+token)
		ctx = metadata.NewOutgoingContext(context.Background(), md)
	} else {
		ctx = context.Background()
	}

	cc := pb.NewControlServiceClient(conn)

	// Capture baseline so we can detect when the poll actually completes.
	before, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil {
		return fmt.Errorf("GetStatus: %w", err)
	}

	if _, err := cc.ForceNetboxPoll(ctx, &pb.ForceNetboxPollRequest{}); err != nil {
		return fmt.Errorf("ForceNetboxPoll: %w", err)
	}

	// Wait for the poll to complete (LastNetboxPollUnix advances).
	// The 18k-record fetch with maxConcurrency=3 typically takes ~5s.
	pollDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(pollDeadline) {
		status, err := cc.GetStatus(ctx, &pb.GetStatusRequest{})
		if err == nil && status.LastNetboxPollUnix > before.LastNetboxPollUnix {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Now wait for CoreDNS to reload the zone (zone write + up to 10s auto reload + margin).
	dnsDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(dnsDeadline) {
		if checkDNS() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s → %s after ForceNetboxPoll", probe, wantIP)
}
