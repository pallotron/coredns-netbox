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
