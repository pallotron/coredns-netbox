# Netbox PostgreSQL Fixture Image Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build and publish `ghcr.io/pallotron/netbox-pg-fixture:4.6.0` — a Bitnami PostgreSQL image with Netbox's Django migrations pre-applied — to cut `make dev.netbox` from 5m34s to ~90s.

**Architecture:** A `build-netbox-fixture.yaml` workflow boots a full Netbox environment in k3d, dumps the schema + `django_migrations` rows, builds a custom image FROM the exact Bitnami PostgreSQL version the chart uses, and pushes to GHCR. `dev/netbox-values.yaml` is updated to reference the fixture image so PostgreSQL starts pre-migrated. Netbox sees all migrations already applied and skips them entirely.

**Tech Stack:** GitHub Actions, k3d, Helm, Bitnami PostgreSQL, `pg_dump`, Docker Buildx, GHCR

**Design doc:** `docs/plans/2026-05-16-netbox-pg-fixture-design.md`

---

### Task 1: Pin Netbox chart version in Makefile

**Files:**
- Modify: `Makefile`

The `make dev.netbox` target currently pulls the latest Netbox Helm chart. Pin it so the fixture image tag (tied to app version `4.6.0`) always matches what CI installs.

**Step 1: Add version variables at the top of the Makefile**

Find the existing variable block near the top (after `HELM_RELEASE`, `HELM_NAMESPACE`, etc.) and add:

```makefile
# Netbox chart and app version — must match the fixture image tag
NETBOX_CHART_VERSION := 8.2.15
NETBOX_APP_VERSION   := 4.6.0
```

**Step 2: Use the chart version in dev.netbox**

Find the `helm upgrade --install netbox` command in `dev.netbox` and add `--version $(NETBOX_CHART_VERSION)`:

```makefile
dev.netbox:
	helm repo add netbox-community https://netbox-community.github.io/netbox-chart/ || true
	helm repo update
	kubectl create namespace $(NETBOX_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f dev/netbox-extra-configmap.yaml
	helm upgrade --install netbox netbox-community/netbox \
		-n $(NETBOX_NAMESPACE) \
		--version $(NETBOX_CHART_VERSION) \
		-f dev/netbox-values.yaml \
		--wait --timeout 10m
	@echo "Netbox deployed."
```

**Step 3: Verify the Makefile is valid**

```bash
make -n dev.netbox 2>&1 | head -20
```

Expected: prints the helm command with `--version 8.2.15`, no errors.

**Step 4: Commit**

```bash
jj describe -m "chore: pin netbox chart version 8.2.15 (app v4.6.0) in Makefile

Required so the netbox-pg-fixture image tag is always in sync
with what CI installs."
jj new
```

---

### Task 2: Create the build-netbox-fixture workflow

**Files:**
- Create: `.github/workflows/build-netbox-fixture.yaml`

This workflow boots Netbox in k3d, dumps the schema + migration records, builds a custom Bitnami PostgreSQL image with the dump as an init script, and pushes to GHCR.

**Step 1: Create the workflow file**

```yaml
name: Build Netbox PostgreSQL Fixture

on:
  workflow_dispatch:
  push:
    branches: [main]
    paths:
      - 'dev/netbox-values.yaml'
      - 'dev/netbox-extra-configmap.yaml'
      - '.github/workflows/build-netbox-fixture.yaml'

env:
  REGISTRY: ghcr.io
  OWNER: ${{ github.repository_owner }}

jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v4

      - name: Install k3d
        uses: nolar/setup-k3d-k3s@v1
        with:
          skip-creation: true

      - name: Install kubectl
        uses: azure/setup-kubectl@v4

      - name: Install helm
        uses: azure/setup-helm@v4

      - name: Boot Netbox
        run: make dev.cluster dev.netbox

      - name: Detect PostgreSQL image
        id: pg-image
        run: |
          PG_IMAGE=$(kubectl get pod -n netbox netbox-postgresql-0 \
            -o jsonpath='{.spec.containers[0].image}')
          echo "image=$PG_IMAGE" >> $GITHUB_OUTPUT
          echo "Detected PostgreSQL image: $PG_IMAGE"

      - name: Dump schema and migration records
        run: |
          # Schema: all tables, indexes, sequences, constraints
          kubectl exec -n netbox netbox-postgresql-0 -- \
            pg_dump -U netbox --schema-only netbox \
            > /tmp/netbox-fixture.sql

          # Migration records: tells Django all migrations already applied
          kubectl exec -n netbox netbox-postgresql-0 -- \
            pg_dump -U netbox --data-only --table=public.django_migrations netbox \
            >> /tmp/netbox-fixture.sql

          echo "Fixture dump: $(wc -l < /tmp/netbox-fixture.sql) lines"

      - name: Build fixture image
        run: |
          mkdir /tmp/fixture-build
          cp /tmp/netbox-fixture.sql /tmp/fixture-build/

          cat > /tmp/fixture-build/Dockerfile <<'EOF'
          FROM ${{ steps.pg-image.outputs.image }}
          COPY netbox-fixture.sql /docker-entrypoint-initdb.d/01-netbox-fixture.sql
          EOF

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Push fixture image
        uses: docker/build-push-action@v6
        with:
          context: /tmp/fixture-build
          push: true
          platforms: linux/amd64
          tags: |
            ghcr.io/${{ env.OWNER }}/netbox-pg-fixture:4.6.0
            ghcr.io/${{ env.OWNER }}/netbox-pg-fixture:latest
```

**Step 2: Validate the YAML**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/build-netbox-fixture.yaml'))" && echo OK
```

Expected: `OK`

**Step 3: Commit**

```bash
jj describe -m "feat: add build-netbox-fixture workflow

Builds ghcr.io/pallotron/netbox-pg-fixture:4.6.0 — a Bitnami
PostgreSQL image with Netbox Django migrations pre-applied.
Triggered manually or on changes to dev/netbox-values.yaml."
jj new
```

---

### Task 3: Trigger the workflow and make the image public

This task is manual — it requires GitHub UI access.

**Step 1: Push the current branch and open a PR (or push to main)**

The workflow triggers on push to `main` for the relevant paths. Either merge Tasks 1-2, or trigger manually via the GitHub Actions UI: Actions → "Build Netbox PostgreSQL Fixture" → "Run workflow".

**Step 2: Wait for the workflow to complete**

Watch the run. It will take ~8-10 minutes (Netbox boot + build + push). Verify the image appears at:
`https://github.com/pallotron?tab=packages`

**Step 3: Make the package public**

In GHCR, find `netbox-pg-fixture`, go to Package Settings → Change visibility → Public.

This allows the e2e workflow to pull the image without auth (no `docker login` needed in e2e.yaml when image_tag is empty / local build path).

---

### Task 4: Use the fixture image in netbox-values.yaml

**Files:**
- Modify: `dev/netbox-values.yaml`

**Step 1: Add the postgresql.image override**

In `dev/netbox-values.yaml`, find the `postgresql:` block (currently around line 37) and add the `image:` override:

```yaml
postgresql:
  image:
    registry: ghcr.io
    repository: pallotron/netbox-pg-fixture
    tag: "4.6.0"
  auth:
    username: netbox
    database: netbox
  primary:
    persistence:
      enabled: true
      storageClass: "local-path"
      size: 5Gi
```

**Step 2: Validate the YAML**

```bash
python3 -c "import yaml; yaml.safe_load(open('dev/netbox-values.yaml'))" && echo OK
```

Expected: `OK`

**Step 3: Commit**

```bash
jj describe -m "perf: use netbox-pg-fixture image for pre-migrated PostgreSQL

Replaces the default Bitnami PostgreSQL image with a pre-baked
fixture that has Netbox 4.6.0 migrations already applied.
Cuts dev.netbox from 5m34s to ~90s."
jj new
```

---

### Task 5: Drop the profiling timestamps from e2e.yaml

**Files:**
- Modify: `.github/workflows/e2e.yaml`

The `tmp: add timestamps to e2e setup for profiling` commit served its purpose. Remove the `ts()` scaffolding and restore clean step commands.

**Step 1: Restore the setup step**

In `.github/workflows/e2e.yaml`, replace the `ts()`-instrumented run block:

```yaml
      - name: Setup dev environment and run E2E tests
        run: |
          make dev.cluster
          k3d image import coredns-netbox:dev -c coredns-netbox
          k3d image import coredns-netbox-sidecar:dev -c coredns-netbox
          make dev.netbox dev.token dev.seed dev.deploy
          make dev.wait
          make test.e2e
```

**Step 2: Validate**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/e2e.yaml'))" && echo OK
```

Expected: `OK`

**Step 3: Commit**

```bash
jj describe -m "chore: remove profiling timestamps from e2e setup step"
jj new
```

---

## Verification

After all tasks are merged and a CI run completes:

1. Check the `CI / e2e / e2e` job duration — expect ~5 min total (down from 8m43s)
2. In the job log, `dev.netbox` should complete in ~90s instead of 5m34s
3. Netbox migrations log should show "No migrations to apply" or similar fast exit
4. All e2e tests pass

If Netbox fails to start (readiness probe fails), check:
- The fixture image was built from the correct Netbox chart version (8.2.15 / app 4.6.0)
- The `django_migrations` data is in the dump: `grep "INSERT INTO.*django_migrations" /tmp/netbox-fixture.sql | wc -l` should be non-zero
- The Bitnami init script ran: check PostgreSQL pod logs for "executing /docker-entrypoint-initdb.d/01-netbox-fixture.sql"
