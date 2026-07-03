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

print(f"Device seed complete: {Device.objects.count()} devices")
