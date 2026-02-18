# Netbox IP Analyzer

A CLI tool to analyze Netbox IP address data and preview what forward (A/AAAA) and reverse (PTR) DNS records would be generated.

## Quick Start

```bash
# 1. Fetch data from Netbox (requires NETBOX_TOKEN)
NETBOX_TOKEN='your-token' NETBOX_URL='http://netbox.example.com' ./scripts/fetch_netbox_ips.sh
jq -s '[.[].results[]]' ./netbox_ips_dump/page_*.json > all_ips.json

# 2. Build the analyzer
go build -o analyzer ./cmd/analyzer/main.go

# 3. Analyze the data (set your domain)
./analyzer -file all_ips.json -domain "yourcompany.com" -stats
```

## Usage

```bash
# Show statistics about the data
./analyzer -file all_ips.json -stats

# Show summary (default output)
./analyzer -file all_ips.json

# Show detailed output for all devices
./analyzer -file all_ips.json -format detailed

# Show detailed output including all IPs for each device
./analyzer -file all_ips.json -format detailed -all

# Filter to a specific device
./analyzer -file all_ips.json -device "dc1-r01-hv-01" -format detailed -all

# Output as CSV
./analyzer -file all_ips.json -format csv > dns_records.csv
```

## Command Line Flags

### General Options
- `-file <path>` - Path to the Netbox all_ips.json file (required)
- `-domain <suffix>` - Domain suffix for DNS zones (default: `example.com`)
- `-stats` - Show statistics about the data
- `-all` - Show all IPs for each device (use with `-format detailed`)
- `-device <substring>` - Filter to devices matching the substring
- `-format <format>` - Output format: `summary` (default), `detailed`, or `csv`

### Reverse DNS Options
- `-enable-reverse-zones` - Enable PTR record preview (default: `true`)
- `-ipv4-zones <zones>` - Comma-separated list of IPv4 reverse zones (default: `"10.in-addr.arpa,172.16.in-addr.arpa"`)
- `-ipv6-zones <zones>` - Comma-separated list of IPv6 reverse zones (default: `""`)

**Note:** Zones must match your IP space. For example:
- IPs in 10.x.x.x → use `10.in-addr.arpa`
- IPs in 172.28.x.x → use `28.172.in-addr.arpa`
- IPs in 192.168.x.x → use `168.192.in-addr.arpa`

### Pattern Customization

You can customize the interface categorization patterns:

- `-bmc-pattern` - Regex for BMC interfaces (default: `(?i)bmc|ipmi|ilo|idrac`)
- `-loopback-pattern` - Regex for loopback interfaces (default: `^lo$|^lo0|^Loopback`)
- `-dataplane-pattern` - Regex for dataplane interfaces (default: `(?i)storage|vtep|vsan`)
- `-mgmt-vrf-pattern` - Regex for management VRFs (default: `(?i)mgmt|oob`)
- `-mgmt-iface-pattern` - Regex for management interfaces (default: `(?i)mgmt|Management|fxp0|eth[01]|mgt|NET`)

## How It Works

The analyzer:

1. Reads the Netbox IP address JSON data
2. Filters to only active IP addresses
3. Categorizes each interface based on regex patterns:
   - **BMC**: BMC/IPMI interfaces
   - **Loopback**: Routing protocol loopbacks
   - **Dataplane**: Storage/production traffic interfaces
   - **Management (VRF-based)**: Interfaces in management/OOB VRFs
   - **Management (interface name)**: Interfaces with management naming
4. Groups IPs by device
5. Selects the best management IP (prefers VRF-based over interface name)
6. Selects BMC IP if available
7. Generates forward DNS records:
   - `{device}.{zone}.` → Primary management IP (A/AAAA record)
   - `{device}-bmc.{zone}.` → BMC IP (A/AAAA record if available)
8. Generates reverse DNS zones and PTR records:
   - Discovers reverse zones based on IP prefix length (e.g., /16 → `1.10.in-addr.arpa`)
   - Creates PTR records mapping IPs back to hostnames

## Zone Extraction

The zone is extracted from the device name using this logic:

- `dc1-site1a-r101-prod-hv-01` → `dc1-site.yourcompany.com`
- `dc2-m21-r101-prod-hv-01` → `dc2-m.yourcompany.com`
- `site-cu2a-r207-lab-console-01` → `site-cu.yourcompany.com`

Pattern: Takes first two components, strips trailing digits/letters from second component, then appends the domain suffix.

The domain suffix defaults to `example.com` but can be set with the `-domain` flag.

## Reverse DNS (PTR Records)

The analyzer previews reverse DNS zones and PTR records using static parent zones that match your IP space.

**Static Parent Zones:**
Instead of dynamically creating many small zones, you configure a few large zones that cover your entire IP allocation. This matches how the sidecar generates zones for AXFR to secondaries.

**Examples:**

```bash
# Default zones (10.0.0.0/8 and 172.16.0.0/16)
./analyzer -file all_ips.json -stats

# Custom zones for your IP space
./analyzer -file all_ips.json -ipv4-zones "10.in-addr.arpa,28.172.in-addr.arpa" -stats

# Cover multiple /16 blocks in 172.16.0.0/12
./analyzer -file all_ips.json \
  -ipv4-zones "16.172.in-addr.arpa,17.172.in-addr.arpa,18.172.in-addr.arpa" \
  -stats

# Disable reverse zone preview
./analyzer -file all_ips.json -enable-reverse-zones=false -stats
```

**Finding Your Zones:**
If you're not sure which zones to configure, run the analyzer with a broad zone and check the output:
```bash
# Start with /8 coverage
./analyzer -file all_ips.json -ipv4-zones "10.in-addr.arpa,172.in-addr.arpa" -stats
```

## Examples

### Statistics

```
$ ./analyzer -file all_ips.json -stats
Total active IP records: 38320
Records with device assigned: 36411 (95.0%)
Records with DNS name: 774 (2.0%)

Interface Categories:
  mgmt-interface      :   6177 (16.1%)
  bmc                 :   5592 (14.6%)
  unknown             :   2260 (5.9%)
  dataplane           :  14931 (39.0%)
  mgmt-vrf            :   8091 (21.1%)
  loopback            :   1269 (3.3%)

Reverse DNS Zones: 12
Total PTR records: 18608

Top 10 Reverse Zones:
  26.172.in-addr.arpa                      :   9753 PTR records
  1.10.in-addr.arpa                        :   3421 PTR records
  2.10.in-addr.arpa                        :   2566 PTR records
  3.10.in-addr.arpa                        :   1893 PTR records
```

### Summary

```
$ ./analyzer -file all_ips.json
Total devices with DNS records: 13031
Devices with primary management IP: 13028
Devices with BMC IP: 5580

Forward DNS Records to be created:
  Primary hostnames: 13028
  BMC hostnames (-bmc suffix): 5580
  Total DNS A/AAAA records: 18608

Reverse DNS Records to be created:
  Reverse zones: 12
  Total PTR records: 18608

Top 10 Forward Zones:
  dc1-site.yourcompany.com      :   6073 devices
  dc2-site.yourcompany.com      :   2566 devices
  site3-cu.yourcompany.com      :   2103 devices
  dc4-loc.yourcompany.com       :   1693 devices

Sample Forward DNS records (first 10 devices):
  dc1-r01-hv-01.dc1-site.yourcompany.com. → 172.26.33.1
  dc1-r01-hv-01-bmc.dc1-site.yourcompany.com. → 172.26.1.1
  dc1-r01-hv-02.dc1-site.yourcompany.com. → 172.26.33.2
  ...

Sample Reverse DNS (PTR) records (first 10):
  1.33.26.172.26.172.in-addr.arpa. → dc1-r01-hv-01.dc1-site.yourcompany.com.
  1.1.26.172.26.172.in-addr.arpa. → dc1-r01-hv-01-bmc.dc1-site.yourcompany.com.
  2.33.26.172.26.172.in-addr.arpa. → dc1-r01-hv-02.dc1-site.yourcompany.com.
  ...
```

### Detailed Device View

```
$ ./analyzer -file all_ips.json -domain "yourcompany.com" -device "dc1-r101-prod-hv-01" -format detailed -all
Device: dc1-r101-prod-hv-01
  Zone: dc1.yourcompany.com
  Primary: dc1-r101-prod-hv-01.dc1.yourcompany.com. → 172.26.33.64 (interface: mgmt-front-1, vrf: dc1mgmt)
  BMC:     dc1-r101-prod-hv-01-bmc.dc1.yourcompany.com. → 172.26.1.64 (interface: bmc-front, vrf: dc1mgmt)
  All IPs:
    ovn-vtep-if          10.26.1.11      prodnet                   dataplane
    block-storage-if     10.206.1.11     prodnet                   dataplane
    file-storage-if      10.207.128.65   storage                   dataplane
    bmc-front            172.26.1.64     dc1mgmt                   bmc
    mgmt-front-1         172.26.33.64    dc1mgmt                   mgmt-vrf
```

### CSV Export

The CSV export includes both forward (A/AAAA) and reverse (PTR) records:

```bash
$ ./analyzer -file all_ips.json -format csv | head -20
record_type,zone,name,value,device,interface,vrf
A,dc1.yourcompany.com,dc1-r01-hv-01,172.26.33.64,dc1-r01-hv-01,mgmt-front-1,dc1mgmt
A,dc1.yourcompany.com,dc1-r01-hv-01-bmc,172.26.1.64,dc1-r01-hv-01,bmc-front,dc1mgmt
PTR,64.33.26.172.in-addr.arpa,64,dc1-r01-hv-01.dc1.yourcompany.com,dc1-r01-hv-01,mgmt-front-1,dc1mgmt
PTR,64.1.26.172.in-addr.arpa,64,dc1-r01-hv-01-bmc.dc1.yourcompany.com,dc1-r01-hv-01,bmc-front,dc1mgmt
...
```

**CSV Columns:**
- `record_type` - Record type (A, AAAA, or PTR)
- `zone` - DNS zone name (forward or reverse)
- `name` - Record name (hostname for A/AAAA, PTR name for PTR)
- `value` - Record value (IP address for A/AAAA, target hostname for PTR)
- `device` - Source device name
- `interface` - Source interface name
- `vrf` - Source VRF name
```
