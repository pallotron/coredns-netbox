# Design: netbox-pg-fixture Docker Image

## Problem

`make dev.netbox` takes **5m34s** — 64% of total e2e CI time. The breakdown:
- PostgreSQL init: ~30-60s (Bitnami initializes data directory, creates DB/user)
- Django migrations: ~3-4 min (`manage.py migrate` runs hundreds of SQL migrations against a blank schema)
- Gunicorn startup + readiness probe: ~30-60s

The migrations are the dominant cost and are entirely redundant on every CI run — the schema never changes between runs of the same Netbox version.

## Solution

Build a custom Docker image based on the exact Bitnami PostgreSQL image used by the Netbox Helm chart, with a pre-applied SQL dump baked in as an init script. Bitnami PostgreSQL runs files in `/docker-entrypoint-initdb.d/` on first boot when the data directory is empty — after the chart creates the `netbox` database and user. Our init script creates all tables (schema) and inserts the `django_migrations` rows (marking all migrations as already applied).

When Netbox starts against this pre-baked database:
1. PostgreSQL: loads init script (~10-20s) instead of waiting for Netbox to run migrations
2. `manage.py migrate`: finds all migrations already recorded → exits in ~2s
3. Gunicorn starts + readiness probe passes: ~60s
4. **Total: ~90s** instead of 5m34s → saves ~4 minutes

`dev.token` and `dev.seed` still run at runtime (7s combined — not worth pre-baking).

## What the dump contains

Two parts, concatenated into one SQL file (`/docker-entrypoint-initdb.d/01-netbox-fixture.sql`):

1. **Schema only** (`pg_dump --schema-only`): all tables, indexes, sequences, constraints, functions from the fully-migrated Netbox database. No user data, no IP addresses, no tokens.

2. **`django_migrations` rows** (`pg_dump --data-only --table=public.django_migrations`): the records that tell Django which migrations have been applied. Without these rows, Django would see a schema but assume no migrations ran, and attempt to re-run them all.

`pg_dump` does not include `CREATE DATABASE` or `\connect` — the dump is a clean schema+data script that runs against the existing `netbox` database created by Bitnami's init process.

## Image naming and versioning

**Name:** `ghcr.io/pallotron/netbox-pg-fixture`  
**Tag:** Netbox app version (e.g. `4.6.0`) — the app version determines which migrations are baked in, not the chart version.  
**Also tagged:** `latest`

The image is **public** on GHCR — no auth required to pull. Anyone testing a Netbox integration can use it.

**When to rebuild:** When the Netbox app version changes (new migrations). Triggered by:
- `workflow_dispatch` (manual)
- Push to `main` that modifies `dev/netbox-values.yaml` or the build workflow itself

## Components

### 1. `Makefile` — pin chart version

`make dev.netbox` currently uses the latest chart. Pin it so the fixture tag is always correct and CI is reproducible:

```makefile
NETBOX_CHART_VERSION := 8.2.15
NETBOX_APP_VERSION   := 4.6.0
```

Use `--version $(NETBOX_CHART_VERSION)` in the `helm upgrade --install` command.

### 2. `.github/workflows/build-netbox-fixture.yaml` — new workflow

Triggered manually or on relevant file changes:
1. Install k3d, kubectl, helm
2. `make dev.cluster dev.netbox` (full boot — waits for Netbox ready)
3. Detect the exact Bitnami PostgreSQL image from the running pod
4. Dump schema + `django_migrations` rows via `kubectl exec`
5. Write a Dockerfile `FROM <detected-pg-image>` with the dump as init script
6. Build + push to GHCR as `netbox-pg-fixture:4.6.0` and `netbox-pg-fixture:latest`

### 3. `dev/netbox-values.yaml` — use fixture image

```yaml
postgresql:
  image:
    registry: ghcr.io
    repository: pallotron/netbox-pg-fixture
    tag: "4.6.0"
```

### 4. Remove profiling timestamps from `e2e.yaml`

The `tmp: add timestamps` commit is squashed/dropped before merge.

## Sequencing

The fixture image must exist in GHCR before `netbox-values.yaml` can reference it. Workflow:
1. Merge the build workflow + Makefile pin (without the values.yaml change yet)
2. Trigger `build-netbox-fixture` manually → image published to GHCR
3. Add the `postgresql.image` override to `netbox-values.yaml` in a follow-up commit
4. CI runs with the fixture image → verify timing

## Expected outcome

| Step | Before | After |
|------|--------|-------|
| dev.netbox | 5m34s | ~90s |
| **Total e2e** | **8m43s** | **~5m** |
| **Total CI** | **9m42s** | **~6m** |
