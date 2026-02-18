# Helm Chart Production Readiness

Findings from reviewing `helm/coredns-netbox/` for production use.

## Done ✅

### 1. Primary service selector matches secondary pods ✅

`service.yaml` now includes `app.kubernetes.io/component: primary` in its selector, and the primary deployment's pod template carries the same label. Traffic cannot hit secondary pods.

### 4. `transfer { to * }` allows unrestricted AXFR ✅

`transfer.to` is now a list defaulting to `[]`. The transfer block is omitted from the Corefile when the list is empty.

### 10. Busybox init container hardcoded and fragile ✅

The busybox `resolve-primary` init container has been eliminated entirely. The secondary deployment needs no init container.

---

## Critical

### 2. No `securityContext` anywhere

All containers (CoreDNS, sidecar, zone-init) run as root with full capabilities.

**Fix:** Add pod-level and container-level security contexts to both deployments:

```yaml
# Pod level
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  runAsGroup: 1000
  fsGroup: 1000
  seccompProfile:
    type: RuntimeDefault

# Container level
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
    add: ["NET_BIND_SERVICE"]  # only on CoreDNS containers (port 53)
```

### 3. `hostPort.enabled: true` by default

Current default breaks multi-replica setups, forces `Recreate` strategy (no zero-downtime upgrades), and can interfere with node-level DNS.

**Fix:** Default `hostPort.enabled` to `false`. Add a guard or `NOTES.txt` warning if `replicaCount > 1` and `hostPort.enabled: true`.

### 5. No resource limits on init container or secondary

The `zone-init` init container and the secondary CoreDNS container both lack `resources:` blocks. The primary `coredns` and `sidecar` containers are already covered.

**Fix:** Add resource values and wire them in:

```yaml
resources:
  zoneInit:
    requests: { cpu: 50m, memory: 32Mi }
    limits: { cpu: 100m, memory: 64Mi }
  secondary:
    requests: { cpu: 100m, memory: 64Mi }
    limits: { cpu: 200m, memory: 128Mi }
```

### 9. No validation that netbox credentials are set

Deploying with both `netbox.token: ""` and `netbox.existingSecret: ""` creates a Secret with an empty token. The sidecar fails at runtime with no clear Helm-time error.

**Fix:** Add a `required` guard in `secret.yaml`:

```yaml
{{- if not .Values.netbox.existingSecret }}
{{- if not .Values.netbox.token }}
  {{- fail "Either netbox.token or netbox.existingSecret must be set" }}
{{- end }}
{{- end }}
```

---

## Important

### 6. No PodDisruptionBudget

A single replica with no PDB means any node drain causes DNS outage.

**Fix:** Add `templates/pdb.yaml` gated by `pdb.enabled`:

```yaml
pdb:
  enabled: true
  minAvailable: 1
```

### 7. No anti-affinity / topology spread

All replicas can schedule on the same node. A single node failure takes down DNS.

**Fix:** Add default soft anti-affinity and expose `affinity`, `topologySpreadConstraints` in values:

```yaml
affinity: {}
topologySpreadConstraints: []
```

### 8. No Prometheus metrics

CoreDNS ships a `prometheus` plugin but the Corefile doesn't enable it. No query rate, latency, cache hit, or error metrics.

**Fix:** Add `prometheus :9153` to the Corefile (gated by `metrics.enabled`), expose port 9153 on the deployment and service, and add an optional `ServiceMonitor` template.

### 11. No `imagePullSecrets`, `nodeSelector`, `tolerations`, `affinity`

Standard boilerplate missing from both deployments and `values.yaml`. Required for private registries and production node pools.

**Fix:** Add to `values.yaml` and wire into both deployments:

```yaml
imagePullSecrets: []
nodeSelector: {}
tolerations: []
affinity: {}
```

### 12. No ServiceAccount

Pods run under the `default` service account, inheriting any permissions bound to it.

**Fix:** Add a dedicated `ServiceAccount` with `automountServiceAccountToken: false`:

```yaml
serviceAccount:
  create: true
  name: ""
  automountServiceAccountToken: false
  annotations: {}
```

### 13. Secondary `replicas: 1` hardcoded

`deployment-secondary.yaml` has `replicas: 1` hardcoded instead of reading from values.

**Fix:** Use `{{ .Values.secondary.replicaCount | default 1 }}`.

---

## Nice to Have

### 14. Missing `NOTES.txt` and helm test templates

No post-install instructions displayed to the user. No `helm test` template for validation.

**Fix:** Add `templates/NOTES.txt` with service name, test commands, and warnings. Add a test pod under `templates/tests/` with `helm.sh/hook: test`.

### 15. `emptyDir` volumes have no `sizeLimit`

If zone files grow (large Netbox), the emptyDir can exhaust node disk.

**Fix:** Add `sizeLimit` to emptyDir volumes, configurable via `zoneStorage.sizeLimit: 256Mi`.

### 16. Sidecar readiness probe is meaningless

The sidecar's `/healthz` endpoint always returns healthy (`healthy` bool is never set to false in the Go code). The readiness probe doesn't reflect whether zones have been written.

**Fix:** Code-level fix in `cmd/sidecar/main.go` — set `healthy = true` only after the first successful zone write.

### 17. Service uses numeric `targetPort`

`service.yaml` uses `targetPort: 53` instead of named ports (`dns-udp`, `dns-tcp`). Named ports are more robust to port number changes.

### 18. No `podAnnotations` / `podLabels` passthrough

Users can't add custom annotations (Vault injection, Datadog, IAM roles, etc.).

**Fix:** Add `podAnnotations: {}` and `podLabels: {}` to values and merge them in deployment templates.

### 19. `Chart.yaml` metadata incomplete

Missing `home`, `sources`, `maintainers`, `keywords`, `icon`. The `appVersion: "1.0.0"` doesn't match the default image tag `dev`.

### 20. Sidecar image tag missing fallback

In `deployment.yaml`, the sidecar container uses `{{ .Values.sidecar.tag }}` with no `| default .Chart.AppVersion` fallback. When `sidecar.tag` is empty the image reference has a bare trailing `:` with no tag. The zone-init init container and coredns container both have the fallback correctly.

**Fix:** Change to `{{ .Values.sidecar.tag | default .Chart.AppVersion }}`.
