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
| `netboxPlugin.enabled` | `false` | Enable CoreDNS netbox plugin for live API fallthrough |
| `netboxPlugin.zones` | `[]` | **Required when enabled.** Zones the netbox plugin handles; queries outside these zones go straight to forward |
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
| `pageSize` | `1000` | Records per API page (max 1000) |
| `maxConcurrency` | `10` | Parallel API request limit |
| `forwardServers` | `[1.1.1.1, 8.8.8.8]` | Upstream DNS resolvers |
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
| `BMC_INTERFACE_PATTERN` | `(?i)bmc\|ipmi\|ilo\|idrac` | Regex for BMC interfaces |
| `LOOPBACK_PATTERN` | `^lo$\|^lo0\|^Loopback` | Regex for loopback interfaces |
| `DATAPLANE_PATTERN` | `(?i)storage\|vtep\|vsan` | Regex for dataplane interfaces |
| `MGMT_VRF_PATTERN` | `(?i)mgmt\|oob` | Regex for management VRFs |
| `MGMT_INTERFACE_PATTERN` | `(?i)mgmt\|Management\|fxp0\|eth[01]\|mgt\|NET` | Regex for management interfaces |
| `DOMAIN_SUFFIX` | `example.org` | DNS domain suffix for zone extraction from device names |

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
