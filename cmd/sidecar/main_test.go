package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

func TestWebhookPollAllowed(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	min := 5 * time.Second

	tests := []struct {
		name string
		last time.Time
		now  time.Time
		want bool
	}{
		{"zero value last always allows the first poll", time.Time{}, now, true},
		{"exactly at the cooldown boundary allows", now.Add(-min), now, true},
		{"just inside the cooldown window is blocked", now.Add(-min + time.Second), now, false},
		{"well within the cooldown window is blocked", now.Add(-time.Second), now, false},
		{"well past the cooldown window allows", now.Add(-2 * min), now, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, webhookPollAllowed(tt.last, tt.now, min))
		})
	}
}
