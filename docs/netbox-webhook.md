# Netbox Webhook Trigger

The sidecar can apply Netbox `ipam.ipaddress` changes immediately, via a webhook Netbox posts directly to the sidecar — no message broker. There is no need to filter which IP addresses the webhook covers: every `ipam.ipaddress` create/update/delete event is handled, whether or not the record has `dns_name` set in Netbox.

## How it works

```
Netbox (ipam.ipaddress webhook, HMAC-signed)
    → POST /webhook/netbox on the sidecar's HEALTH_ADDR port
    → verify X-Hook-Signature (HMAC-SHA512)
    → parse event (created/updated/deleted)

    dns_name set in Netbox?
      yes → write directly into the dynamic record store
            → trigger the same debounced re-merge + zone write + CoreDNS
              reload path the gRPC DynamicZoneService API already uses
              (no Netbox API call)

      no  → trigger a full Netbox poll instead (see "Records without
            dns_name" below) — reuses the periodic poll's existing
            device-name-generation logic unchanged, rate-limited by
            WEBHOOK_POLL_MIN_INTERVAL
```

The periodic poll (`POLL_INTERVAL`) keeps running unchanged as a reconciliation backstop regardless of which path an event takes — it is not required for correctness, only for recovering from a missed webhook delivery.

## Enabling it

Set `NETBOX_WEBHOOK_SECRET` to a shared secret and configure a matching Netbox webhook + event rule:

- Webhook `payload_url`: `http://<sidecar-service>:8082/webhook/netbox`
- Webhook `secret`: same value as `NETBOX_WEBHOOK_SECRET`
- Event rule `object_types`: `ipam.ipaddress`
- Event rule `event_types`: `object_created`, `object_updated`, `object_deleted`

No condition/filter on `dns_name` is needed on the event rule — subscribe to all `ipam.ipaddress` events. If `NETBOX_WEBHOOK_SECRET` is unset, the `/webhook/netbox` route is not registered at all — there is no way to accidentally expose an unauthenticated trigger endpoint.

## Records without `dns_name`

Many environments generate DNS names from device names (`ipcategorizer`/`nameformat`) rather than setting `dns_name` explicitly in Netbox — in some deployments this is the overwhelming majority of records, not an edge case. A single webhook event can't compute that name itself:

- `ipcategorizer.SelectDeviceIPs` picks the *one* preferred IP for a device by comparing **all** of that device's interfaces (mgmt VRF vs. BMC vs. dataplane, etc.) — a webhook event for one IP address doesn't carry the device's other IPs.
- `nameformat`'s CNAME alias generation needs the **full zone's** record set to resolve collisions between different devices.

**Why the webhook payload can't supply this itself, no matter how it's configured:** the `data` field in a Netbox webhook (the default JSON body, or a custom Jinja2 `body_template`) is always the plain REST-API-serialized representation of the changed object — never a live, queryable Django object. Jinja2 templates can't perform relational lookups (`data.assigned_object.device.interfaces.all` is not available); you only ever get fields the object's own serializer already includes. For an `ipam.ipaddress` event that's the one assigned interface/device name (already parsed for the direct-write path), never the device's *other* interfaces or IPs. Subscribing to `dcim.device` events instead wouldn't help either — NetBox's device serializer exposes `primary_ip4`/`primary_ip6` (a single reference each) plus metadata, not a nested interface list; that's always a separate `/api/dcim/interfaces/?device_id=X` call regardless of webhook configuration. Closing this gap requires an actual poll no matter how the webhook is set up.

So instead of a no-op, an event with no `dns_name` (a `created`/`updated` event whose `data.dns_name` is empty, or a `deleted` event whose `snapshots.prechange.dns_name` was empty) triggers a full Netbox poll — the same code path `ForceNetboxPoll` already uses, unchanged. This means these records get the same near-real-time propagation as `dns_name`-having ones, just via a slightly heavier (but already correct and tested) mechanism, rather than waiting for the next `POLL_INTERVAL` tick.

**Bulk changes (e.g. a region turnup adding many devices at once) are self-throttling by design, not by chance:**

- The trigger channel is buffered size 1 with a non-blocking send — a burst of hundreds of these events collapses into whatever single poll is currently pending, the same debounce pattern used everywhere else in this feature.
- `WEBHOOK_POLL_MIN_INTERVAL` (default `5s`) additionally bounds how often a webhook-triggered poll can run at all, independent of how many events arrive — a hard ceiling on Netbox API request rate during a burst, on top of the channel's own coalescing. A signal that arrives inside the cooldown window is simply dropped; the periodic poll (or the next webhook event once the cooldown has elapsed) is always there as a backstop.
- The worst case under a sustained burst is back-to-back full polls for as long as the burst lasts, bounded by `burst_duration / poll_cycle_duration` — this degrades toward the *pre-webhook baseline* (continuous polling), not into something new or worse. The webhook path only removes idle-time API calls; it never adds a failure mode under load that didn't already exist.
- This is deliberately a **separate signal channel** from the one `ForceNetboxPoll` (the gRPC control call) uses, so a deliberate operator-triggered force-poll always runs immediately and is never subject to `WEBHOOK_POLL_MIN_INTERVAL`'s cooldown.

## Limitations

- **Concurrent/out-of-order delivery is handled, not prevented.** Netbox dispatches webhooks via a background worker queue, so a burst of changes (e.g. one bulk edit) can arrive out of order. Each webhook-sourced record carries the originating event's timestamp; an older event can never overwrite an already-applied newer one for the same name.
- **A webhook-sourced record never permanently shadows Netbox.** It's automatically cleared once the next full poll proves it's already reflected there — so a dropped webhook delivery self-heals on the next poll cycle rather than leaving stale data indefinitely.
- **A manually-added record (via the `DynamicZoneService` gRPC API) always wins over a webhook-sourced one for the same name.** The webhook write is skipped and logged at `WARN` — this applies to both webhook creates/updates and webhook deletes.
- **A deleted record can be resurrected by a delayed, out-of-order "created"/"updated" event for the same name.** Unlike the upsert path, `applyDelete` has no timestamp-based ordering guard — Netbox delivers webhooks via a background worker queue, so a delayed create/update arriving after a delete for the same DNS name will recreate it. This is a known, accepted gap (not yet mitigated by a tombstone mechanism); the periodic poll's reconciliation bounds how long a resurrected record can persist before being corrected.
- In `netbox-dns` discovery mode, zone routing for a `dns_name`-having webhook event makes its own Netbox API call per event (same `Discoverer.Discover` used by the full fetch) — this does not benefit from the same API-call reduction as `zone-depth`/`common-suffix` mode. Events without `dns_name` are unaffected, since they already trigger a full poll.
