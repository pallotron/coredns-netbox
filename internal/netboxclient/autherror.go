package netboxclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Paths the client reads, and the NetBox permission each one needs.
const (
	ipAddressesPath     = "/api/ipam/ip-addresses/"
	devicesPath         = "/api/dcim/devices/"
	virtualMachinesPath = "/api/virtualization/virtual-machines/"
)

var requiredPermission = map[string]string{
	ipAddressesPath:     "ipam.view_ipaddress",
	devicesPath:         "dcim.view_device",
	virtualMachinesPath: "virtualization.view_virtualmachine",
}

// AuthError is a 401 or 403 from NetBox. NetBox returns 403 both for an
// invalid or expired token and for a valid token whose user lacks the view
// permission, so the message names both causes. It is never retried: neither
// cause fixes itself within a poll.
type AuthError struct {
	Status     int
	Path       string
	Permission string
	Body       string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("netbox API returned %d for %s: the token is invalid or expired, or lacks the %s permission: %s",
		e.Status, e.Path, e.Permission, e.Body)
}

// isAuthError reports whether err (or anything it wraps) is an AuthError.
func isAuthError(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// statusError builds the error for a non-200 response from apiPath, reading
// at most maxBody bytes of the body.
func statusError(resp *http.Response, apiPath string, maxBody int64) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &AuthError{
			Status:     resp.StatusCode,
			Path:       apiPath,
			Permission: requiredPermission[apiPath],
			Body:       string(body),
		}
	}
	return fmt.Errorf("netbox API returned %d: %s", resp.StatusCode, string(body))
}
