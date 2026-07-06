package reloader

import (
	"context"
	"log/slog"
	"sync"
	"time"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Reloader calls ZoneReloadService.Reload() on each configured CoreDNS pod.
type Reloader struct {
	addrs []string
	token string

	// RetryDelays are the waits between attempts for one address; a push is
	// tried len(RetryDelays)+1 times before giving up. The first push after
	// a rollout often races CoreDNS startup, and a dropped push defers zone
	// load to CoreDNS's fallback poll interval.
	RetryDelays []time.Duration

	// ResultCounter counts the final outcome per address (label: result =
	// success|error, after retries). RetryCounter counts individual retry
	// attempts. DirtyGauge tracks how many addresses still await a successful
	// push. All are optional; nil disables the instrumentation.
	ResultCounter *prometheus.CounterVec
	RetryCounter  prometheus.Counter
	DirtyGauge    prometheus.Gauge

	// dirty holds addresses whose last push failed (or was never attempted
	// since the last zone change). Reconcile re-pushes exactly this set, so a
	// push lost to a rollout race converges on a later poll cycle instead of
	// being dropped until the next zone change.
	mu    sync.Mutex
	dirty map[string]bool
}

// New creates a Reloader that will call Reload() on all addrs (host:port).
// token is the bearer token for gRPC auth; empty means no auth header sent.
func New(addrs []string, token string) *Reloader {
	return &Reloader{
		addrs:       addrs,
		token:       token,
		RetryDelays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		dirty:       make(map[string]bool),
	}
}

// Reload marks every configured address dirty and fans out Reload() RPCs to
// all of them in parallel. Errors are logged but do not propagate — zone
// files are on disk and CoreDNS will pick them up on its fallback poll
// interval regardless; failed addresses stay dirty for Reconcile.
func (r *Reloader) Reload(ctx context.Context) {
	r.mu.Lock()
	for _, addr := range r.addrs {
		r.dirty[addr] = true
	}
	r.mu.Unlock()
	r.pushDirty(ctx)
}

// Reconcile re-pushes only to addresses whose last push never succeeded.
// Call it on every poll cycle: it is a no-op when all addresses are clean,
// and it converges pushes that Reload lost to a rollout race once the target
// pod becomes reachable.
func (r *Reloader) Reconcile(ctx context.Context) {
	r.pushDirty(ctx)
}

func (r *Reloader) pushDirty(ctx context.Context) {
	r.mu.Lock()
	addrs := make([]string, 0, len(r.dirty))
	for addr := range r.dirty {
		addrs = append(addrs, addr)
	}
	r.mu.Unlock()
	if len(addrs) == 0 {
		r.setDirtyGauge()
		return
	}

	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if err := r.reloadOne(ctx, addr); err != nil {
				slog.Warn("coredns reload failed", "addr", addr, "err", err)
				r.countResult("error")
			} else {
				r.mu.Lock()
				delete(r.dirty, addr)
				r.mu.Unlock()
				r.countResult("success")
			}
		}(addr)
	}
	wg.Wait()
	r.setDirtyGauge()
}

func (r *Reloader) setDirtyGauge() {
	if r.DirtyGauge == nil {
		return
	}
	r.mu.Lock()
	n := len(r.dirty)
	r.mu.Unlock()
	r.DirtyGauge.Set(float64(n))
}

func (r *Reloader) reloadOne(ctx context.Context, addr string) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = r.pushOne(ctx, addr); err == nil {
			return nil
		}
		if attempt >= len(r.RetryDelays) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(r.RetryDelays[attempt]):
		}
		if r.RetryCounter != nil {
			r.RetryCounter.Inc()
		}
	}
}

func (r *Reloader) countResult(result string) {
	if r.ResultCounter != nil {
		r.ResultCounter.WithLabelValues(result).Inc()
	}
}

func (r *Reloader) pushOne(ctx context.Context, addr string) error {
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
