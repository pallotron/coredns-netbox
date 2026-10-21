# Production Deployment

## Prerequisites

- A Kubernetes cluster
- [Helm](https://helm.sh/)
- A running Netbox instance (3.x or 4.x) with an API token

## Install

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

## High Availability (Multiple Replicas)

For zero-downtime deploys and node-failure resilience, run multiple CoreDNS replicas with a standalone sidecar. **No shared or RWX storage is required** — the sidecar serves zone files over HTTP from its own RWO PVC.

```bash
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set replicaCount=2 \
  --set sidecar.standalone=true \
  --set sidecar.storage.storageClass=standard-rwo  # your RWO StorageClass
```

The chart automatically creates a `ReadWriteOnce` PVC for the sidecar and configures all CoreDNS pods to fetch zones from the sidecar's HTTP endpoint.

**How it works:**

- One sidecar Deployment (strategy: `Recreate`) polls Netbox, writes zone files to its own RWO PVC, and serves them at `GET /zones/` on port 8082
- CoreDNS pods fetch zone files from the sidecar HTTP endpoint on startup (`zone-init`) and on every `Reload()` call
- After each zone write, the sidecar calls `ZoneReloadService.Reload()` on every CoreDNS pod — propagation is near-instant
- A fallback poll inside each CoreDNS pod (default 60s) covers gRPC failures
- The `Recreate` strategy means the sidecar's RWO PVC is released before the new pod starts — `dynamic.json` and zone files persist across updates via the PVC

**Storage class by platform:**

| Platform | StorageClass |
|---|---|
| GCP GKE | `standard-rwo` (pd-balanced) |
| AWS EKS | `gp2` or `gp3` |
| Azure AKS | `managed-csi` |
| k3d / k3s | `local-path` (default) |
| Bare-metal | any RWO provisioner |

## Persistent Zone Storage

By default zone files are written to an `emptyDir` volume that is discarded when the pod restarts. This means that if Netbox is unreachable when a pod starts, the init container will fail and Kubernetes will retry with exponential backoff until Netbox recovers — honest behaviour, but it delays restart.

For production environments where you want pods to restart immediately even during a full Netbox outage, enable persistent storage. Each pod gets its own `PersistentVolumeClaim` (one per replica, `ReadWriteOnce`). On restart, the init container finds the cached zone files from the previous run and lets CoreDNS start immediately. The sidecar then resumes polling and updates zones once Netbox is available again.

```bash
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set zoneStorage.persistent=true
```

To use a specific StorageClass (e.g. GKE SSD):

```bash
  --set zoneStorage.persistent=true \
  --set zoneStorage.storageClass=premium-rwo \
  --set zoneStorage.size=1Gi
```

**Cloud compatibility:**

| Platform | Default StorageClass | Works out of the box |
|---|---|---|
| GKE | `standard-rwo` | Yes |
| EKS | `gp2` | Yes |
| AKS | `default` | Yes |
| k3d / k3s | `local-path` | Yes |
| Bare-metal | *(none)* | Set `storageClass` explicitly |

**Upgrading an existing release** from `persistent: false` to `persistent: true` is a rolling upgrade — the workload is always a `StatefulSet`, so no kind change occurs. Helm will add the `volumeClaimTemplates` entry and Kubernetes will provision a PVC for each pod on the next rollout:

```bash
helm upgrade coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set zoneStorage.persistent=true
```

## Surviving a NetBox outage

The sidecar keeps the generated zone files on its own volume, and that
cache is what lets DNS keep answering when NetBox is unreachable. Two
behaviours make it work, and they are easy to mistake for each other:

**The sidecar will not overwrite the cache with nothing.** Until a fetch
has succeeded in the current process, merges are refused rather than run
against an empty record set — otherwise a restart during an outage would
delete every cached zone. You will see this in the log as
`skipping merge/write: no successful Netbox fetch yet and cached zone
files exist`.

**Readiness means "I can serve zones", not "my data is fresh".** A sidecar
holding cached zones reports Ready even when NetBox is down and the merge
above is being refused. This matters because the readiness probe gates the
sidecar's Service, and in standalone mode a restarting CoreDNS pod fetches
its zones from that Service — its own volume is an `emptyDir` and starts
empty. If readiness waited for a successful fetch, a NetBox outage plus a
CoreDNS restart would leave the new pod unable to reach zones that were
sitting on the sidecar's volume the whole time.

A sidecar with no zones at all stays NotReady: a fresh install during an
outage has genuinely nothing to serve, and answering from an empty zone is
worse than not answering.

Because readiness no longer implies freshness, **staleness is what you
alert on**:

```promql
netbox_sidecar_zone_staleness_seconds > 900
```

Only a successful fetch resets it, so it climbs for the whole outage and
is unaffected by merges of cached data.

## Zone Transfers (AXFR)

The primary CoreDNS can serve zone transfers to secondary DNS servers (CoreDNS, BIND, NSD, etc.) in remote data centers.

### Propagation to secondaries

The primary sends DNS NOTIFY (RFC 1996) whenever a zone's SOA serial
changes, so a secondary normally transfers within seconds of the change.
SOA refresh (default 300s) is the fallback, not the expected path.

Two deployment details decide whether a NOTIFY actually arrives, and both
fail silently — the zone still converges on the refresh timer, just slower:

1. **`transfer.to` must list concrete addresses.** It doubles as the NOTIFY
   target list (upstream's transfer plugin has no separate notify setting)
   and wildcards are skipped, so `transfer.to: ["*"]` notifies nobody.
2. **The secondary must accept the primary's source address.** A secondary
   compares the NOTIFY's source IP against its own `transfer from` list, so
   listing the primary's Service VIP is not enough when the packet leaves
   with a pod IP — the secondary logs `Dropping notify` and waits. List the
   address the primary actually sends from, or keep refresh low enough that
   the fallback is acceptable.

A NOTIFY to a Service VIP fronting several secondary replicas reaches only
one of them; the rest wait for refresh. Give each replica its own reachable
address if prompt convergence matters for all of them.

Two more things that quietly cost you the NOTIFY and leave refresh as the
only path:

- **Include the port when it is not 53.** `transfer.to` entries accept
  `host:port` and default to 53, so a secondary published on another port
  must be named as `10.0.1.5:5354`. Sent to the wrong port the NOTIFY is
  answered by whatever else is listening — in a Kubernetes cluster that is
  typically a SERVFAIL from an unrelated service, with nothing in the
  secondary's log to show for it.
- **Several primary replicas can waste a NOTIFY.** Each replica notifies
  after its own reload, but the secondary responds by querying the SOA of
  whatever is first in its own `transfer from` list — which may be a
  replica that has not reloaded yet. It logs `no SOA serial increase seen`
  and waits for its refresh timer. The window is short (replicas reload
  within seconds of each other) and the next change notifies again, but it
  means prompt convergence is best-effort rather than guaranteed with a
  multi-replica primary. Keeping `soa.refresh` low is what bounds it.

Watch `coredns_netboxreload_notify_total` to confirm sends are happening.

### Primary: Allow AXFR

Set `transfer.to` to the list of secondary IPs allowed to pull zones:

```bash
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set 'transfer.to[0]=10.0.1.5' \
  --set 'transfer.to[1]=10.0.1.6'
```

### Secondary: CoreDNS AXFR replica

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

### External secondaries

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
};
```
