package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewGCPHandler_FieldNames(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewGCPHandler(&buf, nil))
	logger.Info("hello world", "key", "value")

	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out), "invalid JSON output %q", buf.String())
	if _, ok := out["severity"]; !ok {
		t.Error("expected 'severity' field, not found")
	}
	if _, ok := out["level"]; ok {
		t.Errorf("unexpected 'level' field in output")
	}
	if _, ok := out["message"]; !ok {
		t.Error("expected 'message' field, not found")
	}
	if _, ok := out["msg"]; ok {
		t.Errorf("unexpected 'msg' field in output")
	}
	if got := out["severity"]; got != "INFO" {
		t.Errorf("severity = %q, want %q", got, "INFO")
	}
	if got := out["message"]; got != "hello world" {
		t.Errorf("message = %q, want %q", got, "hello world")
	}
	if got := out["key"]; got != "value" {
		t.Errorf("key = %q, want %q", got, "value")
	}
}

func TestNewGCPHandler_SeverityMapping(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARNING"},
		{slog.LevelError, "ERROR"},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		logger := slog.New(NewGCPHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logger.Log(context.TODO(), tt.level, "test")

		var out map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &out), "level %v: invalid JSON", tt.level)
		if got := out["severity"]; got != tt.want {
			t.Errorf("level %v: severity = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestNewGCPHandler_PreservesExistingReplaceAttr(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "time" {
				return slog.Attr{} // drop timestamps for deterministic output
			}
			return a
		},
	}
	logger := slog.New(NewGCPHandler(&buf, opts))
	logger.Info("test")

	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out), "invalid JSON")
	if _, ok := out["time"]; ok {
		t.Error("expected 'time' to be dropped by user-provided ReplaceAttr")
	}
	if _, ok := out["severity"]; !ok {
		t.Error("expected 'severity' field still present")
	}
}
