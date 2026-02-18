# Netbox IP Analyzer

A CLI tool to analyze Netbox IP address data and show what DNS records would be generated.

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

- `-file <path>` - Path to the Netbox all_ips.json file (required)
- `-domain <suffix>` - Domain suffix for DNS zones (default: `example.com`)
- `-stats` - Show statistics about the data
- `-all` - Show all IPs for each device (use with `-format detailed`)
- `-device <substring>` - Filter to devices matching the substring
- `-format <format>` - Output format: `summary` (default), `detailed`, or `csv`

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
7. Generates DNS records:
   - `{device}.{zone}.` → Primary management IP
   - `{device}-bmc.{zone}.` → BMC IP (if available)

## Zone Extraction

The zone is extracted from the device name using this logic:

- `dc1-site1a-r101-prod-hv-01` → `dc1-site.yourcompany.com`
- `dc2-m21-r101-prod-hv-01` → `dc2-m.yourcompany.com`
- `site-cu2a-r207-lab-console-01` → `site-cu.yourcompany.com`

Pattern: Takes first two components, strips trailing digits/letters from second component, then appends the domain suffix.

The domain suffix defaults to `example.com` but can be set with the `-domain` flag.

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
```

### Summary

```
$ ./analyzer -file all_ips.json
Total devices with DNS records: 13031
Devices with primary management IP: 13028
Devices with BMC IP: 5580

DNS Records to be created:
  Primary hostnames: 13028
  BMC hostnames (-bmc suffix): 5580
  Total DNS A/AAAA records: 18608

Top 10 Zones:
  dc1-site.yourcompany.com      :   6073 devices
  dc2-site.yourcompany.com      :   2566 devices
  site3-cu.yourcompany.com      :   2103 devices
  dc4-loc.yourcompany.com       :   1693 devices
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
