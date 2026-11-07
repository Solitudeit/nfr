// Package log configures the process wide structured logger.
package log

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds a slog logger writing text records to stderr and, when path is
// non-empty, appending the same records to a file.
func New(level, path string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	var w io.Writer = os.Stderr
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			w = io.MultiWriter(os.Stderr, f)
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv}))
}
