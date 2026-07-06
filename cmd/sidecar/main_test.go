package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRegisterHealth(t *testing.T) {
	var ready atomic.Bool
	mux := http.NewServeMux()
	registerHealth(mux, &ready)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		assert.NoError(t, err, "GET %s", path)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	t.Run("livez is 200 before first successful fetch", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, get("/livez"))
	})

	t.Run("healthz is 503 before first successful fetch", func(t *testing.T) {
		assert.Equal(t, http.StatusServiceUnavailable, get("/healthz"))
	})

	t.Run("healthz is 200 once ready", func(t *testing.T) {
		ready.Store(true)
		assert.Equal(t, http.StatusOK, get("/healthz"))
		assert.Equal(t, http.StatusOK, get("/livez"))
	})
}
