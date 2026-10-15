"""Seed Netbox with a device-dominated record set at scale.

Used by: make dev.seed.scale (the N26 scale drill — see docs/scale.md)

Where DNS records come mostly from DEVICES, the expensive path runs:
interface categorization per device, canonical + alias templating, and
CNAME collision resolution across the zone. The regular e2e seed
(dev/seed-ips.py) has the opposite shape — mostly IPs carrying an explicit
dns_name — so it cannot show how a device-dominated workload behaves.

Device names match the deviceNameParsers in dev/coredns-netbox-values.yaml
so templating and alias generation actually run.

Run inside the Netbox pod:
    kubectl exec deploy/netbox -n netbox -c netbox -- \
        env SEED_DEVICES=25000 python /opt/netbox/netbox/manage.py shell \
        --no-startup --no-imports -c "$(cat dev/seed-scale.py)"
"""
import os

from dcim.models import Device, DeviceRole, DeviceType, Interface, Manufacturer, Site
from django.contrib.contenttypes.models import ContentType
from ipam.models import IPAddress

count = int(os.environ.get("SEED_DEVICES", "25000"))

site, _ = Site.objects.get_or_create(name="scale", defaults={"slug": "scale"})
mfr, _ = Manufacturer.objects.get_or_create(name="generic", defaults={"slug": "generic"})
dtype, _ = DeviceType.objects.get_or_create(
    manufacturer=mfr, model="server", defaults={"slug": "server"}
)
role, _ = DeviceRole.objects.get_or_create(name="server", defaults={"slug": "server"})

# Scale-drill objects live in 10.20-10.60.x and the "scale" site so a
# teardown is a single filter; they stay disjoint from seed-ips.py ranges.
devices = [
    Device(
        name=f"dc{i % 3 + 1}-h{i % 9 + 1}{'abcd'[i % 4]}-r{i % 400 + 1}-prod-hv-{i}",
        device_type=dtype,
        role=role,
        site=site,
    )
    for i in range(count)
]
Device.objects.bulk_create(devices, batch_size=1000, ignore_conflicts=True)
created = list(Device.objects.filter(site=site).order_by("id"))
print(f"devices: {len(created)}")

interfaces = []
for dev in created:
    interfaces.append(Interface(device=dev, name="mgmt0", type="1000base-t"))
    interfaces.append(Interface(device=dev, name="bmc0", type="1000base-t"))
Interface.objects.bulk_create(interfaces, batch_size=1000, ignore_conflicts=True)
print(f"interfaces: {Interface.objects.filter(device__site=site).count()}")

# bulk_create skips GenericForeignKey descriptors, so set the content type
# and object id directly — that is what the sidecar's fetch reads back.
iface_ct = ContentType.objects.get_for_model(Interface)
ifaces = list(Interface.objects.filter(device__site=site).order_by("id"))
ips = [
    IPAddress(
        address=f"10.{(20 if iface.name == 'mgmt0' else 60) + n // 65024}.{(n // 254) % 254}.{n % 254 + 1}/24",
        status="active",
        assigned_object_type=iface_ct,
        assigned_object_id=iface.id,
    )
    for n, iface in enumerate(ifaces)
]
IPAddress.objects.bulk_create(ips, batch_size=1000, ignore_conflicts=True)
print(f"ips: {IPAddress.objects.filter(assigned_object_type=iface_ct).count()}")
print("scale seed complete — allow one POLL_INTERVAL, then: make dev.scale.report")
