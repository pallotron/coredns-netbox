"""Bulk-create 18,000 IP addresses directly via Django ORM.

Much faster than the REST API — bypasses serialization, validation,
and HTTP overhead. Used for dev/testing only.

Run inside the Netbox pod via:
    kubectl exec deploy/netbox -n netbox -c netbox -- \
        python /opt/netbox/netbox/manage.py shell --no-startup --no-imports \
        -c "$(cat dev/seed-ips.py)"
"""
from ipam.models import IPAddress
from django.db import connection

# Clean existing IPs
deleted, _ = IPAddress.objects.all().delete()
print(f"Deleted {deleted} existing IP addresses")

dcs = [("dc1", 1), ("dc2", 2), ("dc3", 3)]
ifaces = [("mgmt", 0), ("bmc", 8), ("storage", 16)]
hosts_per_dc = 2000

records = []
for dc_name, dc_id in dcs:
    for if_name, base_octet3 in ifaces:
        for h in range(1, hosts_per_dc + 1):
            octet3 = base_octet3 + (h - 1) // 254
            octet4 = (h - 1) % 254 + 1
            addr = f"10.{dc_id}.{octet3}.{octet4}/24"
            fqdn = f"server{h}-{if_name}.{dc_name}.mycompany.com"
            records.append(IPAddress(address=addr, dns_name=fqdn, status="active"))

# bulk_create in batches of 1000
batch_size = 1000
for i in range(0, len(records), batch_size):
    batch = records[i:i + batch_size]
    IPAddress.objects.bulk_create(batch, batch_size=batch_size)
    print(f"Created {min(i + batch_size, len(records))}/{len(records)} records")

print(f"Seed complete: {IPAddress.objects.count()} total IP addresses")

# --- Devices with assigned interfaces (exercise the categorizer/template path) ---
# Two hypervisors in dc1/hall h1a. Their IPs (10.9.0.x mgmt, 10.9.8.x BMC) are
# disjoint from the dns_name seed ranges above. Device names match the
# deviceNameParsers configured in dev/coredns-netbox-values.yaml.
from dcim.models import Device, DeviceRole, DeviceType, Interface, Manufacturer, Site

site, _ = Site.objects.get_or_create(name="dev", defaults={"slug": "dev"})
mfr, _ = Manufacturer.objects.get_or_create(name="generic", defaults={"slug": "generic"})
dtype, _ = DeviceType.objects.get_or_create(manufacturer=mfr, model="server", defaults={"slug": "server"})
role, _ = DeviceRole.objects.get_or_create(name="server", defaults={"slug": "server"})

for i in (1, 2):
    dev, _ = Device.objects.get_or_create(
        name=f"dc1-h1a-r10{i}-prod-hv-0{i}",
        defaults={"device_type": dtype, "role": role, "site": site},
    )
    mgmt, _ = Interface.objects.get_or_create(device=dev, name="mgmt0", defaults={"type": "1000base-t"})
    bmc, _ = Interface.objects.get_or_create(device=dev, name="bmc0", defaults={"type": "1000base-t"})
    mgmt_ip = IPAddress.objects.filter(address=f"10.9.0.{i}/24").first()
    if mgmt_ip is None:
        mgmt_ip = IPAddress(address=f"10.9.0.{i}/24", status="active")
        mgmt_ip.assigned_object = mgmt
        mgmt_ip.save()
    bmc_ip = IPAddress.objects.filter(address=f"10.9.8.{i}/24").first()
    if bmc_ip is None:
        bmc_ip = IPAddress(address=f"10.9.8.{i}/24", status="active")
        bmc_ip.assigned_object = bmc
        bmc_ip.save()

# N2 negative leg: an interface that matches none of the categorization
# patterns (swp1 is a front-panel/dataplane port, not mgmt/BMC/loopback)
# must yield no DNS record at all. It hangs off a device that already has
# mgmt0 and bmc0, so the device itself still resolves — what is under test
# is that this extra address appears nowhere in the zone.
dev1 = Device.objects.get(name="dc1-h1a-r101-prod-hv-01")
swp, _ = Interface.objects.get_or_create(device=dev1, name="swp1", defaults={"type": "1000base-t"})
swp_ip = IPAddress.objects.filter(address="10.9.16.1/24").first()
if swp_ip is None:
    swp_ip = IPAddress(address="10.9.16.1/24", status="active")
    swp_ip.assigned_object = swp
    swp_ip.save()

# N3 catch-all leg: a device whose name matches none of the configured
# deviceNameParsers falls back to <device>.<extracted zone> with no CNAME
# aliases. "oob-sw-01" is deliberately outside the dc-hall-rack-env-role-idx
# convention the dev parser expects.
catchall, _ = Device.objects.get_or_create(
    name="oob-sw-01",
    defaults={"device_type": dtype, "role": role, "site": site},
)
catchall_mgmt, _ = Interface.objects.get_or_create(
    device=catchall, name="mgmt0", defaults={"type": "1000base-t"}
)
catchall_ip = IPAddress.objects.filter(address="10.9.24.1/24").first()
if catchall_ip is None:
    catchall_ip = IPAddress(address="10.9.24.1/24", status="active")
    catchall_ip.assigned_object = catchall_mgmt
    catchall_ip.save()

# N7: one IPv6 assignment, in the RFC 3849 documentation prefix. Forward
# gives an AAAA; reverse exercises ip6.arpa, which dev enables via
# reverseZones.ipv6 (8.b.d.0.1.0.0.2.ip6.arpa covers 2001:db8::/32).
v6 = IPAddress.objects.filter(address="2001:db8::1/64").first()
if v6 is None:
    IPAddress.objects.create(
        address="2001:db8::1/64",
        dns_name="server-v6.dc1.mycompany.com",
        status="active",
    )

print(f"Device seed complete: {Device.objects.count()} devices")

# --- Task 11: primary_ip4 selection, interface records, and PTR priority ---
# Three fixtures exercising the device A-record/interface-record/PTR-priority
# spec: primary_ip4 as the source of truth over the regex heuristic, per-
# interface address records (INTERFACE_RECORDS_ENABLED), and PTR priority
# (explicit dns_name > interface > device > bmc). Names match the
# dc-hall-rack-env-role-idx convention so deviceNameParsers matches them,
# same as the dc1-h1a-r10N hypervisors above; racks 301/401/501 are unused
# elsewhere in this fixture.

# Switch-like device: primary_ip4 points at "Management1" even though several
# other interface-assigned addresses ("EthernetN/1", the switch's front-panel
# ports) are also present as candidates.
switch, _ = Device.objects.get_or_create(
    name="dc1-h1a-r301-prod-sw-01",
    defaults={"device_type": dtype, "role": role, "site": site},
)
mgmt1_iface, _ = Interface.objects.get_or_create(
    device=switch, name="Management1", defaults={"type": "1000base-t"}
)
mgmt1_ip = IPAddress.objects.filter(address="10.9.32.1/24").first()
if mgmt1_ip is None:
    mgmt1_ip = IPAddress(address="10.9.32.1/24", status="active")
    mgmt1_ip.assigned_object = mgmt1_iface
    mgmt1_ip.save()

for eth_idx in (1, 2, 3):
    eth_iface, _ = Interface.objects.get_or_create(
        device=switch, name=f"Ethernet{eth_idx}/1", defaults={"type": "1000base-t"}
    )
    eth_ip = IPAddress.objects.filter(address=f"10.9.33.{eth_idx}/24").first()
    if eth_ip is None:
        eth_ip = IPAddress(address=f"10.9.33.{eth_idx}/24", status="active")
        eth_ip.assigned_object = eth_iface
        eth_ip.save()

switch.primary_ip4 = mgmt1_ip
switch.save()

# Fallback-path device: no primary_ip4 at all, so SelectDeviceIPsHybrid must
# fall back to the regex heuristic. Its mgmt address lives on "eno1" -- a
# different branch of the "management" fallback pattern (^eno[0-9]) from the
# mgmt0/"(?i)mgmt" branch the dc1-h1a-r10N hypervisors above already exercise.
fallback_dev, _ = Device.objects.get_or_create(
    name="dc1-h1a-r401-prod-fw-01",
    defaults={"device_type": dtype, "role": role, "site": site},
)
eno1_iface, _ = Interface.objects.get_or_create(
    device=fallback_dev, name="eno1", defaults={"type": "1000base-t"}
)
eno1_ip = IPAddress.objects.filter(address="10.9.40.1/24").first()
if eno1_ip is None:
    eno1_ip = IPAddress(address="10.9.40.1/24", status="active")
    eno1_ip.assigned_object = eno1_iface
    eno1_ip.save()
# primary_ip4 deliberately left unset.

# primary_ip4 + loopback device: primary_ip4 selects the mgmt0 address; a
# second Loopback0 address (uncategorized by the device-selection heuristic)
# sits in the 10.in-addr.arpa range the suite's reverse zones already cover,
# so its forward/PTR records only appear when INTERFACE_RECORDS_ENABLED --
# proving a primary_ip4-selected device's other addresses still get reverse
# coverage via the interface-records path.
loop_dev, _ = Device.objects.get_or_create(
    name="dc1-h1a-r501-prod-sv-01",
    defaults={"device_type": dtype, "role": role, "site": site},
)
loop_mgmt_iface, _ = Interface.objects.get_or_create(
    device=loop_dev, name="mgmt0", defaults={"type": "1000base-t"}
)
loop_mgmt_ip = IPAddress.objects.filter(address="10.9.50.1/24").first()
if loop_mgmt_ip is None:
    loop_mgmt_ip = IPAddress(address="10.9.50.1/24", status="active")
    loop_mgmt_ip.assigned_object = loop_mgmt_iface
    loop_mgmt_ip.save()
loop_dev.primary_ip4 = loop_mgmt_ip
loop_dev.save()

loopback_iface, _ = Interface.objects.get_or_create(
    device=loop_dev, name="Loopback0", defaults={"type": "virtual"}
)
loopback_ip = IPAddress.objects.filter(address="10.9.58.1/24").first()
if loopback_ip is None:
    loopback_ip = IPAddress(address="10.9.58.1/24", status="active")
    loopback_ip.assigned_object = loopback_iface
    loopback_ip.save()

print("Task 11 fixture complete: primary_ip4 switch, eno1 fallback device, "
      "primary_ip4+loopback device seeded")
