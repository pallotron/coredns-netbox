package zonefetch_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pallotron/coredns-netbox/internal/zonefetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchZones(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`["db.mycompany.com","db.10.in-addr.arpa"]`))
		case "/zones/db.mycompany.com":
			_, _ = w.Write([]byte("zone content forward"))
		case "/zones/db.10.in-addr.arpa":
			_, _ = w.Write([]byte("zone content reverse"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	require.NoError(t, zonefetch.FetchZones(srv.URL, dir))

	fwd, err := os.ReadFile(filepath.Join(dir, "db.mycompany.com"))
	require.NoError(t, err)
	assert.Equal(t, "zone content forward", string(fwd))

	rev, err := os.ReadFile(filepath.Join(dir, "db.10.in-addr.arpa"))
	require.NoError(t, err)
	assert.Equal(t, "zone content reverse", string(rev))
}

func TestFetchZones_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := zonefetch.FetchZones(srv.URL, t.TempDir())
	assert.Error(t, err)
}

func TestWaitForSidecar_ConnectionRefused(t *testing.T) {
	// Verify WaitForSidecar retries gracefully when the server is not reachable
	// (e.g. sidecar pod not yet scheduled). Uses a short timeout so the test is fast.
	err := zonefetch.WaitForSidecar("http://127.0.0.1:19999", 200*time.Millisecond, 50*time.Millisecond)
	assert.Error(t, err, "should time out when server is unreachable")
	assert.Contains(t, err.Error(), "not ready")
}

func TestWaitForSidecar_BecomesReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := zonefetch.WaitForSidecar(srv.URL, 2*time.Second, 50*time.Millisecond)
	assert.NoError(t, err)
}
