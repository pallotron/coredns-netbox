# Collapsing Per-DC Zones into a Single Zone

## Background: a naming convention mistake

The ideal DNS naming convention for infrastructure hosts is to encode all
structural information as DNS labels:

```
hv-01.r166.m12.dc-m.example.org
```

With this layout the full hierarchy is navigable via DNS, and clients only need
a single search domain (`example.org`). Short-name resolution works at any
level of specificity:

```
search example.org

# All of these resolve correctly:
hv-01.r166.m12.dc-m     → hv-01.r166.m12.dc-m.example.org
hv-01.r166.m12.dc-m.example.org
```

A common mistake is to flatten the structural part into the hostname label
instead of using separate labels:

```
dc-m12-r166-hv-01.dc-m.example.org   ← hostname embeds dc-m, rack, and row
```

Now the DC (`dc-m`) appears twice: once inside the hostname string and again as
a DNS subdomain. To resolve `dc-m12-r166-hv-01` by short name, clients need
`dc-m.example.org` in their search list — and with many DCs they need one entry
per DC:

```
search dc-m.example.org dc-n.example.org dc-o.example.org ...
```

Most resolvers cap the number of search domains at **6**: macOS (mDNSResponder),
musl libc (Alpine Linux), all BSDs, and Linux with glibc older than 2.26. With
more than 6 DCs, short-name resolution silently breaks for hosts in zone 7+.

> **Resolver limits at a glance**
> | Platform | Limit |
> |---|---|
> | macOS (mDNSResponder) | 6 domains, 256 chars total |
> | musl libc (Alpine Linux) | 6 domains |
> | BSD (Free/Open/Net) | 6 domains |
> | Linux glibc < 2.26 | 6 domains |
> | Linux glibc ≥ 2.26 | unlimited |
> | Windows | 50 domains |

## The Fix

The right long-term fix is to rename hosts to a proper label-per-level
convention. `stripDCLabel` exists as an operational workaround for environments
that cannot rename hosts immediately.

It strips the DC label — the label immediately before `domainSuffix` — from
every record's DNS name before zone discovery:

```
dc-m12-r166-hv-01.dc-m.example.org  →  dc-m12-r166-hv-01.example.org
dc-n34-r042-hv-02.dc-n.example.org  →  dc-n34-r042-hv-02.example.org
```

All records land in a single `example.org` zone. Clients need only one search
domain regardless of how many DCs exist. The DC is still identifiable from the
hostname prefix — no information is lost, it was already redundant.

## Configuration

```yaml
# Helm values
domainSuffix: example.org   # base domain — DC label sits just before this
stripDCLabel: true

zoneDiscovery:
  depth: 2                  # after stripping, names are host.example.org → depth 2
```

Or via environment variables:

```bash
DOMAIN_SUFFIX=example.org
STRIP_DC_LABEL=true
ZONE_DEPTH=2
```

## What Changes

| | Before (`stripDCLabel: false`) | After (`stripDCLabel: true`) |
|---|---|---|
| **Zones** | `dc-m.example.org`, `dc-n.example.org`, … | `example.org` |
| **zone-depth setting** | `depth: 3` (to match `host.dc.example.org`) | `depth: 2` |
| **Search domains needed** | One per DC | One total |
| **Secondary zones** | One entry per DC | One entry |

## Secondary DNS

Update the secondary's zone list to the single collapsed zone:

```yaml
secondary:
  enabled: true
  zones:
    - example.org        # was: dc-m.example.org, dc-n.example.org, ...
    - 10.in-addr.arpa
  transferFrom:
    - <primary-clusterip>
```

## Behaviour Details

- Records **not under `domainSuffix`** are left unchanged.
- Records already at `hostname.domain` depth (no DC label present) are left
  unchanged.
- Multi-label hostnames work correctly: `host.role.dc.example.org` →
  `host.role.example.org` (only the label immediately before `domainSuffix` is
  stripped).
- Reverse (PTR) zones are unaffected — they are derived from IP addresses, not
  DNS names.
