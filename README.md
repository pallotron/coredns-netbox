# CoreDNS with Netbox-backed Zone Discovery and AXFR Transfers

A Helm chart deploying CoreDNS with Netbox-backed DNS zones. A Go sidecar periodically scrapes Netbox for IP addresses, auto-discovers zones (by FQDN depth, common suffix, or via the Netbox DNS plugin API), and writes zone files to a shared volume. CoreDNS serves the zone files using the `auto` plugin and optionally falls through to the [netbox plugin](https://github.com/oz123/coredns-netbox-plugin) for records not yet in the zone files. Zone transfers (AXFR) are supported for replicating zones to secondary DNS servers.

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │            Primary Pod                  │
                    │                                         │
   DNS query        │  ┌───────────┐       ┌──────────────┐  │
  ──────────────────┼─▶│  CoreDNS  │       │   Sidecar    │  │
   UDP/TCP :53      │  │           │       │              │  │
                    │  │  auto ────┼───┐   │  poll Netbox │  │
                    │  │  netbox   │   │   │  discover    │  │
                    │  │  transfer │   │   │    zones     │  │
                    │  │  forward  │   │   │  write zone  │  │
                    │  └───────────┘   │   │    files     │  │
                    │                  │   └──────┬───────┘  │
                    │              ┌───┴──────────┴───┐      │
                    │              │  emptyDir /zones │      │
                    │              └──────────────────┘      │
                    └──────────────┬──────────────────────────┘
                                   │
                    ┌──────────────┼──────────────────────────┐
                    │              ▼                          │
                    │    AXFR zone transfer (optional)        │
                    │              │                          │
                    │  ┌───────────▼─────┐   ┌────────────┐  │
                    │  │ Secondary       │   │            │  │
                    │  │ CoreDNS         │   │  Netbox    │  │
                    │  │ (AXFR replica)  │   │  (API)     │  │
                    │  └─────────────────┘   └────────────┘  │
                    └─────────────────────────────────────────┘
```

**Request flow (primary):** cache → auto (local zone files) → netbox (optional API fallthrough) → forward (upstream)

The sidecar polls Netbox on a configurable interval, fetches all active IP addresses with DNS names using parallel paginated requests, auto-discovers zones (by FQDN depth, common suffix, or Netbox DNS plugin), and atomically writes zone files. CoreDNS detects changes via SOA serial increments.

### Performance

The sidecar uses parallel paginated HTTP requests to fetch records from Netbox. With 18,000 IP addresses (6,000 hosts across 3 data centers, each with 3 interfaces), the sidecar fetches all records in 4-8 seconds and writes 3 zone files with 6,000 records each.

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

### Zone Discovery Modes

The sidecar auto-discovers zones from Netbox FQDNs. Three modes are available:

- **`zone-depth`** (default): Uses the last N labels of each FQDN as the zone name. With `depth=2`, `server1-mgmt.dc1.mycompany.com` becomes zone `mycompany.com`. With `depth=3`, it becomes `dc1.mycompany.com`.
- **`common-suffix`**: Groups records by their longest common domain suffix.
- **`netbox-dns`**: Queries the Netbox DNS plugin API (`/api/plugins/netbox-dns/zones/`) and matches records to their longest-matching zone.

## How It Works

1. **Init container** runs the sidecar with `--run-once` to generate initial zone files before CoreDNS starts
2. **CoreDNS** serves DNS using local zone files via the `auto` plugin, with optional fallthrough to the netbox plugin for cache misses
3. **Sidecar** continuously polls Netbox, discovers zones from FQDNs, and atomically writes zone files only when changes are detected
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
dig @127.0.0.1 -p 15353 server1-mgmt.dc1.mycompany.com A
dig @127.0.0.1 -p 15353 server500-bmc.dc2.mycompany.com A
dig @127.0.0.1 -p 15353 google.com A  # forwarded

# Secondary (after second `make dev.deploy` to pick up primary ClusterIP)
dig @127.0.0.1 -p 15354 server1-mgmt.dc1.mycompany.com A
```

### Project Structure

```
├── cmd/
│   └── sidecar/main.go           # Sidecar: poll Netbox → write zone files
├── internal/
│   ├── config/                   # Env var configuration
│   ├── netboxclient/             # Raw HTTP client for Netbox IPAM with parallel pagination
│   ├── zonediscovery/            # Zone auto-discovery (zone-depth, common-suffix, netbox-dns)
│   ├── zonegen/                  # Zone file generator (atomic writes, SOA serial)
│   └── zonemanager/              # Multi-zone lifecycle (create/update/remove zone files)
├── coredns/
│   ├── Dockerfile                # CoreDNS with netbox plugin
│   └── plugin.cfg                # Plugin ordering
├── docker/sidecar/Dockerfile     # Sidecar image
├── helm/coredns-netbox/          # Helm chart
├── dev/                          # k3d + Netbox dev config + seed scripts
└── tests/e2e/                    # DNS resolution tests
```

### Makefile Targets

**Build & Test:**

| Target | Description |
|---|---|
| `make build` | Build sidecar binary |
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

# 8. Test DNS resolution
dig @127.0.0.1 -p 15353 server1-mgmt.dc1.mycompany.com A       # primary
dig @127.0.0.1 -p 15354 server1-mgmt.dc1.mycompany.com A       # secondary (AXFR replica)
dig @127.0.0.1 -p 15353 dc1.mycompany.com AXFR +tcp             # full zone transfer
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
