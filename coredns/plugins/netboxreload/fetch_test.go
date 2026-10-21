package netboxreload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	// The validator is an ETag over the content, matching what the real
	// zone server emits. It is deliberately NOT a timestamp: two writes
	// inside one second share a Last-Modified value, and the second one
	// was answered 304, leaving the client serving a stale zone with no
	// signal that anything was wrong.
	etagFor := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return fmt.Sprintf("%q", hex.EncodeToString(sum[:]))
	}
	content := zoneV1
	fullFetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			_, _ = w.Write([]byte(`["db.mycompany.com"]`))
		case "/zones/db.mycompany.com":
			etag := etagFor(content)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			fullFetches++
			w.Header().Set("ETag", etag)
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

// A zone rewritten twice inside one wall-clock second must reach the plugin.
//
// This is the client half of the silent-update-loss regression. The server
// used to validate with Last-Modified, whose one-second resolution meant the
// second write within a second was answered 304; the plugin then kept the
// previously parsed zone indefinitely, until some unrelated later write
// landed in a different second. DNS answered authoritative NXDOMAIN for a
// record that had been published, and nothing alerted, because the sidecar
// had written the file and staleness stayed healthy.
//
// The fake server here models the real one: it validates on a content hash,
// so two different bodies never share a validator no matter how close
// together they are written.
func TestFetchZonesFromURL_SameSecondRewriteIsPickedUp(t *testing.T) {
	const zoneV1 = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052501 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host1 IN A 10.0.0.1
`
	const zoneV2 = `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052501 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host1 IN A 10.0.0.1
host2 IN A 10.0.0.2
`
	etagFor := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return fmt.Sprintf("%q", hex.EncodeToString(sum[:]))
	}
	content := zoneV1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/zones/":
			_, _ = w.Write([]byte(`["db.mycompany.com"]`))
		case "/zones/db.mycompany.com":
			etag := etagFor(content)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, cache, err := fetchZonesFromURL(srv.URL, nil)
	require.NoError(t, err)

	// No sleep: the rewrite is immediate, exactly the case that used to be
	// swallowed.
	content = zoneV2

	zones, _, err := fetchZonesFromURL(srv.URL, cache)
	require.NoError(t, err)
	require.Contains(t, zones, "mycompany.com.")
	assert.Contains(t, zones["mycompany.com."].records, "host2.mycompany.com.",
		"a record added by a rewrite in the same second must be visible, not swallowed by a 304")
}
