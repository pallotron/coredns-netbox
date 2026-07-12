# Netbox Webhook Trigger

The sidecar can apply Netbox `ipam.ipaddress` changes immediately, via a webhook Netbox posts directly to the sidecar — no message broker, no full Netbox re-fetch for the common case.

## How it works

```
Netbox (ipam.ipaddress webhook, HMAC-signed)
    → POST /webhook/netbox on the sidecar's HEALTH_ADDR port
    → verify X-Hook-Signature (HMAC-SHA512)
    → parse event (created/updated/deleted)
    → write directly into the dynamic record store
    → trigger the same debounced re-merge + zone write + CoreDNS reload
      path the gRPC DynamicZoneService API already uses
```

The periodic poll (`POLL_INTERVAL`) keeps running unchanged as a reconciliation backstop — it is not required for correctness, only for records outside the webhook's scope (see Limitations) and for recovering from a missed webhook delivery.

## Enabling it

Set `NETBOX_WEBHOOK_SECRET` to a shared secret and configure a matching Netbox webhook + event rule:

- Webhook `payload_url`: `http://<sidecar-service>:8082/webhook/netbox`
- Webhook `secret`: same value as `NETBOX_WEBHOOK_SECRET`
- Event rule `object_types`: `ipam.ipaddress`
- Event rule `event_types`: `object_created`, `object_updated`, `object_deleted`

If `NETBOX_WEBHOOK_SECRET` is unset, the `/webhook/netbox` route is not registered at all — there is no way to accidentally expose an unauthenticated trigger endpoint.

## Limitations

- **Only records with `dns_name` already set in Netbox are handled.** Device-based DNS name generation (`ipcategorizer`/`nameformat`, used when `dns_name` is empty) needs cross-record context — such as CNAME alias collision resolution across an entire zone — that a single webhook event doesn't have. Those records are only refreshed by the periodic poll.
- **Concurrent/out-of-order delivery is handled, not prevented.** Netbox dispatches webhooks via a background worker queue, so a burst of changes (e.g. one bulk edit) can arrive out of order. Each webhook-sourced record carries the originating event's timestamp; an older event can never overwrite an already-applied newer one for the same name.
- **A webhook-sourced record never permanently shadows Netbox.** It's automatically cleared once the next full poll proves it's already reflected there — so a dropped webhook delivery self-heals on the next poll cycle rather than leaving stale data indefinitely.
- **A manually-added record (via the `DynamicZoneService` gRPC API) always wins over a webhook-sourced one for the same name.** The webhook write is skipped and logged at `WARN`.
- **A deleted record can be resurrected by a delayed, out-of-order "created"/"updated" event for the same name.** Unlike the upsert path, `applyDelete` has no timestamp-based ordering guard — Netbox delivers webhooks via a background worker queue, so a delayed create/update arriving after a delete for the same DNS name will recreate it. This is a known, accepted gap (not yet mitigated by a tombstone mechanism); the periodic poll's reconciliation bounds how long a resurrected record can persist before being corrected.
- In `netbox-dns` discovery mode, zone routing for a webhook event makes its own Netbox API call per event (same `Discoverer.Discover` used by the full fetch) — this does not benefit from the same API-call reduction as `zone-depth`/`common-suffix` mode.
