# Observability

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

A Prometheus `ServiceMonitor` can be enabled via `metrics.serviceMonitor.enabled: true` for automatic scrape discovery in clusters running the Prometheus Operator.

## Dev Environment

In the dev environment the sidecar metrics port is mapped to `127.0.0.1:18082`:
```bash
curl -s http://127.0.0.1:18082/metrics | grep netbox_sidecar
```
