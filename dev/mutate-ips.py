"""Mutate NetBox IP data via the Django ORM — companion to seed-ips.py.

Run inside the NetBox pod:
    kubectl exec deploy/netbox -n netbox -c netbox -- \
        env OP=<op> [ARGS...] python /opt/netbox/netbox/manage.py shell \
        --no-startup --no-imports -c "$(cat dev/mutate-ips.py)"

Ops (env vars):
  OP=set-status DNS_NAME=<fqdn> STATUS=<active|reserved|deprecated>
  OP=add        DNS_NAME=<fqdn> ADDRESS=<ip/len> [STATUS=active]
  OP=del        DNS_NAME=<fqdn>
  OP=bulk-add   PREFIX=<name-prefix> ZONE=<zone> COUNT=<n> NET=<10.x>
  OP=bulk-del   PREFIX=<name-prefix>
  OP=revoke-token   (expires every API token; v2 tokens are HMAC-hashed at
                     rest so deletion would be unrecoverable — expiry is the
                     reversible form of revocation)
  OP=restore-token  (clears the expiry on every API token)

The e2e mutation tests (tests/e2e/mutate_test.go) perform the same
operations inline; this script is the human-facing equivalent.
"""
import os

from ipam.models import IPAddress

op = os.environ["OP"]

if op == "set-status":
    ip = IPAddress.objects.get(dns_name=os.environ["DNS_NAME"])
    ip.status = os.environ.get("STATUS", "active")
    ip.save()
    print(f"{ip.dns_name}: status -> {ip.status}")
elif op == "add":
    ip = IPAddress.objects.create(
        address=os.environ["ADDRESS"],
        dns_name=os.environ["DNS_NAME"],
        status=os.environ.get("STATUS", "active"),
    )
    print(f"created {ip.dns_name} -> {ip.address}")
elif op == "del":
    deleted, _ = IPAddress.objects.filter(dns_name=os.environ["DNS_NAME"]).delete()
    print(f"deleted {deleted}")
elif op == "bulk-add":
    prefix, zone = os.environ["PREFIX"], os.environ["ZONE"]
    count, net = int(os.environ["COUNT"]), os.environ["NET"]
    records = [
        IPAddress(
            address=f"{net}.{i // 250}.{i % 250 + 1}/24",
            dns_name=f"{prefix}{i + 1}.{zone}",
            status="active",
        )
        for i in range(count)
    ]
    IPAddress.objects.bulk_create(records, batch_size=1000)
    print(f"created {count}")
elif op == "bulk-del":
    deleted, _ = IPAddress.objects.filter(
        dns_name__startswith=os.environ["PREFIX"]
    ).delete()
    print(f"deleted {deleted}")
elif op == "revoke-token":
    import datetime

    from django.utils import timezone
    from users.models import Token

    n = Token.objects.update(expires=timezone.now() - datetime.timedelta(days=1))
    print(f"expired {n} token(s)")
elif op == "restore-token":
    from users.models import Token

    n = Token.objects.update(expires=None)
    print(f"restored {n} token(s)")
else:
    raise SystemExit(f"unknown OP {op!r}")
