package netboxclient

import "strings"

// AuthHeader returns the appropriate Authorization header value for a Netbox API token.
// Netbox 4.x v2 tokens (nbt_ prefix) use Bearer auth; older tokens use Token auth.
func AuthHeader(token string) string {
	if strings.HasPrefix(token, "nbt_") {
		return "Bearer " + token
	}
	return "Token " + token
}
