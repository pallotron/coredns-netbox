# Scale and capacity

How the sidecar behaves as NetBox grows, which numbers to watch, and how to
re-measure them for your own deployment.

## Where the time goes

At scale the poll cycle is dominated by the **NetBox fetch**, not by zone
generation. Fetching tens of thousands of IP records over the paginated REST
API and decoding the JSON takes tens of seconds; turning those records into
zone files takes well under a second.

That ordering drives everything below, and it is worth confirming on your own
deployment rather than assuming:

| Phase | Metric |
|---|---|
| NetBox fetch | `netbox_sidecar_netbox_fetch_duration_seconds` |
| Merge + generate + write | `netbox_sidecar_poll_duration_seconds` |
| **Whole cycle** | `netbox_sidecar_poll_cycle_duration_seconds` |

Compare the last one against `POLL_INTERVAL`. That ratio is the headroom
number: because the cycle is fetch-bound, it grows roughly linearly with the
NetBox record count, and once it meets the interval, polls begin to overlap.
Raise `POLL_INTERVAL`, or give the sidecar more CPU, before reaching that
point.

**The sidecar's CPU limit is usually the lever that matters.** Decoding a
large JSON result set is CPU-bound, so a sidecar running near its CPU limit
is throttled, which inflates the fetch phase directly. If the cycle is too
close to the interval, raise the CPU limit first — it is cheaper and more
effective than tuning `NETBOX_PAGE_SIZE` or `NETBOX_MAX_CONCURRENCY`.

Memory is typically the lesser concern: CoreDNS holds two zone sets during a
reload, but zone files are small relative to the container limits in the
chart. Check `kubectl top pods` against those limits rather than assuming.

## Regression guard (runs in CI)

`TestScale_DeviceDominatedPipeline` (`cmd/sidecar/scale_test.go`) runs the
record pipeline — device categorization, name templating, zone discovery,
zone generation — over a large, **device-dominated** corpus.

The shape is the point. A deployment whose DNS records come mostly from
devices exercises the expensive path: `SelectDeviceIPs` compares all of a
device's interfaces, the formatter renders a canonical plus aliases per
device, and CNAME collision resolution runs across the whole zone. A corpus
built mostly from IPs carrying an explicit `dns_name` — which is the shape of
the e2e seed — barely touches any of that, so it cannot catch a regression
there.

The test asserts a deliberately generous ceiling, for work that completes in
well under a second. It is a tripwire for an accidental O(n²), not a
performance measurement: timing assertions tight enough to measure
performance would mostly measure the CI runner.

```sh
go test ./cmd/sidecar/ -run TestScale_DeviceDominatedPipeline -v   # prints timings
go test ./cmd/sidecar/ -bench ScaleDeviceDominated -run '^$' -cpuprofile cpu.out
```

## Full-stack drill (one-off, before a significant rollout)

The guard above covers generation only. To exercise fetch, reload and memory
end to end, run the drill against a cluster — deliberately **not** a nightly
CI job: on a shared runner the numbers measure the runner, and a noisy
nightly is a nightly everyone ignores.

```sh
make dev.up                       # k3d cluster + NetBox + chart
SEED_DEVICES=25000 make dev.seed.scale
make dev.scale.report             # cycle timings, zone counts, memory
```

What to record, and what each one tells you:

1. `netbox_sidecar_poll_cycle_duration_seconds` versus `POLL_INTERVAL` —
   the headroom number.
2. `netbox_sidecar_netbox_fetch_duration_seconds` — confirms whether the
   cycle is still fetch-bound (it should be).
3. `kubectl top pods` for sidecar and CoreDNS versus their chart limits —
   peak RSS, remembering CoreDNS holds two zone sets mid-reload.
4. `netbox_sidecar_zones_active` and the emitted record count — that the
   corpus produced the zones you expected.

When a deployment already runs at the scale you care about, read these from
its metrics instead: it is real data, needs no simulation, and costs a
scrape.
