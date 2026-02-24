// Package logging provides a GCP-compatible slog handler.
package logging

import (
	"io"
	"log/slog"
)

// NewGCPHandler returns a slog.Handler that writes JSON logs compatible with
// Google Cloud Logging. It remaps two field names that Cloud Logging requires:
//   - "level"  → "severity"  (with GCP uppercase values: DEBUG/INFO/WARNING/ERROR)
//   - "msg"    → "message"
//
// Any user-supplied ReplaceAttr in opts is applied after the GCP remapping.
func NewGCPHandler(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
	var effective slog.HandlerOptions
	if opts != nil {
		effective = *opts // shallow copy — avoids mutating caller's struct
	}
	userReplace := effective.ReplaceAttr
	effective.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 {
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
				if level, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(gcpSeverity(level))
				}
			case slog.MessageKey:
				a.Key = "message"
			}
		}
		if userReplace != nil {
			return userReplace(groups, a)
		}
		return a
	}
	return slog.NewJSONHandler(w, &effective)
}

func gcpSeverity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
