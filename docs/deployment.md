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

For zero-downtime deploys and node-failure resilience, run multiple CoreDNS replicas with a standalone sidecar sharing a `ReadWriteMany` (RWX) PVC.

```bash
# 1. Pre-create a RWX PVC (e.g. GCP Filestore, AWS EFS, Azure Files)
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: coredns-zones
  namespace: coredns-netbox
spec:
  accessModes: [ReadWriteMany]
  storageClassName: filestore-rwx   # your RWX StorageClass
  resources:
    requests:
      storage: 1Gi
EOF

# 2. Deploy with standalone sidecar and multiple replicas
helm upgrade --install coredns-netbox ./helm/coredns-netbox \
  -n coredns-netbox \
  --set netbox.existingSecret=my-netbox-token \
  --set replicaCount=2 \
  --set sidecar.standalone=true \
  --set zoneStorage.existingClaim=coredns-zones
```

**How it works:**

- One sidecar Deployment polls Netbox and writes zones to the shared PVC
- All N CoreDNS pods read zones from the same PVC
- After each zone write the sidecar calls `ZoneReloadService.Reload()` on every CoreDNS pod via the headless Service — propagation is near-instant
- A fallback poll inside each CoreDNS pod (default 60s) covers gRPC failures
- The sidecar uses `maxSurge: 1 / maxUnavailable: 0` — a new sidecar pod becomes Ready before the old one stops, so the gRPC control plane never has a gap

**RWX storage options by platform:**

| Platform | StorageClass | Notes |
|---|---|---|
| GCP | `filestore` (Filestore CSI) | Requires Filestore instance pre-provisioned |
| AWS | `efs-sc` (EFS CSI) | Dynamic provisioning via EFS access points |
| Azure | `azurefile` | Built-in, no extra setup |
| Bare-metal | NFS or Longhorn (RWX mode) | |

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

## Zone Transfers (AXFR)

The primary CoreDNS can serve zone transfers to secondary DNS servers (CoreDNS, BIND, NSD, etc.) in remote data centers.

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
    allow-notify { <primary-ip>; };
};
```
