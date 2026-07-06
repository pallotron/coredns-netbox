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

	zones, _, err := fetchZonesFromURL(srv.URL, nil)
	require.NoError(t, err)
	require.Contains(t, zones, "mycompany.com.")
	assert.Contains(t, zones["mycompany.com."].records, "host1.mycompany.com.")
}

func TestFetchZonesFromURL_ReusesUnchangedZones(t *testing.T) {
	const zoneV1 = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052501 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host1 IN A 10.0.0.1
`
	const zoneV2 = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052502 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host2 IN A 10.0.0.2
`
	lastMod := "Mon, 06 Jul 2026 10:00:00 GMT"
	content := zoneV1
	fullFetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			_, _ = w.Write([]byte(`["db.mycompany.com"]`))
		case "/zones/db.mycompany.com":
			if r.Header.Get("If-Modified-Since") == lastMod {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			fullFetches++
			w.Header().Set("Last-Modified", lastMod)
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	zones1, cache1, err := fetchZonesFromURL(srv.URL, nil)
	require.NoError(t, err)
	require.Contains(t, zones1, "mycompany.com.")
	require.Equal(t, 1, fullFetches)

	t.Run("304 reuses the parsed zone", func(t *testing.T) {
		zones2, cache2, err := fetchZonesFromURL(srv.URL, cache1)
		require.NoError(t, err)
		assert.Same(t, zones1["mycompany.com."], zones2["mycompany.com."],
			"unchanged zone must be reused, not re-parsed")
		assert.Equal(t, 1, fullFetches, "no full fetch on 304")
		cache1 = cache2
	})

	t.Run("changed content is re-fetched and re-parsed", func(t *testing.T) {
		content = zoneV2
		lastMod = "Mon, 06 Jul 2026 10:01:00 GMT"
		zones3, _, err := fetchZonesFromURL(srv.URL, cache1)
		require.NoError(t, err)
		assert.NotSame(t, zones1["mycompany.com."], zones3["mycompany.com."])
		assert.Contains(t, zones3["mycompany.com."].records, "host2.mycompany.com.")
		assert.Equal(t, 2, fullFetches)
	})
}

func TestFetchZonesFromURL_NoValidatorAlwaysFetches(t *testing.T) {
	const fwdZone = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052501 3600 900 604800 86400)
`
	fetches := 0
	// Server sends no Last-Modified — the plugin must fall back to full fetches.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			_, _ = w.Write([]byte(`["db.mycompany.com"]`))
		case "/zones/db.mycompany.com":
			fetches++
			assert.Empty(t, r.Header.Get("If-Modified-Since"),
				"no conditional header without a stored validator")
			_, _ = w.Write([]byte(fwdZone))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, cache, err := fetchZonesFromURL(srv.URL, nil)
	require.NoError(t, err)
	_, _, err = fetchZonesFromURL(srv.URL, cache)
	require.NoError(t, err)
	assert.Equal(t, 2, fetches)
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

	_, _, err := fetchZonesFromURL(srv.URL, nil)
	assert.Error(t, err)
}

func TestFetchZonesFromURL_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "oops", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := fetchZonesFromURL(srv.URL, nil)
	assert.Error(t, err)
}
