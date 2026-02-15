# Helm Chart Production Readiness

Findings from reviewing `helm/coredns-netbox/` for production use.

## Critical

### 1. Primary service selector matches secondary pods

`service.yaml` uses `selectorLabels` (only `name` + `instance`) without a `component` label. This means DNS traffic routed through the primary Service can hit secondary pods too.

**Fix:** Add `app.kubernetes.io/component: primary` to the primary deployment's pod labels and service selector. The secondary already has `component: secondary`.

### 2. No `securityContext` anywhere

All containers (CoreDNS, sidecar, init containers, busybox) run as root with full capabilities.

**Fix:** Add pod-level and container-level security contexts:

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

### 4. `transfer { to * }` allows unrestricted AXFR

`configmap.yaml` line 14-16: any host can initiate a zone transfer from the primary.

**Fix:** Make transfer targets configurable via values:

```yaml
transfer:
  targets: []  # empty = disabled, ["*"] = unrestricted, ["10.0.0.1"] = specific
```

### 5. No resource limits on init containers or secondary

`zone-init` init container (`deployment.yaml`), `resolve-primary` init container, and the secondary CoreDNS container all lack `resources:` blocks.

**Fix:** Add resource values:

```yaml
resources:
  zoneInit:
    requests: { cpu: 50m, memory: 32Mi }
    limits: { cpu: 100m, memory: 64Mi }
secondary:
  resources:
    requests: { cpu: 100m, memory: 64Mi }
    limits: { cpu: 200m, memory: 128Mi }
```

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

### 10. Busybox init container hardcoded and fragile

`deployment-secondary.yaml` hardcodes `busybox:1.36`. This:
- Cannot be overridden (breaks air-gapped / private registry environments)
- Uses `nslookup` + `awk` parsing that varies across busybox versions
- Has no timeout on the retry loop (blocks pod startup forever if primary never resolves)

**Fix:**
- Make the image configurable via `secondary.initImage.{image,tag,pullPolicy}`
- Add a timeout to the retry loop
- Investigate whether `secondary { transfer from <FQDN>:53 }` actually works (the earlier error was for zone `.`, not for a hostname — re-test with the service FQDN directly to potentially eliminate the init container entirely)

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
