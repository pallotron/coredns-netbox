// Package netboxwebhook implements a direct HTTP webhook receiver for Netbox
// ipam.ipaddress create/update/delete events. It replaces the
// webhook-to-message-broker relay originally proposed in issue #27: Netbox's
// built-in webhook feature posts straight to this handler, HMAC-signed, and
// changes are applied to internal/dynamicstore without a full Netbox re-fetch.
package netboxwebhook

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
)

// verifySignature reports whether header is the correct HMAC-SHA512 hex
// digest of body under secret, matching Netbox's X-Hook-Signature scheme
// (verified against a real Netbox v4.6.0 delivery — see issue #27 comment).
func verifySignature(secret string, body []byte, header string) bool {
	if header == "" {
		return false
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}
