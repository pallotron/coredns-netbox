package zoneserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pallotron/coredns-netbox/internal/zoneserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZoneServer_List(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte("zone content"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.10.in-addr.arpa"), []byte("reverse zone"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dynamic.json"), []byte("{}"), 0o644))

	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	req := httptest.NewRequest(http.MethodGet, "/zones/", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var files []string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &files))
	assert.Contains(t, files, "db.mycompany.com")
	assert.Contains(t, files, "db.10.in-addr.arpa")
	assert.NotContains(t, files, "dynamic.json") // non-zone files excluded
}

func TestZoneServer_GetFile(t *testing.T) {
	dir := t.TempDir()
	content := "$ORIGIN mycompany.com.\n$TTL 300\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte(content), 0o644))

	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	req := httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, content, rr.Body.String())
}

func TestZoneServer_GetFile_NotFound(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	req := httptest.NewRequest(http.MethodGet, "/zones/db.notexist", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestZoneServer_GetFile_TraversalWithinPrefix(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	// This stays under /zones/ but tries to escape via the filename — exercises
	// the application-level guard (strings.Contains check) not just mux cleaning.
	// We set RawPath so the mux receives an already-decoded path without triggering
	// the redirect that fires when ".." path segments appear in the URL string.
	req := httptest.NewRequest(http.MethodGet, "/zones/db..%2F..%2F..%2Fetc%2Fpasswd", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestZoneServer_GetFile_TraversalRejected(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	req := httptest.NewRequest(http.MethodGet, "/zones/../etc/passwd", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.NotEqual(t, http.StatusOK, rr.Code)
}

// A zone rewritten twice inside one wall-clock second must still be served.
//
// This is the regression test for a silent-update-loss bug. The handler used
// http.ServeFile, which validates with Last-Modified/If-Modified-Since. HTTP
// dates carry whole seconds, and Go truncates the file's mtime to a second
// before comparing, answering 304 when modtime <= the supplied time. So the
// second write inside a second was reported as "not modified" and CoreDNS
// kept serving the previous zone -- not late, invisible, until some unrelated
// later write happened to land in a different second.
//
// Nothing alerted while that happened: the sidecar had written the file, so
// zone staleness stayed healthy while DNS answered authoritative NXDOMAIN.
//
// The two writes here are deliberately not separated by a sleep: the whole
// point is that they share a second. Against the pre-fix handler this test
// fails, returning 304 and the stale body.
func TestZoneServer_SecondWriteWithinSameSecondIsServed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mycompany.com")

	first := "$ORIGIN mycompany.com.\n$TTL 300\nold A 10.0.0.1\n"
	require.NoError(t, os.WriteFile(path, []byte(first), 0o644))

	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	// Initial fetch: note the validator the server hands out.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	etag := rr.Header().Get("ETag")
	require.NotEmpty(t, etag, "server must offer a content validator")
	require.Equal(t, first, rr.Body.String())

	// Rewrite with different content, no sleep: same second as the fetch.
	second := "$ORIGIN mycompany.com.\n$TTL 300\nold A 10.0.0.1\nnew A 10.0.0.2\n"
	require.NoError(t, os.WriteFile(path, []byte(second), 0o644))

	// Conditional re-fetch with the validator from before the rewrite.
	req := httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code,
		"a changed zone must not be answered 304 just because it changed within the same second")
	assert.Equal(t, second, rr.Body.String(), "the new content must be served")
	assert.NotEqual(t, etag, rr.Header().Get("ETag"), "changed content must yield a new validator")
}

// Unchanged content must still be answered 304, or every reload re-parses
// every zone -- the optimisation this validator exists for.
func TestZoneServer_UnchangedContentIsNotModified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mycompany.com")
	content := "$ORIGIN mycompany.com.\n$TTL 300\nhost A 10.0.0.1\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	etag := rr.Header().Get("ETag")

	req := httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotModified, rr.Code)
	assert.Empty(t, rr.Body.String(), "a 304 carries no body")
}

// Rewriting a file with byte-identical content must keep the same validator,
// so a sidecar that rewrites without changing anything costs no re-parsing.
func TestZoneServer_IdenticalRewriteKeepsValidator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mycompany.com")
	content := "$ORIGIN mycompany.com.\n$TTL 300\nhost A 10.0.0.1\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	etag := rr.Header().Get("ETag")

	// Same bytes, new mtime.
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, etag, rr.Header().Get("ETag"),
		"identical content must validate identically regardless of mtime")
}

// The server must not emit Last-Modified: a client that preferred it could
// reintroduce the one-second blind spot this handler was changed to avoid.
func TestZoneServer_NoLastModifiedOffered(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"),
		[]byte("$ORIGIN mycompany.com.\n"), 0o644))

	mux := http.NewServeMux()
	zoneserver.Register(mux, dir)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/zones/db.mycompany.com", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, rr.Header().Get("Last-Modified"),
		"time-based validation is what caused silent update loss; do not offer it")
}
