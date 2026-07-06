# Observability

## Health Endpoints

The sidecar serves two probe endpoints on `HEALTH_ADDR` (`:8082` by default):

- `/livez` — returns 200 as soon as the HTTP server is listening. Use for the liveness probe; it only asserts the process is up, so a slow first NetBox fetch is never mistaken for a hung process.
- `/healthz` — returns 200 after the first successful NetBox fetch and zone merge. Use for the readiness probe; zone-init containers also wait on it before fetching zone files.

## Prometheus Metrics

The sidecar exposes a Prometheus metrics endpoint on the same port as `/healthz` (`:8082` by default, controlled by `HEALTH_ADDR`). In Kubernetes the port is named `sidecar-health` and exposed via the Service when `metrics.enabled: true`.

```bash
curl http://localhost:8082/metrics | grep netbox_sidecar
```

| Metric | Type | Labels | Description |
|---|---|---|---|
| `netbox_sidecar_poll_total` | Counter | `result=success\|error` | Total poll attempts |
| `netbox_sidecar_poll_duration_seconds` | Histogram | — | Full poll cycle duration (fetch + discover + write) |
| `netbox_sidecar_last_successful_poll_timestamp_seconds` | Gauge | — | Unix timestamp of the last successful poll |
| `netbox_sidecar_netbox_fetch_duration_seconds` | Histogram | — | Netbox HTTP fetch duration |
| `netbox_sidecar_netbox_records_fetched` | Gauge | — | IP records returned by Netbox in the last poll |
| `netbox_sidecar_netbox_empty_response_total` | Counter | — | Polls where Netbox returned zero records |
| `netbox_sidecar_zones_active` | Gauge | — | Number of DNS zones currently managed |
| `netbox_sidecar_zone_writes_total` | Counter | `op=create\|update\|delete` | Zone file operations |
| `netbox_sidecar_zone_write_errors_total` | Counter | — | Zone file write failures |
| `netbox_sidecar_netbox_fetch_retries_total` | Counter | — | Netbox HTTP request retries (excludes first attempt) |
| `netbox_sidecar_zone_staleness_seconds` | Gauge | — | Seconds since the last successful poll (0 when healthy) |
| `netbox_sidecar_coredns_reload_total` | Counter | `result=success\|error` | CoreDNS reload pushes per target address, final outcome after retries |
| `netbox_sidecar_coredns_reload_retries_total` | Counter | — | CoreDNS reload push retries (excludes first attempt) |
| `netbox_sidecar_coredns_reload_dirty_targets` | Gauge | — | CoreDNS addresses whose last reload push has not succeeded yet; re-pushed on every poll cycle, so non-zero for more than a few cycles means a pod is not accepting reloads |

A Prometheus `ServiceMonitor` can be enabled via `metrics.serviceMonitor.enabled: true` for automatic scrape discovery in clusters running the Prometheus Operator.

## CoreDNS Plugin Metrics

The `netboxreload` plugin exports its own metrics through the standard CoreDNS `prometheus` endpoint (`:9153` by default), alongside the built-in query metrics:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `coredns_netboxreload_reload_total` | Counter | `trigger=grpc\|poll\|startup`, `result=reloaded\|unchanged\|error` | Zone reload attempts; `unchanged` means all source content matched the previous load and nothing was re-parsed |
| `coredns_netboxreload_last_reload_timestamp_seconds` | Gauge | — | Unix timestamp of the last successful reload check (including unchanged ones) |
| `coredns_netboxreload_zones_loaded` | Gauge | — | Zones currently loaded in memory |

In steady state expect `result="unchanged"` on nearly every poll tick; a growing `result="error"` rate means the plugin cannot reach its zone source.

## Dev Environment

In the dev environment the sidecar metrics port is mapped to `127.0.0.1:18082`:
```bash
curl -s http://127.0.0.1:18082/metrics | grep netbox_sidecar
```
