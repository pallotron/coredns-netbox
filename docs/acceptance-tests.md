# Acceptance tests and failure modes

This is the white-box test plan for the whole path this project owns:

```
NetBox -> sidecar (zone generation) -> CoreDNS primary -> resolver
                                           |
                                           +-- AXFR/NOTIFY -> downstream secondaries
```

Every row below is a behaviour the component is expected to have, together
with the test that proves it. The point of writing them down was never the
list itself: working through it surfaced twelve real defects, each recorded
in the gap table and pinned by a named test so it cannot come back.

## How to read a row

| Mark | Meaning |
|---|---|
| ✅ | Asserted by the named test, runs on every CI build |
| 🟡 | Some legs covered, others open |
| ⬜ | Proposed, not yet written |

**Where** a row runs:

- **k3d CI** — the GitHub e2e pipeline: a k3d cluster, a real NetBox and
  Postgres, ~18k seeded IPs plus device fixtures, two CoreDNS primary
  replicas, the sidecar with its PVC, a live AXFR secondary, the gRPC
  control plane, the HMAC webhook, and metrics. Failure injection is done
  with `kubectl scale`/`delete`/`exec` and NetBox's Django ORM.
- **unit** — plain `go test`.

Failure-mode rows run in a separate `make test.e2e.chaos` pass
(`TestChaos_*` / `TestMutate_*`, gated by `CHAOS_TESTS=true`) so the fast
suite stays fast. Both passes run in CI.

Fixtures use `mycompany.com` and `server*-{mgmt,bmc,storage}.dc{1..3}`.
No real hostnames or addresses appear in test code.

---

## Gaps found by writing this plan

Defects surfaced by writing and running the matrix. Each is pinned by the
row named at the end, so the fix stays fixed.

| | Gap | Resolution |
|---|---|---|
| **G1** | An empty or implausibly small NetBox response could replace the published zones. Downstream, the wipe advances the serial and fans out to every secondary. | Refuse-on-empty (#73), then a shrink guard (#87) refusing a drop of more than 5% **and** 100 records globally, or 50% **and** 100 in one zone, cleared by `ForceNetboxPoll{allow_shrink:true}`. Alert on `netbox_sidecar_shrink_guard_blocked`. Pinned by **N20/N21**. |
| **G2** | The `YYYYMMDDNN` SOA serial went backwards on the 100th change of a UTC day, so secondaries ignored every later update. | Fixed (#71). Pinned by **N10**. |
| **G3** | `ns1.<zone>` was named in SOA/NS but had no address record, so it resolved nowhere. | Fixed (#75) — optional `PRIMARY_NS_IP` / helm `primaryNSIP`. Pinned by **N15**. |
| **G4** | A sidecar restart during a NetBox outage wiped the PVC zone cache, because the first merge ran with no fetched data. | Fixed (#77) — no merge until a fetch has succeeded, or there is nothing on disk to destroy. Pinned by **N16**. |
| **G5** | Every sidecar restart rewrote all zones with serial+1, fanning a full AXFR re-transfer out to every secondary. | Fixed (#82) — the content fingerprint is persisted in the zone file and verified on startup. Pinned by **N22**. |
| **G6** | `zone_staleness_seconds` could never fire during a NetBox outage: merging the previous zones still counted as success and reset it. | Fixed (#76) — only a successful fetch resets staleness. Pinned by **N17**. |
| **G7** | The webhook fast path ignored IP status, so a reserved or deprecated address could be published even though the poll path filtered it out. | Fixed (#80) — one publish policy for both paths (`NETBOX_PUBLISH_STATUSES`, default `active`). Pinned by **N1**. |
| **G8** | A single invalid `dns_name` — reachable through ORM writes, which skip API validation — made the rendered zone unparseable and froze every zone update. | Fixed (#81) — zonegen drops unsafe names at the single render choke point. Pinned by **N6**. |
| **G9** | `poll_duration_seconds` measured only merge+write while its help text claimed the whole poll, hiding the fetch that dominates the cycle. | Fixed (#83) — new `netbox_sidecar_poll_cycle_duration_seconds`. Pinned by **N26**. |
| **G10** | A damaged zone file was trusted instead of repaired — a regression introduced by the G5 fix, since a truncated file kept its valid header. | Fixed (#84) — a body hash over the file's own bytes. Pinned by **N25**. |
| **G11** | Every rolling restart dropped queries (4 of 354 measured): no lameduck, so a pod stopped serving before its endpoint was removed. | Fixed (#85) — health now carries `lameduck 5s`. Pinned by **N24**. |
| **G12** | IPv6 reverse names had swapped nibble pairs, so generated `ip6.arpa` names were ones no resolver would ask for — and a correctly configured zone matched nothing. IPv6 reverse could never have worked. | Fixed (#93, in review) — nibbles are emitted high-then-low per byte. Pinned by **N7**. |

Two notes on G1, because the thresholds are the interesting part. A
percentage trigger with an absolute floor was chosen over a fixed count:
churn scales with the size of the deployment, so a count tuned for today
starts firing on routine churn as the estate grows and eventually gets
switched off, whereas a percentage needs no maintenance. The floor exists
because percentages are meaningless on a handful of records. The defaults
were set against a month of telemetry from a production-scale deployment,
where the largest genuine shrink in any window stayed well under the
trigger — see `docs/scale.md` for how to take that measurement on your own
deployment before changing them.

---

## N1x — NetBox data to records

| ID | Test | What it must do | How it's exercised | Where | Status |
|---|---|---|---|---|---|
| **N1** | Only `status=active` IPs published | Active IPs become A/PTR; reserved, deprecated and dhcp never appear | ORM-create one IP per status; force a poll; dig each | k3d CI + unit | ✅ `TestMutate_N1_OnlyActivePublished` — only the active IP serves A+PTR, asserted per replica. Policy configurable since #80 (`publishStatuses`), default active-only |
| **N2** | Interface categorization drives naming | Management-pattern interface produces the canonical record, BMC-pattern the BMC name, an unmatched interface produces nothing | Add IPs on `mgmt0`, `bmc0`, `swp1`; poll; dig | k3d CI + unit | ✅ `TestDeviceCanonicalRecords` (mgmt + BMC) · `TestUnmatchedInterfaceYieldsNoRecord` — an uncategorized interface yields no A and no PTR |
| **N3** | Name templates: canonical + aliases, parser precedence | First-match-wins parsers; the canonical holds the A, each alias is a CNAME to it; catch-all devices get a flat canonical and no aliases | Seed devices plus one catch-all device | k3d CI | ✅ `TestDeviceCanonicalRecords`, `TestCNAMEAliasLookup` · `TestCatchAllDeviceGetsFlatCanonicalWithoutAliases` |
| **N4** | Plain `dns_name` passthrough | A manual `dns_name` resolves regardless of device assignment | Seeded fixture | k3d CI | ✅ `TestARecordLookup`, `TestMultipleHosts`, `TestPTRRecordLookup` |
| **N5** | CNAME collision policy (RFC 1034) | Alias versus manual A: the CNAME is dropped and the A wins. Same alias, different targets: all dropped. Every drop is ERROR-logged and the zone always parses | ORM-create a `dns_name` equal to a known alias; two devices with colliding aliases; poll | unit | ✅ `TestResolveCNAMECollisions`, `TestGenerateDropsCollidingCNAME` |
| **N6** | Data hygiene | Garbage `dns_name` values never corrupt the zone: only A/AAAA/PTR/CNAME are emitted, offenders dropped and logged | ORM-insert hostile `dns_name` values; poll; AXFR and parse | k3d CI + unit | ✅ `TestFormatRejectsInvalidRenderedNames` (unit) · `TestMutate_N6_HostileDNSNames` + `TestGenerateDropsUnsafeDNSNames`. Found **G8** |
| **N7** | IPv6 forward and reverse | An active IPv6 assignment yields an AAAA record, and its PTR answers under the RFC 3596 nibble name | ORM-create an IPv6 IP with a `dns_name`; poll; dig AAAA and `dig -x` | k3d CI + unit | ✅ `TestIPv6ForwardAAAA` · `TestIPv6ReversePTR` (builds the query with `dns.ReverseAddr`) · unit conformance against the reference encoder (#93, in review). Found **G12** |

## N2x — Zone generation and publication

| ID | Test | What it must do | How it's exercised | Where | Status |
|---|---|---|---|---|---|
| **N8** | Zone discovery (depth-2) | Names group into depth-2 zones; dotted owners are labels, not new zones | Seeded fixture | k3d CI | ✅ `TestMultipleHosts`, `TestMetricsZonesActive`; unit `TestZoneDepthDiscoverer_*` |
| **N9** | Reverse zones; PTR targets canonical only | Every A in an enabled reverse range gets a PTR targeting the canonical, never an alias | Seeded fixture | k3d CI | ✅ `TestPTRRecordLookup`, `TestPTRTargetsCanonicalOnly`, `TestDynamicRecordGetsPTR` |
| **N10** | SOA serial monotonic across 100+ same-day writes | The serial strictly increases at any realistic change rate | 110 upserts, recording the SOA serial after each | unit + k3d CI | 🟡 `TestNextSerialMonotonicAcross100SameDayWrites` (unit); G2 fix merged (#71). The e2e leg (110 live upserts plus a secondary transfer) is not written — the unit pin covers the arithmetic, which is where the bug was |
| **N11** | Propagation: create | A new NetBox IP is resolvable within one poll plus reload, and near-instantly through the webhook | Create through ORM/API; time both paths | k3d CI | ✅ webhook path `TestWebhook_CreatedEventUpdatesDNSWithoutPoll`; poll path `TestMutate_N11_PollPathCreateTiming`, which logs create-to-resolvable against its budget on every CI run |
| **N12** | Propagation: change and delete | Reassignment updates A+PTR; deletion yields NXDOMAIN after the next poll; nothing stale survives | ORM-modify then delete; poll; dig past the TTL | k3d CI | ✅ `TestMutate_N12_ChangeAndDelete` — reassignment updates A+PTR, deletion gives NXDOMAIN, and the absence of stale answers is asserted on each replica individually |
| **N13** | Batch change | A 1,000-record bulk change lands in one poll: all present, a single serial bump | ORM `bulk_create` of 1,000; poll; AXFR-diff against intent | k3d CI | ✅ `TestMutate_N13_BatchChange` — 1000/1000 via AXFR and exactly one serial bump |
| **N14** | Reload reaches every replica | Both CoreDNS replicas serve new data consistently after a write | Covered by existing rows | k3d CI | ✅ `TestMultipleReplicasConsistentDNS`, `TestEachReplicaServesCorrectly`, `TestZoneReloadService` |
| **N15** | `ns1.<zone>` resolves | The SOA/NS name has an A record | `dig ns1.mycompany.com` | unit + k3d CI | ✅ unit `TestGeneratePrimaryNSAddressRecord`, e2e `TestPrimaryNSHasARecord`; **G3** fixed via `primaryNSIP` (#75) |

## N3x — Failure modes: NetBox side

| ID | Test | What it must do | How it's exercised | Where | Status |
|---|---|---|---|---|---|
| **N16** | NetBox down at sidecar startup | The replacement sidecar comes up Running (NotReady until NetBox returns, by design), never destroys the cached zones, and DNS answers throughout | Scale NetBox to 0; delete the sidecar pod; dig continuously | k3d CI | ✅ `TestChaos_N16_NetboxDownAtSidecarStartup`; **G4** fixed (#77) — before the fix the restart wiped the PVC cache |
| **N17** | NetBox down mid-run: serve stale, self-recover | Polls fail while answers keep serving; `zone_staleness_seconds` climbs and `poll_total{result="error"}` increments; the next poll after recovery resets staleness with no restart | Scale NetBox to 0 for two or more poll intervals; dig and scrape throughout; scale back up | k3d CI | ✅ `TestChaos_N17_NetboxDownMidRun`; found and fixed **G6** (#76) — staleness used to reset on every merge of stale data |
| **N18** | Partial fetch fails closed | Any page error discards the whole poll, so the zone is never built from a truncated set | `httptest` server failing `offset>0` requests | unit | ✅ `TestFetchIPAddresses_PartialPageFailureFailsClosed` (#72) — load-bearing for G1's blast radius |
| **N19** | API token revoked | 403s surface as poll errors, stale serving continues per N17, and recovery after restore needs no restart | ORM-delete the token; wait a poll; recreate | k3d CI | ✅ `TestChaos_N19_TokenRevoked` — poll errors counted, zero lost queries, recovery on restore without a restart |
| **N20** | Empty (HTTP 200) NetBox result | The publish is refused, the previous zone keeps serving, `empty_response_total` increments, and the refusal is logged loudly | ORM: set all IPs non-active; force a poll; dig; restore | unit + k3d CI | ✅ `TestFetchNetboxRefusesEmptyOverPublished` plus the shrinkguard table (unit) · `TestChaos_N20_EmptyResultRefusedAndUnoverridable` — zones hold, counters climb, and an override does **not** clear it |
| **N21** | Mass-delete shrink guard | A shrink beyond the threshold is refused pending an operator override | ORM-delete 90%; poll; exercise the override | unit + k3d CI | ✅ `TestChaos_N21_MassRemovalRefusedUntilOverride` (#87) — 90% removal refused with the serial unchanged, then published on override. The dev values use a looser trigger than the chart default so the fixture's own churn does not trip it |

## N4x — Failure modes: sidecar and CoreDNS

| ID | Test | What it must do | How it's exercised | Where | Status |
|---|---|---|---|---|---|
| **N22** | Sidecar pod killed | CoreDNS keeps answering, the replacement sidecar re-fetches, and the serial never regresses | Delete the sidecar pod under a continuous dig loop | k3d CI | ✅ `TestChaos_N22_SidecarKilled` — answers throughout the kill, serial never regresses, and since **G5** (#82) a restart rewrites nothing |
| **N23** | Sidecar PVC lost | The replacement regenerates all zones from NetBox within one poll cycle | Delete the PVC and pod; time to full service | k3d CI | ✅ `TestChaos_N23_SidecarPVCLost` — volume destroyed and rebuilt from NetBox in seconds. Serial continuity is file-backed, so a rebuilt volume restarts the sequence (logged, not asserted) |
| **N24** | CoreDNS rolling restart under load | With two replicas, no client-visible SERVFAIL beyond failover | `rollout restart` under a 10 q/s dig loop | k3d CI | ✅ `TestChaos_N24_RollingRestartUnderLoad` — zero failures required. Found **G11** on its first real run: the rollout dropped about 1% of queries because CoreDNS had no lameduck, so every helm upgrade had been doing this |
| **N25** | Corrupt zone file on disk | An unparseable zone is not loaded, the previous in-memory zone keeps serving, and the error surfaces in `zone_write_errors_total` and the logs | Truncate a zone file; trigger a reload | k3d CI | ✅ `TestGeneratorRepairsDamagedZoneFile` (unit, #84) plus the `make dev.chaos.corrupt-zone` drill. No e2e leg: distroless images make exec-truncation impossible. Found **G10** |
| **N26** | Production-scale headroom | At scale the cycle (fetch + generate + write) fits inside `POLL_INTERVAL` and memory stays inside the chart limits | Read the cycle, fetch and memory metrics off a deployment running at scale; guard the generation path in CI against an algorithmic regression | live telemetry + unit (CI) + drill on demand | ✅ CI guard `TestScale_DeviceDominatedPipeline`; the cycle is fetch-bound, so headroom scales with record count. `docs/scale.md` gives the method and the series to read on your own deployment |

## N5x — Serving secondaries (AXFR/NOTIFY out)

| ID | Test | What it must do | How it's exercised | Where | Status |
|---|---|---|---|---|---|
| **N27** | Full AXFR correctness | AXFR yields the complete zone including CNAMEs, parseable, matching the primary's own answers | Covered by existing rows | k3d CI | ✅ `TestAXFRTransfer`, `TestAXFRIncludesCNAME` |
| **N28** | Secondary convergence after a change | The secondary serves the new serial within the NOTIFY delay rather than waiting out the refresh timer | Force a zone write; poll the secondary's SOA; measure | k3d CI | 🟡 In review (#92). The gap was real: no NOTIFY was ever sent, so convergence was bounded by the SOA refresh. NOTIFY now ships and the refresh default is much shorter; `TestChaos_N28_SecondaryConvergesOnNotify` measures convergence in single-digit milliseconds |
| **N29** | Secondary serves through a primary outage | With the primary scaled to 0 the secondary answers existing names until SOA expiry, and recovers automatically | Scale the primary to 0 across refresh intervals; dig the secondary; scale back | k3d CI | ✅ `TestChaos_N29_SecondaryServesThroughPrimaryOutage` — answers throughout, no restart. Proves continued serving only; post-recovery transfer is N28's subject |
| **N30** | Transfer ACL enforced | A restricted `transfer.to` REFUSEs unlisted clients while listed ones still transfer | Helm-upgrade to a restricted ACL; AXFR from an unlisted pod and from the secondary | k3d CI | ✅ `TestChaos_N30_TransferACLEnforced` (#86) — an unlisted client is refused while the secondary still serves, plus a template assertion that a restricted list renders only the named peers |
| **N31** | CNAME-only zone flagged | A zone of only CNAMEs logs the "secondaries must enumerate it" warning | Craft devices whose aliases land in an A-less zone; poll | unit | ✅ `TestCNAMEOnlyZones` |

## N6x — Control plane, webhook and metrics

| ID | Test | What it must do | How it's exercised | Where | Status |
|---|---|---|---|---|---|
| **N32** | gRPC auth | A wrong or missing bearer token is rejected on every RPC | Covered by existing rows | k3d CI + unit | ✅ `TestAuthRejectsWrongToken`, `TestAuthRejectsMissingToken`; unit `TestAuthInterceptor_*` |
| **N33** | Dynamic records coexist with polling | Dynamic zones and records survive polls and merges; delete, batch and PTR all work | Covered by existing rows | k3d CI | ✅ `TestDynamicZoneCreate`, `TestDynamicRecordUpsertAndResolve`, `TestDynamicRecordDelete`, `TestBatchUpsert`, `TestDynamicRecordSurvivesNetboxPoll`, `TestDynamicRecordGetsPTR` |
| **N34** | Webhook rejects bad signatures | A missing, wrong or tampered HMAC gives 4xx, is counted, and changes no DNS | Covered (unit) | unit | ✅ `TestVerifySignature_*`, `TestHandler_InvalidSignature_Rejected` — an e2e leg would add only transport |
| **N35** | Stale or out-of-order webhook cannot regress state | An event older than current state never overwrites newer data | Covered (unit) | unit | ✅ `TestHandler_StaleEvent_Ignored`, `TestHandler_ConcurrentSameNameDeliveries_NewerEventWins`, `TestReconcileWebhookSourced_*` |
| **N36** | Webhook-triggered poll rate limit | Event bursts coalesce and full polls respect the minimum interval | Covered (unit) | unit | ✅ `TestWebhookPollAllowed` |
| **N37** | Metrics endpoint and core series | The endpoint is reachable and the poll, zone and staleness series are present and advancing | Covered by existing rows | k3d CI | ✅ `TestMetricsEndpointReachable`, `TestMetricsContainExpectedSeries`, `TestMetricsSuccessfulPoll`, `TestMetricsZonesActive` |

Per-failure-mode metric assertions are embedded in N17, N19, N20 and N25
rather than duplicated here.

---

## Out of scope

Deployment-side observability — that a `zone_staleness_seconds` alert
actually pages, and that a dashboard's reload and AXFR panels reflect real
events — depends on the monitoring stack around a given deployment, not on
this component. It is best exercised as a one-off drill by inducing N17
against a dev deployment with alerting live. Note the alert is only
meaningful after the **G6** fix: before it, the metric reset every tick.

## Harness

The rows above needed these, all now in the repo:

1. Chaos helpers in the Makefile — `dev.netbox.{down,up}`,
   `dev.chaos.kill-sidecar`, `dev.chaos.corrupt-zone`.
2. ORM mutation fixtures — `dev/mutate-ips.py` (set-status, add, del,
   bulk-add, bulk-del) plus the `netboxORM` / `forcePoll` helpers in
   `tests/e2e/mutate_test.go`, and the revoke/restore-token operations.
3. A continuous-query probe — `startProbe` / `probeStats` in
   `tests/e2e/chaos_test.go`, which reports any failure window. Used by
   N16, N17, N22, N24 and N29.
4. A restricted-transfer values variant for N30.

## What is not covered

Being explicit about this is the point of the matrix:

- **N10's e2e leg** — 110 live upserts plus a secondary transfer. The unit
  pin covers the serial arithmetic, which is where G2 actually lived; the
  e2e leg would mostly re-time the reload path that N11 already measures.
- **N25's e2e leg** — distroless images make exec-truncation impossible, so
  the corruption drill is a Makefile target rather than a test.
- **N34's e2e leg** — the unit tests cover signature verification; an e2e
  leg would add only HTTP transport.
