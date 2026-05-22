package netboxreload

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/miekg/dns"
	"google.golang.org/grpc"
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

	mu         sync.RWMutex
	zones      map[string]*zone // origin (e.g. "mycompany.com.") -> loaded zone
	grpcServer *grpc.Server     // stored for graceful shutdown via stopGRPC
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
	recs, nameExists := z.records[name]
	for _, rr := range recs {
		if rr.Header().Rrtype == q.Qtype || q.Qtype == dns.TypeANY {
			m.Answer = append(m.Answer, dns.Copy(rr))
		}
	}
	// collect SOA while still holding the lock
	var soaRR dns.RR
	for _, rr := range z.records[z.origin] {
		if rr.Header().Rrtype == dns.TypeSOA {
			soaRR = dns.Copy(rr)
			break
		}
	}
	p.mu.RUnlock()

	if !nameExists {
		m.Rcode = dns.RcodeNameError
	}
	if len(m.Answer) == 0 && soaRR != nil {
		// Both NXDOMAIN and NODATA responses include SOA in authority per RFC 2308
		m.Ns = append(m.Ns, soaRR)
	}

	if err := w.WriteMsg(m); err != nil {
		return dns.RcodeServerFailure, err
	}
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
// File I/O happens before the lock is taken; the lock only guards the swap.
// loadZoneDir is fail-fast: if any zone file fails to parse, the in-memory
// map is left unchanged (safe partial-write behaviour).
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

// Transfer implements the transfer.Transferer interface so the CoreDNS
// transfer plugin can serve AXFR/IXFR requests to secondary DNS servers.
// serial==0 means AXFR (full transfer); serial>0 is IXFR (incremental —
// we fall back to AXFR unless the client is already up-to-date).
func (p *Plugin) Transfer(zone string, serial uint32) (<-chan []dns.RR, error) {
	p.mu.RLock()
	z, ok := p.zones[dns.Fqdn(zone)]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("not authoritative for zone %s", zone)
	}

	ch := make(chan []dns.RR)
	go func() {
		defer close(ch)
		p.mu.RLock()
		defer p.mu.RUnlock()

		// Locate the SOA at the zone apex.
		var soa dns.RR
		for _, rr := range z.records[z.origin] {
			if rr.Header().Rrtype == dns.TypeSOA {
				soa = dns.Copy(rr)
				break
			}
		}
		if soa == nil {
			return // zone has no SOA; nothing to transfer
		}

		// IXFR: if the client already has the current serial, send just SOA.
		if serial != 0 && soa.(*dns.SOA).Serial == serial {
			ch <- []dns.RR{soa}
			return
		}

		// AXFR: SOA → all non-SOA records → SOA.
		ch <- []dns.RR{soa}
		for _, rrset := range z.records {
			var batch []dns.RR
			for _, rr := range rrset {
				if rr.Header().Rrtype == dns.TypeSOA {
					continue // SOA is sent separately at start and end
				}
				batch = append(batch, dns.Copy(rr))
			}
			if len(batch) > 0 {
				ch <- batch
			}
		}
		ch <- []dns.RR{soa}
	}()
	return ch, nil
}
