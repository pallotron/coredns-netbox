# Design: Speed Up E2E CI Setup (Issue #31)

## Problem

The `e2e` workflow takes 10–15 minutes before tests start, threatening the 45-minute job timeout. Four bottlenecks:

1. Docker images are rebuilt from scratch every run (3–5 min) — no BuildKit cache
2. k3d, kubectl, and helm are re-downloaded via `curl` every run (~30–60 sec)
3. Netbox readiness/liveness probes have `initialDelaySeconds: 120` (90 sec of dead time)
4. The `pr-images` workflow already builds these images with GHA cache — e2e ignores it

## Approach: Single-job with BuildKit GHA cache

Keep everything in one job (same runner). Replace the raw `docker build` calls in the Makefile flow with `docker/build-push-action@v6` using `load: true` and `type=gha` cache. Use cached setup actions for tools.

No cross-job image sharing is needed — each GitHub Actions runner is an ephemeral, isolated VM with its own Docker daemon. The `:dev` tag used locally cannot collide across concurrent PR runs.

The GHA cache scopes (`coredns`, `sidecar`) intentionally match those already used in `pr-images.yaml`, so both workflows share the same BuildKit layer cache and warm each other up.

## Changes

### 1. `dev/netbox-values.yaml`

Reduce `initialDelaySeconds` from 120 to 30 on both probes:

- `readinessProbe.initialDelaySeconds`: 120 → 30
- `customLivenessProbe.initialDelaySeconds`: 120 → 30

The startup probe (`failureThreshold: 60`, `periodSeconds: 10`) already provides a 10-minute budget for slow first boots, making the 120-second delays on readiness/liveness redundant.

### 2. `.github/workflows/e2e.yaml`

Replace the `curl`-based tool install step and `make dev` with explicit steps:

**Tool installs** (replace curl with cached actions):
- `medyagh/setup-k3d@v0.0.7`
- `azure/setup-kubectl@v4`
- `azure/setup-helm@v4`

**Image builds** (add before cluster setup):
- `docker/setup-buildx-action@v3` — required for GHA cache backend
- `docker/build-push-action@v6` for coredns image: `load: true`, `tags: coredns-netbox:dev`, `cache-from/to: type=gha,scope=coredns`
- `docker/build-push-action@v6` for sidecar image: `load: true`, `tags: coredns-netbox-sidecar:dev`, `cache-from/to: type=gha,scope=sidecar`

**Dev environment setup** (replace `make dev` with explicit targets):
```
make dev.cluster
k3d image import coredns-netbox:dev -c coredns-netbox
k3d image import coredns-netbox-sidecar:dev -c coredns-netbox
make dev.netbox dev.token dev.seed dev.deploy
make dev.wait
make test.e2e
```

`k3d image import` must run after `make dev.cluster` (cluster must exist). Image tags match `COREDNS_IMAGE` and `SIDECAR_IMAGE` in the Makefile — no Makefile changes needed.

### 3. No Makefile changes

Calling individual make targets explicitly (`dev.cluster`, `dev.netbox`, etc.) is sufficient. No new targets needed.

## Expected Savings

| Fix | Savings |
|-----|---------|
| Cached tool installs | ~30–60 sec |
| Probe delays 120 → 30s | ~90 sec |
| BuildKit GHA layer cache (warm) | ~2–4 min |
| **Total (warm cache)** | **~4–6 min** |

First run on a new branch will be slower on the build step (cold cache), but still benefits from tool caching and reduced probe delays. Subsequent commits to the same PR will be fast.
