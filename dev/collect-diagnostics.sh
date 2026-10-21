#!/bin/sh
# Collect everything needed to explain an e2e failure after the fact.
#
# Run by CI when the e2e job fails, and usable by hand against a local k3d
# cluster. Writes one file per source into $1 (default ./e2e-diag) and
# prints a short summary; the bulky files are for the uploaded artifact,
# not the console.
#
# The bias here is toward things that are gone once the cluster is torn
# down and that cannot be reconstructed from the Go test output:
# timestamps, and the sidecar's own view of what it was doing.
set -eu

OUT="${1:-./e2e-diag}"
NS="${HELM_NAMESPACE:-coredns-netbox}"
NETBOX_NS="${NETBOX_NAMESPACE:-netbox}"
mkdir -p "$OUT"

# --timestamps on everything: the whole point is correlating a test's own
# timings against what the sidecar and CoreDNS were doing at that moment.
# Without them a log is just a list of events in an unknown order relative
# to the failure.
logs() { # <file> <namespace> <kubectl logs args...>
  f="$1"; ns="$2"; shift 2
  # Flags last: putting them before the caller's args would wedge
  # "-l <selector>" apart and kubectl would read the flag as the selector.
  kubectl -n "$ns" logs "$@" --timestamps --tail=-1 >"$OUT/$f" 2>&1 || true
}

# The sidecar is where zone generation, polling and the shrink guard live,
# so it explains most failures. Previous container too: a restart (OOM,
# crash) is invisible in the current one.
logs sidecar.log           "$NS" -l app.kubernetes.io/component=sidecar --all-containers
logs sidecar-previous.log  "$NS" -l app.kubernetes.io/component=sidecar --all-containers --previous
logs primary.log           "$NS" -l app.kubernetes.io/component=primary --all-containers
logs primary-previous.log  "$NS" -l app.kubernetes.io/component=primary --all-containers --previous
logs secondary.log         "$NS" -l app.kubernetes.io/component=secondary --all-containers
logs netbox.log            "$NETBOX_NS" deploy/netbox -c netbox

# A metrics scrape says what the logs do not: how long poll cycles are
# taking, whether the shrink guard is blocking, how stale the zones are.
# Ordering matters for triage, so keep the whole scrape.
kubectl -n "$NS" exec deploy/coredns-netbox-sidecar -- \
  wget -qO- http://127.0.0.1:8082/metrics >"$OUT/sidecar-metrics.txt" 2>&1 \
  || curl -fsS http://127.0.0.1:18082/metrics >"$OUT/sidecar-metrics.txt" 2>&1 \
  || echo "metrics unavailable" >"$OUT/sidecar-metrics.txt"

# Events catch what no container log does: OOMKills, failed scheduling,
# image pull errors, probe failures.
kubectl get events -A --sort-by=.lastTimestamp >"$OUT/events.txt" 2>&1 || true

{
  kubectl -n "$NS" get pods -o wide
  echo
  kubectl -n "$NS" describe pods
  echo
  kubectl -n "$NETBOX_NS" get pods -o wide
} >"$OUT/pods.txt" 2>&1 || true

# The rendered Corefile: several past failures were a config that did not
# match what the tests assumed (reload interval, transfer peers, rewrite).
kubectl -n "$NS" get cm coredns-netbox -o yaml >"$OUT/configmap.yaml" 2>&1 || true

kubectl cluster-info dump --output-directory="$OUT/cluster-dump" >/dev/null 2>&1 || true

# Console summary: only what a human reads first.
echo "=== pods ==="
kubectl -n "$NS" get pods -o wide 2>&1 || true
echo
echo "=== sidecar, last 60 lines ==="
tail -60 "$OUT/sidecar.log" 2>/dev/null || echo "(none)"
echo
echo "=== poll / guard / staleness ==="
grep -E '^netbox_sidecar_(poll_total|poll_cycle_duration_seconds_(sum|count)|zone_staleness_seconds|shrink_guard_blocked|zone_write_errors_total|netbox_records_fetched)' \
  "$OUT/sidecar-metrics.txt" 2>/dev/null || echo "(no metrics)"
echo
echo "Full diagnostics in $OUT (uploaded as an artifact in CI)."
