package netboxclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchDevices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/dcim/devices/":
			_, _ = fmt.Fprint(w, `{"count": 2, "results": [
				{"name": "dc1-site13a-r101-prod-eth-tor-01", "status": {"value": "active"},
				 "primary_ip4": {"id": 101, "address": "172.16.1.2/24"}, "oob_ip": null},
				{"name": "dc1-site13a-r101-prod-hv-01", "status": {"value": "racked"},
				 "primary_ip4": null, "oob_ip": {"id": 202, "address": "10.9.0.5/24"}}
			]}`)
		case "/api/virtualization/virtual-machines/":
			_, _ = fmt.Fprint(w, `{"count": 1, "results": [
				{"name": "dc1-vm-01", "status": {"value": "active"},
				 "primary_ip4": {"id": 303, "address": "10.1.2.3/24"}}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "test-token", 1000, 4, 0, time.Millisecond, time.Millisecond)
	require.NoError(t, err, "client construction should succeed")

	devices, err := c.FetchDevices(context.Background())
	require.NoError(t, err, "FetchDevices should succeed")
	require.Len(t, devices, 3, "two devices + one VM")

	assert.Equal(t, DeviceRecord{Name: "dc1-site13a-r101-prod-eth-tor-01", Status: "active", PrimaryIP4ID: 101}, devices[0])
	assert.Equal(t, DeviceRecord{Name: "dc1-site13a-r101-prod-hv-01", Status: "racked", OOBIPID: 202}, devices[1])
	assert.Equal(t, DeviceRecord{Name: "dc1-vm-01", Status: "active", PrimaryIP4ID: 303}, devices[2])
}

func TestFetchDevicesPagination(t *testing.T) {
	// 3 devices with pageSize 2 → a probe, then two pages.
	devices := []string{"dev-a", "dev-b", "dev-c"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dcim/devices/" {
			_, _ = fmt.Fprint(w, `{"count": 0, "results": []}`)
			return
		}
		offset, limit := 0, 1
		_, _ = fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
		_, _ = fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		out := `{"count": 3, "results": [`
		first := true
		for i := offset; i < len(devices) && i < offset+limit; i++ {
			if !first {
				out += ","
			}
			first = false
			out += fmt.Sprintf(`{"name": %q, "status": {"value": "active"}, "primary_ip4": {"id": %d}}`, devices[i], i+1)
		}
		out += `]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, out)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "test-token", 2, 4, 0, time.Millisecond, time.Millisecond)
	require.NoError(t, err, "client construction should succeed")

	got, err := c.FetchDevices(context.Background())
	require.NoError(t, err, "FetchDevices should succeed")
	require.Len(t, got, 3, "all pages collected")
	assert.Equal(t, "dev-a", got[0].Name, "deterministic page order")
	assert.Equal(t, "dev-c", got[2].Name, "deterministic page order")
}
