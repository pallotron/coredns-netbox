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
