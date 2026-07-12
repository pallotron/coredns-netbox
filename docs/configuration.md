# Configuration

## DNS Record Generation (Hybrid Approach)

The sidecar uses a **hybrid approach** to generate DNS records from Netbox:

1. **Records with `dns_name` populated** - Preserved as-is from Netbox
   - Typically network switches/routers with manually-configured DNS names
   - Example: A switch with `dns_name="switch01.example.com"` → DNS record created directly

2. **Records without `dns_name`** - Generated from device names using interface categorization
   - Typically servers, PDUs, and infrastructure devices
   - Example: Device `dc1-r101-prod-srv-01` with mgmt interface → `dc1-r101-prod-srv-01.dc1.example.com`

This ensures both manually-managed DNS records and automatically-generated device records coexist.

### Zone Extraction from Device Names

For devices without `dns_name`, the DNS zone is extracted from the device name pattern:

- **Multi-site pattern**: `loc1-site2a-r101-prod-srv-01` → zone `loc1-site.{domainSuffix}`
  - Device name: `{location}-{site}{digits}{letters}-...` where site code is alphabetic prefix

- **Single-site pattern**: `dc1-r101-srv-01` → zone `dc1.{domainSuffix}`
  - Device name: `{location}-r{rack}-...`

- **Lab environment pattern**: `loc1-site2a-r301-lab-dev-srv-01` → zone `loc1-lab-dev.{domainSuffix}`
  - Device name: `{location}-{site}-{rack}-lab-{environment}-...`

The `domainSuffix` is configured via Helm value or `DOMAIN_SUFFIX` environment variable.

**Example configuration:**
```yaml
domainSuffix: example.com
```

Results in zones like:
- `dc1-site13a-r101-prod-srv-01` → `dc1-site13a-r101-prod-srv-01.dc1-site.example.com`
- `dc1-r101-srv-01` → `dc1-r101-srv-01.dc1.example.com`

## Device Name Templates and CNAME Aliases

Device-generated record names can be customized with an ordered list of parser regexes and Go `text/template` FQDN formats. The canonical template produces the A/AAAA records; each alias template produces a CNAME pointing at the canonical, so every host has exactly one canonical name (PTR records always target it) reachable under any number of alias conventions. Aliases resolve with a `CNAME + A` answer on both the primary and secondaries.

| Env var | Helm value | Meaning |
|---|---|---|
| `DEVICE_NAME_PARSERS` | `deviceNameParsers` (list) | Newline-separated RE2 regexes with named capture groups; ordered, first match wins. Non-matching devices keep the legacy `<device>.<zone>` naming. |
| `NAME_FORMAT_CANONICAL` | `nameFormats.canonical` | Template rendering the complete canonical FQDN. |
| `NAME_FORMAT_ALIASES` | `nameFormats.aliases` (list) | Newline-separated alias FQDN templates (one CNAME each). |
| `NAME_FORMAT_ZONE` | `nameFormats.zone` | Optional sub-template reusable as `{{template "zone" .}}`. |

Template variables: the regex capture groups, plus `{{.name}}` (full device name, lowercased) and `{{.domain}}` (`DOMAIN_SUFFIX`). Functions: `alphaPrefix` (leading letters of a string), `upper`, `lower`. BMC records derive from the rendered FQDN with `-bmc` inserted before the first dot.

**Example (Helm values):**

```yaml
deviceNameParsers:
  # dc1-hall2a-r101-prod-hv-01
  - '^(?P<dc>[a-z0-9]+)-(?P<hall>[a-z]+[0-9][a-z0-9]*)-r(?P<rack>[0-9]+)-(?P<env>prod|mgmt|staging)-(?P<role>[a-z][a-z0-9-]*?)-(?P<idx>[0-9]+)$'
nameFormats:
  zone: '{{.dc}}-{{alphaPrefix .hall}}.{{.domain}}'
  canonical: '{{.name}}.{{template "zone" .}}'
  aliases:
    - '{{.role}}{{.idx}}-{{.dc}}-{{.hall}}-r{{.rack}}.{{template "zone" .}}'
# dc1-hall2a-r101-prod-hv-01.dc1-hall.example.org      A     10.0.0.1  (canonical)
# dc1-hall2a-r101-prod-hv-01-bmc.dc1-hall.example.org  A     10.0.8.1
# hv01-dc1-hall2a-r101.dc1-hall.example.org            CNAME -> canonical
# hv01-dc1-hall2a-r101-bmc.dc1-hall.example.org        CNAME -> canonical-bmc
```

Configure these via a values file. Plain `--set` cannot parse values that start with `{{`; use `--set-json` if you must set them on the CLI.

**RFC 1034 collision handling:** A CNAME colliding with address data is dropped (address data wins); when two devices render the same alias, all claims are dropped (never an arbitrary winner); CNAMEs with empty targets or at a zone apex are dropped; every drop is logged at ERROR. Validate a template set against a Netbox export before deploying:

```
analyzer -file <netbox-ip-dump.json> -validate-name-formats \
  -name-parser '...' -name-canonical '...' -name-alias '...'
```

**Caveats:** An alias template that lands in a zone not enumerated in `secondary.zones` is served by the primary but silently never transferred to statically-configured secondaries (the sidecar logs a warning for zones containing only CNAMEs). Netbox `dns_name` pass-through records are not templated and get no aliases.

## Interface Categorization

The sidecar uses regex patterns to categorize network interfaces and intelligently select the appropriate IP for each device:

**Categories:**
- **BMC**: BMC/IPMI interfaces (pattern: `(?i)bmc|ipmi|ilo|idrac`) → Creates `{hostname}-bmc` DNS records
- **Management (VRF-based)**: Interfaces in management/OOB VRFs (pattern: `(?i)mgmt|oob`) → Primary DNS record (preferred)
- **Management (interface name)**: Interfaces with management naming (pattern: `(?i)mgmt|Management|fxp0|eth[01]|mgt|NET`) → Primary DNS record
- **Loopback**: Routing protocol loopbacks (pattern: `^lo$|^lo0|^Loopback`) → Skipped
- **Dataplane**: Production/storage traffic (pattern: `(?i)storage|vtep|vsan`) → Skipped

**Example:** A hypervisor `dc1-r101-prod-hv-01` with 5 interfaces generates:
- `dc1-r101-prod-hv-01.dc1.example.com` → Management IP (172.26.33.64)
- `dc1-r101-prod-hv-01-bmc.dc1.example.com` → BMC IP (172.26.1.64)

Dataplane interfaces (ovn-vtep-if, storage-if) are automatically excluded from DNS.

All patterns are configurable via environment variables (see Interface Categorization Patterns below).

## Reverse DNS (PTR Records)

The sidecar automatically generates reverse DNS zones alongside forward zones using **static parent zones** that cover your entire IP space.

**Why Static Zones?**
- **Simple configuration**: Define a few large zones once, no dynamic discovery needed on secondaries
- **Easy zone transfers**: Secondary DNS servers just need the same static zone list
- **Scales efficiently**: One zone can cover thousands of subnets

**IPv4 Reverse Zones:**
Configure static parent zones that match your IP allocation:
- `10.in-addr.arpa` - Covers all of 10.0.0.0/8
- `16.172.in-addr.arpa,17.172.in-addr.arpa` - Covers 172.16.0.0/16 and 172.17.0.0/16
- Example: `10.1.2.3` → PTR record `3.2.1.10.in-addr.arpa. PTR server1.example.com.`

**IPv6 Reverse Zones:**
Use reverse nibble notation for IPv6 zones:
- `b.8.0.d.0.1.2.0.ip6.arpa` - Covers 2001:db8::/32

**Configuration:**
```yaml
# Helm values
reverseZones:
  enabled: true
  ipv4:
    - "10.in-addr.arpa"
    - "16.172.in-addr.arpa"
  ipv6:
    - "b.8.0.d.0.1.2.0.ip6.arpa"
```

Or via environment variables:
```bash
ENABLE_REVERSE_ZONES=true
REVERSE_ZONES_IPV4="10.in-addr.arpa,16.172.in-addr.arpa"
REVERSE_ZONES_IPV6="b.8.0.d.0.1.2.0.ip6.arpa"
```

**Secondary Configuration:**
Configure the same zones on your secondary DNS servers:
```yaml
secondary:
  enabled: true
  zones:
    - "dc1.example.com"           # Forward zones
    - "dc2.example.com"
    - "10.in-addr.arpa"           # Reverse zones (same as primary)
    - "16.172.in-addr.arpa"
  transferFrom: ["10.0.1.1"]
```

## Helm Values

**Netbox compatibility:** The DNS client works with both Netbox 3.x and 4.x. It uses raw HTTP against the stable Netbox REST API and auto-detects token type (v1 `Token` auth vs v2 `nbt_` `Bearer` auth).

See `helm/coredns-netbox/values.yaml` for all options. Key values:

| Value | Default | Description |
|---|---|---|
| `netbox.url` | `http://netbox.netbox.svc.cluster.local` | Netbox API URL |
| `netbox.token` | `""` | Netbox API token (use `existingSecret` instead for production) |
| `netbox.existingSecret` | `""` | Name of existing Secret with `token` key |
| `zoneDiscovery.mode` | `zone-depth` | Zone discovery mode: `zone-depth`, `common-suffix`, or `netbox-dns` |
| `zoneDiscovery.depth` | `2` | Number of trailing labels to use as zone name (zone-depth mode) |
| `zoneStorage.sizeLimit` | `256Mi` | emptyDir size limit (used when `zoneStorage.persistent: false`) |
| `zoneStorage.persistent` | `false` | Provision a PersistentVolumeClaim per pod (requires a default StorageClass) |
| `zoneStorage.storageClass` | `""` | StorageClass for the PVC; empty uses the cluster default |
| `zoneStorage.size` | `1Gi` | PVC size per pod |
| `zoneDir` | `/zones` | Directory for zone files |
| `pollInterval` | `60s` | How often to poll Netbox |
| `ttl` | `300` | Default TTL for DNS records |
| `primaryNS` | `ns1.example.org.` | SOA primary nameserver |
| `adminEmail` | `admin.example.org.` | SOA admin email (dot notation) |
| `domainSuffix` | `example.org` | DNS domain suffix for device-based record generation (zone extraction from device names) |
| `stripDCLabel` | `false` | Strip the DC label (immediately before `domainSuffix`) from all Netbox DNS names before zone discovery. See [Collapsing Per-DC Zones](dc-label-stripping.md). |
| `deviceNameParsers` | `[]` | Ordered list of RE2 regexes with named capture groups for device name parsing. First match wins; non-matching devices use legacy naming. See [Device Name Templates and CNAME Aliases](#device-name-templates-and-cname-aliases). |
| `nameFormats.canonical` | `""` | Go `text/template` rendering the complete canonical FQDN for matched devices. |
| `nameFormats.aliases` | `[]` | List of alias FQDN templates; each renders to a CNAME pointing at the canonical. |
| `nameFormats.zone` | `""` | Optional sub-template reusable as `{{template "zone" .}}` in other format strings. |
| `pageSize` | `1000` | Records per API page (max 1000) |
| `maxConcurrency` | `10` | Parallel API request limit |
| `forwardServers` | `[1.1.1.1, 8.8.8.8]` | Upstream DNS resolvers |
| `cacheTTL` | `30` | Maximum TTL (seconds) for cached DNS responses (positive and negative). Reduce in dev/test environments to ensure zone changes are visible quickly. |
| `transfer.to` | `[]` | IPs allowed to pull AXFR (empty = disabled) |
| `secondary.enabled` | `false` | Deploy a secondary CoreDNS for AXFR replication |
| `secondary.zones` | `[]` | Zones to replicate on the secondary |
| `secondary.transferFrom` | `[]` | Primary IPs to pull AXFR from |
| `hostPort.enabled` | `false` | Expose primary CoreDNS on host port 53 |
| `reverseZones.enabled` | `true` | Enable automatic PTR record generation |
| `reverseZones.ipv4` | `["10.in-addr.arpa", ...]` | Static IPv4 reverse zones |
| `reverseZones.ipv6` | `[]` | Static IPv6 reverse zones |
| `metrics.enabled` | `true` | Expose sidecar `/metrics` and CoreDNS metrics ports via the Service |
| `metrics.port` | `9153` | CoreDNS prometheus plugin port |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |

## Interface Categorization Patterns

Customize interface categorization via environment variables:

| Environment Variable | Default | Description |
|---|---|---|
| `LOG_LEVEL` | `INFO` | Logging verbosity: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `GRPC_ADDR` | `:8083` | Address the gRPC server listens on |
| `GRPC_AUTH_TOKEN` | `""` | Bearer token required on all gRPC calls; empty disables auth |
| `NETBOX_WEBHOOK_SECRET` | `""` | HMAC secret for the `/webhook/netbox` route; empty disables the route entirely. See [Netbox Webhook Trigger](netbox-webhook.md). |
| `WEBHOOK_POLL_MIN_INTERVAL` | `5s` | Minimum interval between webhook-triggered full polls (records without `dns_name`); bounds Netbox API request rate during a burst of such events. Does not apply to the gRPC `ForceNetboxPoll` control call. See [Netbox Webhook Trigger](netbox-webhook.md). |
| `BMC_INTERFACE_PATTERN` | `(?i)bmc\|ipmi\|ilo\|idrac` | Regex for BMC interfaces |
| `LOOPBACK_PATTERN` | `^lo$\|^lo0\|^Loopback` | Regex for loopback interfaces |
| `DATAPLANE_PATTERN` | `(?i)storage\|vtep\|vsan` | Regex for dataplane interfaces |
| `MGMT_VRF_PATTERN` | `(?i)mgmt\|oob` | Regex for management VRFs |
| `MGMT_INTERFACE_PATTERN` | `(?i)mgmt\|Management\|fxp0\|eth[01]\|mgt\|NET` | Regex for management interfaces |
| `DOMAIN_SUFFIX` | `example.org` | DNS domain suffix for zone extraction from device names |
| `STRIP_DC_LABEL` | `false` | Strip the DC label from DNS names before zone discovery. See [Collapsing Per-DC Zones](dc-label-stripping.md). |
| `DEVICE_NAME_PARSERS` | `""` | Newline-separated RE2 regexes with named capture groups; ordered, first match wins. See [Device Name Templates and CNAME Aliases](#device-name-templates-and-cname-aliases). |
| `NAME_FORMAT_CANONICAL` | `""` | Go `text/template` rendering the complete canonical FQDN. |
| `NAME_FORMAT_ALIASES` | `""` | Newline-separated alias FQDN templates (one CNAME each). |
| `NAME_FORMAT_ZONE` | `""` | Optional sub-template reusable as `{{template "zone" .}}`. |

These can be set in the Helm chart's `sidecar.env` values or directly in the sidecar container.

**Example - Enable debug logging:**
```yaml
# Helm values
sidecar:
  env:
    - name: LOG_LEVEL
      value: "DEBUG"
```

## Zone Discovery Modes

The sidecar auto-discovers zones from Netbox FQDNs. Three modes are available:

- **`zone-depth`** (default): Uses the last N labels of each FQDN as the zone name. With `depth=2`, `server1-mgmt.dc1.mycompany.com` becomes zone `mycompany.com`. With `depth=3`, it becomes `dc1.mycompany.com`.
- **`common-suffix`**: Groups records by their longest common domain suffix.
- **`netbox-dns`**: Queries the Netbox DNS plugin API (`/api/plugins/netbox-dns/zones/`) and matches records to their longest-matching zone.
