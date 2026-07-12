package netboxwebhook

import (
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/metrics"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/pallotron/coredns-netbox/internal/zonediscovery"
)

// Path is the HTTP route the Netbox webhook posts to.
const Path = "/webhook/netbox"

// maxBodyBytes caps the request body Netbox can send us. A single
// ipam.ipaddress webhook payload is a few KB; this leaves generous headroom
// while bounding worst-case memory use per request.
const maxBodyBytes = 64 * 1024

const objectTypeIPAddress = "ipam.ipaddress"

// Register adds the Netbox webhook route to mux. If secret is empty, no
// route is registered at all — there is never an accidental unauthenticated
// trigger endpoint.
//
// pollSignal is signaled (non-blocking, buffered size 1 by convention, same
// as netboxSignal/ForceNetboxPoll) whenever an event can't be applied
// directly because it has no dns_name — see applyOutcome and the doc
// comment on triggerPoll.
func Register(mux *http.ServeMux, secret string, store dynamicstore.DynamicStore, disc zonediscovery.Discoverer, mergeSignal, pollSignal chan<- struct{}, m *metrics.Sidecar) {
	if secret == "" {
		return
	}
	h := &handler{secret: secret, store: store, disc: disc, mergeSignal: mergeSignal, pollSignal: pollSignal, m: m}
	mux.HandleFunc(Path, h.ServeHTTP)
}

type handler struct {
	secret      string
	store       dynamicstore.DynamicStore
	disc        zonediscovery.Discoverer
	mergeSignal chan<- struct{}
	pollSignal  chan<- struct{}
	m           *metrics.Sidecar

	// mu serializes the read-decide-write sequence in apply (and everything
	// it calls: applyDelete, applyUpsert, upsertGuarded, isManuallyPinned).
	// Those guards each do a GetRecords read followed by a separate
	// Upsert/DeleteRecords write, with no atomicity between the two calls —
	// dynamicstore.FileStore's own internal lock only protects each
	// individual call, not the sequence. net/http runs every request in its
	// own goroutine, and Netbox's webhook dispatcher can deliver events for
	// the same DNS name concurrently, so without this mutex two concurrent
	// deliveries could both read the same "before" state, both pass their
	// guard check, and then race on the write — silently defeating both the
	// AppliedAt ordering guarantee and manual-record precedence. Locking
	// around h.apply(p) alone (not signature verification or payload
	// parsing, which don't touch the store) closes that race for all
	// webhook-driven store mutations from this handler. This intentionally
	// does NOT serialize against gRPC-driven writes to the same store (a
	// separate write path) — an accepted, narrower scope.
	mu sync.Mutex
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.m.WebhookRequestsTotal.WithLabelValues("bad_payload").Inc()
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}

	if !verifySignature(h.secret, body, r.Header.Get("X-Hook-Signature")) {
		h.m.WebhookRequestsTotal.WithLabelValues("invalid_signature").Inc()
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	p, err := parsePayload(body)
	if err != nil {
		h.m.WebhookRequestsTotal.WithLabelValues("bad_payload").Inc()
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	if p.ObjectType != objectTypeIPAddress {
		h.m.WebhookRequestsTotal.WithLabelValues("unsupported_model").Inc()
		slog.Debug("netboxwebhook: ignoring unsupported object type", "object_type", p.ObjectType)
		w.WriteHeader(http.StatusOK)
		return
	}

	h.mu.Lock()
	outcome, err := h.apply(p)
	h.mu.Unlock()
	if err != nil {
		h.m.WebhookRequestsTotal.WithLabelValues("error").Inc()
		slog.Warn("netboxwebhook: failed to apply event", "event", p.Event, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch outcome {
	case outcomeApplied:
		h.m.WebhookRequestsTotal.WithLabelValues("ok").Inc()
		h.m.WebhookLastEventTimestamp.SetToCurrentTime()
		select {
		case h.mergeSignal <- struct{}{}:
		default:
		}
	case outcomePollTriggered:
		h.m.WebhookRequestsTotal.WithLabelValues("poll_triggered").Inc()
	default:
		h.m.WebhookRequestsTotal.WithLabelValues("stale").Inc()
	}
	w.WriteHeader(http.StatusOK)
}

// applyOutcome distinguishes the three ways handling an event can end.
type applyOutcome int

const (
	// outcomeNoop: nothing changed — stale/out-of-order event, pinned by a
	// manual record, unroutable dns_name, or an unrecognized event type.
	outcomeNoop applyOutcome = iota
	// outcomeApplied: a real dynamicstore write happened; mergeSignal must fire.
	outcomeApplied
	// outcomePollTriggered: the event carries no dns_name, so a full Netbox
	// poll was signaled instead of a direct write — see triggerPoll.
	outcomePollTriggered
)

// apply routes the event to the right store mutation or poll trigger.
func (h *handler) apply(p payload) (applyOutcome, error) {
	switch p.Event {
	case "deleted":
		return h.applyDelete(p)
	case "created", "updated":
		return h.applyUpsert(p)
	default:
		slog.Debug("netboxwebhook: ignoring unknown event type", "event", p.Event)
		return outcomeNoop, nil
	}
}

// triggerPoll signals a full Netbox poll for an event whose record has no
// dns_name. Device-based DNS name generation (ipcategorizer/nameformat)
// needs cross-record context a single webhook event can't supply:
// ipcategorizer.SelectDeviceIPs picks the preferred IP for a device by
// comparing ALL of that device's interfaces, and nameformat's CNAME alias
// generation needs the full zone's record set to resolve collisions between
// devices. Netbox's webhook payload (default or a custom body_template) is
// always the plain REST-serialized object — never a live, queryable object
// graph — so there is no way to obtain that context from the webhook alone,
// regardless of which object type the webhook subscribes to; closing this
// gap requires an actual poll.
//
// pollSignal is buffered size 1 with a non-blocking send, so a burst of
// these events (e.g. many devices added during a region turnup) coalesces
// into a single pending poll. cmd/sidecar's run() loop additionally
// rate-limits how often it acts on this channel (WEBHOOK_POLL_MIN_INTERVAL)
// so even a very large, sustained burst can't force full re-fetches faster
// than that floor — see docs/netbox-webhook.md.
func (h *handler) triggerPoll() {
	select {
	case h.pollSignal <- struct{}{}:
	default:
	}
}

func (h *handler) applyDelete(p payload) (applyOutcome, error) {
	if p.Snapshots.PreChange == nil {
		return outcomeNoop, nil
	}
	name := p.Snapshots.PreChange.DNSName
	if name == "" {
		h.triggerPoll()
		return outcomePollTriggered, nil
	}
	zone, ok := h.zoneFor(name)
	if !ok {
		return outcomeNoop, nil
	}

	// Manual-record precedence: don't let a webhook "deleted" event remove a
	// manually-added (non-webhook-sourced) record for the same name — mirrors
	// upsertGuarded's guard against clobbering manually-pinned entries.
	if h.isManuallyPinned(zone, name) {
		slog.Warn("netboxwebhook: skipping delete, name is pinned by a manually-added record", "dns_name", name)
		return outcomeNoop, nil
	}

	if err := h.store.DeleteRecords(zone, []string{name}); err != nil {
		return outcomeNoop, err
	}
	return outcomeApplied, nil
}

// isManuallyPinned reports whether zone already holds a record for name that
// was not sourced from the webhook (i.e. added via the gRPC
// DynamicZoneService API). Such records must never be silently removed or
// clobbered by webhook-driven writes — manual records only ever exist in
// dynamicstore, so nothing else (not the periodic poll, not
// ReconcileWebhookSourced) would ever restore them.
func (h *handler) isManuallyPinned(zone, name string) bool {
	for _, existing := range h.store.GetRecords(zone) {
		if existing.DNSName != name {
			continue
		}
		return existing.Source != netboxclient.SourceWebhook
	}
	return false
}

func (h *handler) applyUpsert(p payload) (applyOutcome, error) {
	rec, err := recordFromPayload(p)
	if err != nil {
		return outcomeNoop, err
	}
	if rec.DNSName == "" {
		h.triggerPoll()
		return outcomePollTriggered, nil
	}

	// mutated tracks whether any store write has happened so far in this
	// call. mergeSignal must fire whenever a real write happened — including
	// a rename's old-name delete succeeding even if the new name's zone can't
	// be resolved below — so the final return must account for it, not just
	// the outcome of the final upsert-or-skip step.
	mutated := false

	// Rename: the old name may live in a different zone bucket.
	if p.Event == "updated" && p.Snapshots.PreChange != nil &&
		p.Snapshots.PreChange.DNSName != "" && p.Snapshots.PreChange.DNSName != rec.DNSName {
		oldName := p.Snapshots.PreChange.DNSName
		if oldZone, ok := h.zoneFor(oldName); ok {
			// Manual-record precedence: don't let a rename's old-name delete
			// remove a manually-added (non-webhook-sourced) record that
			// happens to occupy the old name — mirrors applyDelete's and
			// upsertGuarded's guard against clobbering manually-pinned
			// entries.
			if h.isManuallyPinned(oldZone, oldName) {
				slog.Warn("netboxwebhook: skipping rename delete, old name is pinned by a manually-added record", "dns_name", oldName)
			} else {
				if err := h.store.DeleteRecords(oldZone, []string{oldName}); err != nil {
					return outcomeNoop, err
				}
				mutated = true
			}
		}
	}

	zone, ok := h.zoneFor(rec.DNSName)
	if !ok {
		slog.Warn("netboxwebhook: dns_name does not map to any configured zone", "dns_name", rec.DNSName)
		if mutated {
			return outcomeApplied, nil
		}
		return outcomeNoop, nil
	}

	applied, err := h.upsertGuarded(zone, rec)
	if err != nil {
		if mutated {
			return outcomeApplied, err
		}
		return outcomeNoop, err
	}
	if mutated || applied {
		return outcomeApplied, nil
	}
	return outcomeNoop, nil
}

// upsertGuarded applies rec unless a newer-or-equal webhook-sourced write
// already exists for the same name, or the name is pinned by a manually-added
// (non-webhook) record — in both cases the write is skipped rather than
// clobbering a more authoritative entry.
func (h *handler) upsertGuarded(zone string, rec netboxclient.IPRecord) (bool, error) {
	for _, existing := range h.store.GetRecords(zone) {
		if existing.DNSName != rec.DNSName {
			continue
		}
		if existing.Source != netboxclient.SourceWebhook {
			slog.Warn("netboxwebhook: skipping write, name is pinned by a manually-added record", "dns_name", rec.DNSName)
			return false, nil
		}
		if !rec.AppliedAt.After(existing.AppliedAt) {
			slog.Debug("netboxwebhook: skipping stale/out-of-order event", "dns_name", rec.DNSName)
			return false, nil
		}
		break
	}
	if err := h.store.UpsertRecords(zone, []netboxclient.IPRecord{rec}); err != nil {
		return false, err
	}
	return true, nil
}

// zoneFor resolves the configured zone a DNS name belongs to, reusing the
// same Discoverer the full Netbox fetch uses.
func (h *handler) zoneFor(dnsName string) (string, bool) {
	zones, err := h.disc.Discover([]netboxclient.IPRecord{{DNSName: dnsName}})
	if err != nil {
		slog.Warn("netboxwebhook: zone discovery failed", "dns_name", dnsName, "err", err)
		return "", false
	}
	for zone := range zones {
		return zone, true
	}
	return "", false
}
