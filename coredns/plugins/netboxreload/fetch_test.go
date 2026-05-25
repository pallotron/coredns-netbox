package netboxreload

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchZonesFromURL(t *testing.T) {
	const fwdZone = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052501 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host1 IN A 10.0.0.1
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			_, _ = w.Write([]byte(`["db.mycompany.com"]`))
		case "/zones/db.mycompany.com":
			_, _ = w.Write([]byte(fwdZone))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	zones, err := fetchZonesFromURL(srv.URL)
	require.NoError(t, err)
	require.Contains(t, zones, "mycompany.com.")
	assert.Contains(t, zones["mycompany.com."].records, "host1.mycompany.com.")
}

func TestFetchZonesFromURL_FileFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			_, _ = w.Write([]byte(`["db.mycompany.com"]`))
		default:
			// Individual file fetch fails
			http.Error(w, "file not found", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	_, err := fetchZonesFromURL(srv.URL)
	assert.Error(t, err)
}

func TestFetchZonesFromURL_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "oops", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchZonesFromURL(srv.URL)
	assert.Error(t, err)
}
