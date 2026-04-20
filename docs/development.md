# Development

## Prerequisites

- Go 1.25+ ([mise](https://mise.jdx.dev/) recommended)
- Docker
- [k3d](https://k3d.io/)
- [Helm](https://helm.sh/)
- kubectl

```bash
brew install k3d helm kubectl
```

## Quick Start

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

## Project Structure

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
│   ├── Dockerfile                # CoreDNS image (standard plugins + auto + secondary)
│   └── plugin.cfg                # Plugin ordering
├── docker/sidecar/Dockerfile     # Sidecar image
├── helm/coredns-netbox/          # Helm chart
├── scripts/                      # Utility scripts (fetch Netbox data)
├── dev/                          # k3d + Netbox dev config + seed scripts
└── tests/e2e/                    # DNS resolution tests
```

## Makefile Targets

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

## Step-by-Step Setup

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

# 8. Test DNS resolution (use +tcp on macOS, see above)
dig @127.0.0.1 -p 15353 +tcp server1-mgmt.dc1.mycompany.com A       # primary
dig @127.0.0.1 -p 15354 +tcp server1-mgmt.dc1.mycompany.com A       # secondary (AXFR replica)
dig @127.0.0.1 -p 15353 +tcp dc1.mycompany.com AXFR                 # full zone transfer
```

## Checking Status

You can also use [k9s](https://k9scli.io/) for a convenient interactive TUI.

```bash
k3d cluster list                       # cluster status
kubectl get pods -n netbox             # Netbox pods
kubectl get pods -n coredns-netbox     # CoreDNS pods (primary + secondary)
kubectl logs -n coredns-netbox -l app.kubernetes.io/component=primary -c sidecar   # sidecar logs
kubectl logs -n coredns-netbox -l app.kubernetes.io/component=primary -c coredns   # CoreDNS logs
kubectl logs -n coredns-netbox -l app.kubernetes.io/component=secondary            # secondary logs
```

## Rebuilding After Code Changes

```bash
make dev.images dev.deploy
```

## Seed Data

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

## Known Limitations

- **E2E tests only run on version tag pushes** (`v*.*.*`) and manual workflow dispatch, not on PRs. This means E2E regressions can reach `main` undetected. To run E2E locally before merging, use `make dev && make dev.wait && make test.e2e`.
