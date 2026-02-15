"""Create a Netbox API token and print the full v2 token string.

Used by: make dev.token (Makefile)

Netbox 4.x v2 tokens are HMAC-hashed at rest; the plaintext is only
available in memory immediately after creation. This script captures it
and prints it so the Makefile can store it in a Kubernetes Secret.

Note: v2 tokens (HMAC + peppers, nbt_ prefix) are Netbox 4.x specific.
Netbox 3.x users create standard API tokens via the UI or REST API;
those tokens use plain "Token <key>" authorization.

Run inside the Netbox pod via:
    kubectl exec deploy/netbox -- python manage.py shell --no-startup < create-token.py
"""
from users.models import Token, User

# Uses the admin user created by the superuser section in dev/netbox-values.yaml
user = User.objects.get(username='admin')

# Delete any existing tokens for a clean state
Token.objects.filter(user=user).delete()

# Create a new v2 token
t = Token(user=user)
t.save()

# _token holds the plaintext only in memory right after creation;
# once the process exits this value is lost — only the HMAC hash persists.
full_token = f"nbt_{t.key}.{t._token}"
print(full_token)
