# E2E CI Speedup Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Cut 4–6 minutes from the e2e CI setup time by fixing probe delays, caching tool installs, and using BuildKit GHA cache for Docker image builds.

**Architecture:** Three files change — `dev/netbox-values.yaml` (probe delays), `.github/workflows/e2e.yaml` (tool caching + image build strategy). No Makefile changes: we call individual `make` targets explicitly instead of `make dev` so we can interleave the `k3d image import` after building.

**Tech Stack:** GitHub Actions, Docker BuildKit, k3d, Helm, `docker/build-push-action@v6`

**Design doc:** `docs/plans/2026-05-16-e2e-ci-speedup-design.md`

---

### Task 1: Reduce Netbox probe delays

**Files:**
- Modify: `dev/netbox-values.yaml:70,77`

**Step 1: Make the change**

In `dev/netbox-values.yaml`, change both `initialDelaySeconds` values from 120 to 30:

```yaml
customLivenessProbe:
  httpGet:
    path: /login/
    port: http
  initialDelaySeconds: 30   # was 120
  periodSeconds: 10
  timeoutSeconds: 1
  failureThreshold: 6
  successThreshold: 1

readinessProbe:
  initialDelaySeconds: 30   # was 120
  periodSeconds: 10
  failureThreshold: 6
```

The startup probe (`failureThreshold: 60`, `periodSeconds: 10`) already gives Netbox 10 minutes to boot. The 120s delays on readiness/liveness are dead time on top of that.

**Step 2: Verify the YAML is valid**

```bash
python3 -c "import yaml; yaml.safe_load(open('dev/netbox-values.yaml'))" && echo OK
```

Expected: `OK`

**Step 3: Commit**

```bash
jj describe -m "perf: reduce netbox probe initialDelaySeconds from 120s to 30s

The startup probe already provides a 10-minute boot budget.
The 120s delays on readiness/liveness were redundant dead time."
jj new
```

---

### Task 2: Replace curl-based tool installs with cached actions

**Files:**
- Modify: `.github/workflows/e2e.yaml`

**Step 1: Replace the Install step**

Replace the current `Install k3d, kubectl, and helm` step (lines 19–26) with three cached setup actions:

```yaml
      - name: Install k3d
        uses: medyagh/setup-k3d@v0.0.7

      - name: Install kubectl
        uses: azure/setup-kubectl@v4

      - name: Install helm
        uses: azure/setup-helm@v4
```

Remove the entire old step:
```yaml
      # DELETE THIS:
      - name: Install k3d, kubectl, and helm
        run: |
          curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | bash
          curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
          chmod +x kubectl
          sudo mv kubectl /usr/local/bin/
          curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

**Step 2: Validate the YAML is syntactically correct**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/e2e.yaml'))" && echo OK
```

Expected: `OK`

**Step 3: Commit**

```bash
jj describe -m "perf: replace curl tool installs with cached setup actions in e2e

Saves ~30-60s per run by using action-level caching for k3d,
kubectl, and helm instead of downloading fresh on each run."
jj new
```

---

### Task 3: Add BuildKit image builds and fix setup sequence

**Files:**
- Modify: `.github/workflows/e2e.yaml`

**Step 1: Replace `make dev` with explicit steps**

Find the current `Setup dev environment and run E2E tests` step:

```yaml
      - name: Setup dev environment and run E2E tests
        run: |
          make dev
          make dev.wait
          make test.e2e
```

Replace it — and add new steps before it — so the full block reads:

```yaml
      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Build coredns image
        uses: docker/build-push-action@v6
        with:
          context: coredns/
          file: coredns/Dockerfile
          load: true
          tags: coredns-netbox:dev
          cache-from: type=gha,scope=coredns
          cache-to: type=gha,mode=max,scope=coredns

      - name: Build sidecar image
        uses: docker/build-push-action@v6
        with:
          context: .
          file: docker/sidecar/Dockerfile
          load: true
          tags: coredns-netbox-sidecar:dev
          cache-from: type=gha,scope=sidecar
          cache-to: type=gha,mode=max,scope=sidecar

      - name: Setup dev environment and run E2E tests
        run: |
          make dev.cluster
          k3d image import coredns-netbox:dev -c coredns-netbox
          k3d image import coredns-netbox-sidecar:dev -c coredns-netbox
          make dev.netbox dev.token dev.seed dev.deploy
          make dev.wait
          make test.e2e
```

Notes:
- `load: true` keeps images in the local Docker daemon only — no push, no registry needed, no tag collision across concurrent PR runs (each job runs on an isolated ephemeral runner)
- Image tags (`coredns-netbox:dev`, `coredns-netbox-sidecar:dev`) match `COREDNS_IMAGE` and `SIDECAR_IMAGE` in the Makefile exactly
- GHA cache scopes (`coredns`, `sidecar`) intentionally match `pr-images.yaml` — both workflows warm the same cache
- `k3d image import` must run after `make dev.cluster` (cluster must exist first)

**Step 2: Validate the YAML**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/e2e.yaml'))" && echo OK
```

Expected: `OK`

**Step 3: Verify the full workflow looks correct**

Read the file and confirm step order is:
1. `actions/checkout@v4`
2. `actions/setup-go@v5`
3. `medyagh/setup-k3d@v0.0.7`
4. `azure/setup-kubectl@v4`
5. `azure/setup-helm@v4`
6. `docker/setup-buildx-action@v3`
7. `docker/build-push-action@v6` (coredns)
8. `docker/build-push-action@v6` (sidecar)
9. `Setup dev environment and run E2E tests` (run: block)
10. `Collect logs on failure` (unchanged)
11. `Upload logs as artifacts` (unchanged)

**Step 4: Commit**

```bash
jj describe -m "perf: use BuildKit GHA cache for e2e image builds

Replace 'make dev' with explicit steps that build images via
docker/build-push-action with type=gha cache. Cache scopes match
pr-images.yaml so both workflows share layer cache.

Saves ~2-4 min on warm cache runs."
jj new
```

---

## Verification

The changes can only be fully verified by a real CI run. After pushing:

1. Open a PR (or push to an existing one)
2. Watch the `e2e` job — the `Build coredns image` and `Build sidecar image` steps will be slow on the first run (cold cache), fast (~seconds) on subsequent commits to the same PR
3. Confirm Netbox comes up faster — `make dev.netbox` should complete before the old 120s deadline would have hit
4. Confirm `make test.e2e` passes
