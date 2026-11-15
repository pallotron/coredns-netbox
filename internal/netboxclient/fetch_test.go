package netboxclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimitBodyAllowsExactlyTheCap(t *testing.T) {
	got, err := io.ReadAll(LimitBody(strings.NewReader("12345678"), 8))
	require.NoError(t, err, "a body of exactly the cap is fine")
	assert.Equal(t, "12345678", string(got))
}

func TestLimitBodyRejectsOverTheCap(t *testing.T) {
	_, err := io.ReadAll(LimitBody(strings.NewReader("123456789"), 8))
	require.Error(t, err, "one byte over the cap must fail")
	assert.ErrorIs(t, err, ErrBodyTooLarge)
}

// An error response body is read only up to the cap, so a huge or endless
// error page cannot exhaust memory or flood the logs.
func TestFetchErrorBodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "token", 100, 1, 0, time.Millisecond, time.Millisecond)
	require.NoError(t, err)
	c.maxErrorBodyBytes = 16

	_, err = c.FetchIPAddresses(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "netbox API returned 500")
	assert.NotContains(t, err.Error(), strings.Repeat("x", 17), "the error body must be cut at the cap")
}

// A successful page larger than the cap fails with a clear error instead of
// being buffered, for both the IP and the device fetch.
func TestFetchPageOverCapFails(t *testing.T) {
	big := fmt.Sprintf(`{"count": 1, "results": [{"name": %q, "address": "10.0.0.1/24"}]}`, strings.Repeat("a", 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "token", 100, 1, 0, time.Millisecond, time.Millisecond)
	require.NoError(t, err)
	c.maxPageBodyBytes = 1024

	_, err = c.FetchIPAddresses(context.Background())
	require.Error(t, err, "an oversized IP page must fail the fetch")
	assert.ErrorIs(t, err, ErrBodyTooLarge)

	_, err = c.FetchDevices(context.Background())
	require.Error(t, err, "an oversized device page must fail the fetch")
	assert.ErrorIs(t, err, ErrBodyTooLarge)
}

// The device and VM lists are fetched concurrently. The fake NetBox holds
// every device request until a VM request has arrived, which can only
// happen if the two fetches overlap; run one after the other, the device
// requests time out and the fetch fails.
func TestFetchDevicesFetchesDevicesAndVMsConcurrently(t *testing.T) {
	vmSeen := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case devicesPath:
			select {
			case <-vmSeen:
			case <-time.After(2 * time.Second):
				http.Error(w, "no VM request while a device request was in flight", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"count": 1, "results": [{"name": "dev-1", "primary_ip4": {"id": 1}}]}`)
		case virtualMachinesPath:
			once.Do(func() { close(vmSeen) })
			_, _ = io.WriteString(w, `{"count": 1, "results": [{"name": "vm-1", "primary_ip4": {"id": 2}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "token", 100, 4, 0, time.Millisecond, time.Millisecond)
	require.NoError(t, err)
	devices, err := c.FetchDevices(context.Background())
	require.NoError(t, err, "device and VM fetches must overlap")
	require.Len(t, devices, 2)
	assert.Equal(t, "dev-1", devices[0].Name, "devices still come before VMs")
	assert.Equal(t, "vm-1", devices[1].Name)
}

// However many fetches run at once, NetBox never sees more than
// maxConcurrency requests in flight from one client.
func TestRequestsNeverExceedClientLimit(t *testing.T) {
	var inflight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == ipAddressesPath {
			_, _ = io.WriteString(w, `{"count": 6, "results": [{"address": "10.0.0.1/32"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"count": 6, "results": [{"name": "d"}]}`)
	}))
	defer srv.Close()

	const limit = 2
	c, err := New(srv.URL, "token", 1, limit, 0, time.Millisecond, time.Millisecond) // 6 pages per list
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, err := c.FetchIPAddresses(context.Background()); assert.NoError(t, err) }()
	go func() { defer wg.Done(); _, err := c.FetchDevices(context.Background()); assert.NoError(t, err) }()
	wg.Wait()

	assert.LessOrEqual(t, peak.Load(), int32(limit), "requests in flight must stay within the client limit")
	assert.Equal(t, int32(limit), peak.Load(), "and the limit is actually used")
}
