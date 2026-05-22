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
	defer func() { _ = conn.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if r.token != "" {
		md := metadata.New(map[string]string{"authorization": "bearer " + r.token})
		callCtx = metadata.NewOutgoingContext(callCtx, md)
	}

	_, err = pb.NewZoneReloadServiceClient(conn).Reload(callCtx, &pb.ZoneReloadRequest{})
	return err
}
