package netboxclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
