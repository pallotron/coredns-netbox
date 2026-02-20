# CoreDNS with Netbox-backed Zone Discovery and AXFR Transfers

[![CI](https://github.com/pallotron/coredns-netbox/actions/workflows/ci.yaml/badge.svg)](https://github.com/pallotron/coredns-netbox/actions/workflows/ci.yaml)
[![E2E Tests](https://github.com/pallotron/coredns-netbox/actions/workflows/publish.yaml/badge.svg)](https://github.com/pallotron/coredns-netbox/actions/workflows/publish.yaml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.25.6-00ADD8?logo=go)](go.mod)
[![CoreDNS Image](https://img.shields.io/badge/ghcr.io-coredns--netbox-blue?logo=docker)](https://github.com/pallotron/coredns-netbox/pkgs/container/coredns-netbox)
[![Sidecar Image](https://img.shields.io/badge/ghcr.io-coredns--netbox--sidecar-blue?logo=docker)](https://github.com/pallotron/coredns-netbox/pkgs/container/coredns-netbox-sidecar)

A Helm chart deploying CoreDNS with Netbox-backed DNS zones. A Go sidecar periodically scrapes Netbox for IP addresses, auto-discovers zones from device names, intelligently selects management and BMC IPs using configurable patterns, and writes zone files to a shared volume. CoreDNS serves the zone files using the `auto` plugin and optionally falls through to the [netbox plugin](https://github.com/oz123/coredns-netbox-plugin) for records not yet in the zone files. Zone transfers (AXFR) are supported for replicating zones to secondary DNS servers.

**Key Features:**
- **Smart Interface Categorization**: Automatically identifies BMC, management, loopback, and dataplane interfaces using regex patterns
- **Device-Based Discovery**: Creates DNS records from device names even when `dns_name` is empty in Netbox
- **Multi-IP Support**: Generates both primary management records and BMC records with `-bmc` suffix
- **Flexible Zone Extraction**: Derives DNS zones from device naming conventions (e.g., `dc1-site13a-r101-hv-01` → `dc1-site.example.com`)
- **Reverse DNS (PTR) Records**: Automatically generates reverse zones (in-addr.arpa, ip6.arpa) with configurable prefix lengths

## Architecture

```mermaid
---
config:
    layout: elk
    theme: dark
    elk:
        mergeEdges: true
        nodePlacementStrategy: NETWORK_SIMPLEX
---
flowchart TD
    Client(["DNS Client"])
    Netbox[("Netbox API")]
    Upstream(["Upstream DNS (e.g. 8.8.8.8)"])
    Secondary["Secondary CoreDNS (AXFR replica)"]

    subgraph Primary["Primary Pod"]
        Init["Init Container (sidecar --run-once)"]
        CoreDNS["CoreDNS: cache · auto · netbox · transfer · forward"]
        Zones[("/zones/{zone_files}")]
        Sidecar["Sidecar"]
        Init -->|initial zone files| Zones
        Sidecar -->|writes| Zones
        CoreDNS -->|reads| Zones
    end

    Client -->|"UDP/TCP :53"| CoreDNS
    Sidecar -->|"Paralell, paginated, HTTP poll"| Netbox
    CoreDNS -->|"cache misses"| Upstream
    CoreDNS -.->|"AXFR (optional)"| Secondary
```

**Request flow (primary):** cache → auto (local zone files) → netbox (optional API fallthrough) → forward (upstream)

The sidecar polls Netbox on a configurable interval, fetches all active IP addresses with DNS names using parallel paginated requests, auto-discovers zones (by FQDN depth, common suffix, or Netbox DNS plugin), and atomically writes zone files. CoreDNS detects changes via SOA serial increments.

### Performance

The sidecar uses parallel paginated HTTP requests to fetch records from Netbox. With 18,000 IP addresses (6,000 hosts across 3 data centers, each with 3 interfaces), the sidecar fetches all records in 4-8 seconds and writes 3 zone files with 6,000 records each.

## Interface Categorization

The sidecar uses regex patterns to categorize network interfaces and intelligently select the appropriate IP for each device:

**Categories:**
- **BMC**: BMC/IPMI interfaces (pattern: `(?i)bmc|ipmi|ilo|idrac`) → Creates `{hostname}-bmc` DNS records
- **Management (VRF-based)**: Interfaces in management/OOB VRFs (pattern: `(?i)mgmt|oob`) → Primary DNS record (preferred)
- **Management (interface name)**: Interfaces with management naming (pattern: `(?i)mgmt|Management|fxp0|eth[01]|mgt|NET`) → Primary DNS record
- **Loopback**: Routing protocol loopbacks (pattern: `^lo$|^lo0|^Loopback`) → Skipped
- **Dataplane**: Production/storage traffic (pattern: `(?i)storage|vtep|vsan`) → Skipped

**Example:** A hypervisor `dc1-r101-prod-hv-01` with 5 interfaces generates:
- `dc1-r101-prod-hv-01.dc1.example.com` → Management IP (172.26.33.64)
- `dc1-r101-prod-hv-01-bmc.dc1.example.com` → BMC IP (172.26.1.64)

Dataplane interfaces (ovn-vtep-if, storage-if) are automatically excluded from DNS.

All patterns are configurable via environment variables (see Configuration below).

## Reverse DNS (PTR Records)

The sidecar automatically generates reverse DNS zones alongside forward zones using **static parent zones** that cover your entire IP space.

**Why Static Zones?**
- **Simple configuration**: Define a few large zones once, no dynamic discovery needed on secondaries
- **Easy zone transfers**: Secondary DNS servers just need the same static zone list
- **Scales efficiently**: One zone can cover thousands of subnets

**IPv4 Reverse Zones:**
Configure static parent zones that match your IP allocation:
- `10.in-addr.arpa` - Covers all of 10.0.0.0/8
- `16.172.in-addr.arpa,17.172.in-addr.arpa` - Covers 172.16.0.0/16 and 172.17.0.0/16
- Example: `10.1.2.3` → PTR record `3.2.1.10.in-addr.arpa. PTR server1.example.com.`

**IPv6 Reverse Zones:**
Use reverse nibble notation for IPv6 zones:
- `b.8.0.d.0.1.2.0.ip6.arpa` - Covers 2001:db8::/32

**Configuration:**
```yaml
# Helm values
reverseZones:
  enabled: true
  ipv4:
    - "10.in-addr.arpa"
    - "16.172.in-addr.arpa"
  ipv6:
    - "b.8.0.d.0.1.2.0.ip6.arpa"
```

Or via environment variables:
```bash
ENABLE_REVERSE_ZONES=true
REVERSE_ZONES_IPV4="10.in-addr.arpa,16.172.in-addr.arpa"
REVERSE_ZONES_IPV6="b.8.0.d.0.1.2.0.ip6.arpa"
```

**Secondary Configuration:**
Configure the same zones on your secondary DNS servers:
```yaml
secondary:
  enabled: true
  zones:
    - "dc1.example.com"           # Forward zones
    - "dc2.example.com"
    - "10.in-addr.arpa"           # Reverse zones (same as primary)
    - "16.172.in-addr.arpa"
  transferFrom: ["10.0.1.1"]
```

## Analyzing Your Data

Before deploying, use the analyzer tool to preview what DNS records will be generated:

```bash
# Fetch data from Netbox
NETBOX_TOKEN='your-token' NETBOX_URL='http://netbox.yourcompany.com' ./scripts/fetch_netbox_ips.sh
jq -s '[.[].results[]]' ./netbox_ips_dump/page_*.json > all_ips.json

# Build analyzer
make build.analyzer

# Analyze with your domain
./bin/analyzer -file all_ips.json -domain "yourcompany.com" -stats

# View specific devices
./bin/analyzer -file all_ips.json -domain "yourcompany.com" -device "dc1-r101" -format detailed -all

# Export to CSV
./bin/analyzer -file all_ips.json -domain "yourcompany.com" -format csv > dns_records.csv
```

See `cmd/analyzer/README.md` for full documentation.

## Production Deployment

### Prerequisites

- A Kubernetes cluster
- [Helm](https://helm.sh/)
- A running Netbox instance (3.x or 4.x) with an API token

### Install

Reference a pre-existing Kubernetes Secret containing the Netbox API token. This is the recommended approach — it keeps secrets out of Helm values and integrates with external secret management. (You can also pass `--set netbox.token=...` directly for quick testing.)

```bash
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox --create-namespace \
  --set netbox.url=http://your-netbox:80 \
  --set netbox.existingSecret=my-netbox-token
```

The referenced Secret must have a `token` key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-netbox-token
type: Opaque
stringData:
  token: "nbt_abc123.your-token-plaintext"
```

**External secret management:** The `existingSecret` value works with any tool that syncs secrets into Kubernetes — [External Secrets Operator](https://external-secrets.io/) (GCP Secret Manager, HashiCorp Vault, AWS Secrets Manager), [sealed-secrets](https://github.com/bitnami-labs/sealed-secrets), SOPS, etc. Just ensure the synced Secret has a `token` key.

### Zone Transfers (AXFR)

The primary CoreDNS can serve zone transfers to secondary DNS servers (CoreDNS, BIND, NSD, etc.) in remote data centers.

#### Primary: Allow AXFR

Set `transfer.to` to the list of secondary IPs allowed to pull zones:

```bash
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set 'transfer.to[0]=10.0.1.5' \
  --set 'transfer.to[1]=10.0.1.6'
```

#### Secondary: CoreDNS AXFR replica

Deploy a secondary CoreDNS that pulls zones via AXFR from the primary. The secondary can run in the same cluster, a different cluster, or a separate environment entirely:

```bash
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set 'transfer.to[0]=10.0.1.5' \
  --set secondary.enabled=true \
  --set 'secondary.zones[0]=dc1.mycompany.com' \
  --set 'secondary.zones[1]=dc2.mycompany.com' \
  --set 'secondary.transferFrom[0]=10.0.1.1'
```

#### External secondaries

If you're running secondary DNS servers outside of this Helm chart (standalone CoreDNS, BIND, NSD, etc.), configure them to pull from the primary IP:

CoreDNS secondary:
```
dc1.mycompany.com {
    secondary {
        transfer from <primary-ip>
    }
}
```

BIND secondary:
```
zone "dc1.mycompany.com" {
    type slave;
    masters { <primary-ip>; };
    allow-notify { <primary-ip>; };
};
```

## Configuration

**Netbox compatibility:** The DNS client works with both Netbox 3.x and 4.x. It uses raw HTTP against the stable Netbox REST API and auto-detects token type (v1 `Token` auth vs v2 `nbt_` `Bearer` auth).

### Helm Values

See `helm/coredns-netbox/values.yaml` for all options. Key values:

| Value | Default | Description |
|---|---|---|
| `netbox.url` | `http://netbox.netbox.svc.cluster.local` | Netbox API URL |
| `netbox.token` | `""` | Netbox API token (use `existingSecret` instead for production) |
| `netbox.existingSecret` | `""` | Name of existing Secret with `token` key |
| `netboxPlugin.enabled` | `false` | Enable CoreDNS netbox plugin for live API fallthrough |
| `netboxPlugin.zones` | `[]` | **Required when enabled.** Zones the netbox plugin handles; queries outside these zones go straight to forward |
| `zoneDiscovery.mode` | `zone-depth` | Zone discovery mode: `zone-depth`, `common-suffix`, or `netbox-dns` |
| `zoneDiscovery.depth` | `2` | Number of trailing labels to use as zone name (zone-depth mode) |
| `zoneDir` | `/zones` | Directory for zone files |
| `pollInterval` | `60s` | How often to poll Netbox |
| `ttl` | `300` | Default TTL for DNS records |
| `primaryNS` | `ns1.example.org.` | SOA primary nameserver |
| `adminEmail` | `admin.example.org.` | SOA admin email (dot notation) |
| `pageSize` | `1000` | Records per API page (max 1000) |
| `maxConcurrency` | `10` | Parallel API request limit |
| `forwardServers` | `[1.1.1.1, 8.8.8.8]` | Upstream DNS resolvers |
| `transfer.to` | `[]` | IPs allowed to pull AXFR (empty = disabled) |
| `secondary.enabled` | `false` | Deploy a secondary CoreDNS for AXFR replication |
| `secondary.zones` | `[]` | Zones to replicate on the secondary |
| `secondary.transferFrom` | `[]` | Primary IPs to pull AXFR from |
| `hostPort.enabled` | `true` | Expose primary CoreDNS on host port 53 |
| `reverseZones.enabled` | `true` | Enable automatic PTR record generation |
| `reverseZones.ipv4` | `["10.in-addr.arpa", ...]` | Static IPv4 reverse zones |
| `reverseZones.ipv6` | `[]` | Static IPv6 reverse zones |
| `metrics.enabled` | `true` | Expose sidecar `/metrics` and CoreDNS metrics ports via the Service |
| `metrics.port` | `9153` | CoreDNS prometheus plugin port |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |

### Interface Categorization Patterns

Customize interface categorization via environment variables:

| Environment Variable | Default | Description |
|---|---|---|
| `BMC_INTERFACE_PATTERN` | `(?i)bmc\|ipmi\|ilo\|idrac` | Regex for BMC interfaces |
| `LOOPBACK_PATTERN` | `^lo$\|^lo0\|^Loopback` | Regex for loopback interfaces |
| `DATAPLANE_PATTERN` | `(?i)storage\|vtep\|vsan` | Regex for dataplane interfaces |
| `MGMT_VRF_PATTERN` | `(?i)mgmt\|oob` | Regex for management VRFs |
| `MGMT_INTERFACE_PATTERN` | `(?i)mgmt\|Management\|fxp0\|eth[01]\|mgt\|NET` | Regex for management interfaces |

These can be set in the Helm chart's `env` values or directly in the sidecar container.

### Zone Discovery Modes

The sidecar auto-discovers zones from Netbox FQDNs. Three modes are available:

- **`zone-depth`** (default): Uses the last N labels of each FQDN as the zone name. With `depth=2`, `server1-mgmt.dc1.mycompany.com` becomes zone `mycompany.com`. With `depth=3`, it becomes `dc1.mycompany.com`.
- **`common-suffix`**: Groups records by their longest common domain suffix.
- **`netbox-dns`**: Queries the Netbox DNS plugin API (`/api/plugins/netbox-dns/zones/`) and matches records to their longest-matching zone.

## Observability

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

In the dev environment the sidecar metrics port is mapped to `127.0.0.1:18082`:
```bash
curl -s http://127.0.0.1:18082/metrics | grep netbox_sidecar
```

## How It Works

1. **Init container** runs the sidecar with `--run-once` to generate initial zone files before CoreDNS starts
2. **Sidecar** continuously polls Netbox for all active IP addresses:
   - Fetches device information, interface names, VRFs, and IP addresses
   - Categorizes interfaces using regex patterns (BMC, management, loopback, dataplane)
   - Groups IPs by device and selects the best management IP (prefers VRF-based over interface name)
   - Extracts DNS zones from device names (e.g., `dc1-site13a-r101-hv-01` → `dc1-site.example.com`)
   - Generates DNS records: `{device}.{zone}` for management, `{device}-bmc.{zone}` for BMC
   - Generates reverse zones (in-addr.arpa, ip6.arpa) with PTR records mapping IPs back to hostnames
   - Atomically writes zone files only when changes are detected
3. **CoreDNS** serves DNS using local zone files via the `auto` plugin, with optional fallthrough to the netbox plugin for cache misses
4. **CoreDNS `auto` plugin** watches the zone directory and auto-loads new or changed zone files (every 10s)
5. Queries for names outside discovered zones are forwarded to upstream resolvers
6. If zone transfers are configured, the primary notifies secondaries on zone changes and serves AXFR requests

## Development

### Prerequisites

- Go 1.25+ ([mise](https://mise.jdx.dev/) recommended)
- Docker
- [k3d](https://k3d.io/)
- [Helm](https://helm.sh/)
- kubectl

```bash
brew install k3d helm kubectl
```

### Quick Start

Spin up a full local dev environment (k3d cluster + Netbox + seed data + CoreDNS):

```bash
make dev
```

Then test:

```bash
# Forward lookups (use +tcp for macOS, see note below)
dig @127.0.0.1 -p 15353 +tcp server1-mgmt.dc1.mycompany.com A
dig @127.0.0.1 -p 15353 +tcp server500-bmc.dc2.mycompany.com A
dig @127.0.0.1 -p 15353 +tcp google.com A  # forwarded

# Reverse lookups (PTR records)
dig @127.0.0.1 -p 15353 +tcp -x 10.1.0.1
dig @127.0.0.1 -p 15353 +tcp -x 10.2.8.244

# Secondary
dig @127.0.0.1 -p 15354 +tcp server1-mgmt.dc1.mycompany.com A
```

**Note for macOS users:** Docker Desktop on macOS has a known limitation with UDP port forwarding between the host and containers. DNS queries via UDP will timeout when querying from your Mac host to the k3d cluster. Use the `+tcp` flag with dig to force TCP queries, which work correctly. This limitation does not affect:
- DNS queries inside the Kubernetes cluster (both TCP and UDP work)
- Production deployments on real Kubernetes clusters
- Linux users (Docker on Linux handles UDP correctly)

If you need UDP for testing, run queries from inside a pod:
```bash
kubectl run -n coredns-netbox dnstest --rm -it --image=busybox -- /bin/sh
# Inside the pod:
nslookup server1-mgmt.dc1.mycompany.com 10.43.100.53  # UDP works fine
```

### Project Structure

```
├── cmd/
│   ├── analyzer/main.go          # CLI tool: analyze Netbox data & preview DNS records
│   └── sidecar/main.go           # Sidecar: poll Netbox → write zone files
├── internal/
│   ├── config/                   # Env var configuration + categorization patterns
│   ├── ipcategorizer/            # Interface categorization & device IP selection
│   ├── metrics/                  # Prometheus metrics definitions for the sidecar
│   ├── netboxclient/             # Raw HTTP client for Netbox IPAM with parallel pagination
│   ├── zonediscovery/            # Zone auto-discovery (zone-depth, common-suffix, netbox-dns)
│   ├── zonegen/                  # Zone file generator (atomic writes, SOA serial)
│   └── zonemanager/              # Multi-zone lifecycle (create/update/remove zone files)
├── coredns/
│   ├── Dockerfile                # CoreDNS with netbox plugin
│   └── plugin.cfg                # Plugin ordering
├── docker/sidecar/Dockerfile     # Sidecar image
├── helm/coredns-netbox/          # Helm chart
├── scripts/                      # Utility scripts (fetch Netbox data)
├── dev/                          # k3d + Netbox dev config + seed scripts
└── tests/e2e/                    # DNS resolution tests
```

### Makefile Targets

**Build & Test:**

| Target | Description |
|---|---|
| `make build` | Build sidecar and analyzer binaries |
| `make build.sidecar` | Build sidecar binary only |
| `make build.analyzer` | Build analyzer CLI tool |
| `make test.unit` | Run unit tests |
| `make test.e2e` | Run e2e DNS tests (requires running dev env) |
| `make lint` | Run golangci-lint |
| `make clean` | Remove build artifacts |

**Dev Environment:**

| Target | Description |
|---|---|
| `make dev` | Full dev environment (cluster + netbox + seed + deploy) |
| `make dev.cluster` | Create k3d cluster |
| `make dev.netbox` | Deploy Netbox via Helm |
| `make dev.token` | Create Netbox API token and store in K8s Secret |
| `make dev.seed` | Seed Netbox with 18,000 test IP addresses via Django ORM |
| `make dev.images` | Build and import Docker images into k3d |
| `make dev.deploy` | Deploy Helm chart (run twice for secondary ClusterIP pickup) |
| `make dev.teardown` | Delete k3d cluster |

### Step-by-Step Setup

```bash
# 1. Create the k3d cluster
make dev.cluster

# 2. Deploy Netbox
make dev.netbox

# 3. Create API token
make dev.token

# 4. Seed Netbox with 18,000 test DNS records
make dev.seed

# 5. Build Docker images and import into k3d
make dev.images

# 6. Deploy the Helm chart (primary + secondary)
make dev.deploy

# 7. Run again to pick up primary ClusterIP for secondary
make dev.deploy

# 8. Test DNS resolution (use +tcp on macOS, see Development section above)
dig @127.0.0.1 -p 15353 +tcp server1-mgmt.dc1.mycompany.com A       # primary
dig @127.0.0.1 -p 15354 +tcp server1-mgmt.dc1.mycompany.com A       # secondary (AXFR replica)
dig @127.0.0.1 -p 15353 +tcp dc1.mycompany.com AXFR                 # full zone transfer
```

### Checking Status

You can also use [k9s](https://k9scli.io/) for a convenient interactive TUI.

```bash
k3d cluster list                       # cluster status
kubectl get pods -n netbox             # Netbox pods
kubectl get pods -n coredns-netbox     # CoreDNS pods (primary + secondary)
kubectl logs -n coredns-netbox -l app.kubernetes.io/component=primary -c sidecar   # sidecar logs
kubectl logs -n coredns-netbox -l app.kubernetes.io/component=primary -c coredns   # CoreDNS logs
kubectl logs -n coredns-netbox -l app.kubernetes.io/component=secondary            # secondary logs
```

### Rebuilding After Code Changes

```bash
make dev.images dev.deploy
```

### Seed Data

The seed script (`dev/seed-ips.py`) bulk-creates 18,000 IP addresses directly via Django ORM inside the Netbox pod. This simulates a production-scale environment with 6,000 hosts across 3 data centers, each with 3 network interfaces:

- **3 DCs:** dc1, dc2, dc3 (2,000 hosts each)
- **3 interfaces per host:** mgmt, bmc, storage
- **FQDN pattern:** `server<N>-<if_name>.<dc>.mycompany.com`
- **IP addressing:** `10.<dc_id>.<subnet>.<host>/24`

| Example FQDN | Address | Zone |
|---|---|---|
| server1-mgmt.dc1.mycompany.com | 10.1.0.1/24 | dc1.mycompany.com |
| server1-bmc.dc1.mycompany.com | 10.1.8.1/24 | dc1.mycompany.com |
| server1-storage.dc1.mycompany.com | 10.1.16.1/24 | dc1.mycompany.com |
| server500-mgmt.dc2.mycompany.com | 10.2.1.246/24 | dc2.mycompany.com |
| server2000-storage.dc3.mycompany.com | 10.3.23.208/24 | dc3.mycompany.com |
