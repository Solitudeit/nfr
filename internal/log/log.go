// Package log configures the process wide structured logger.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func openLogWriter(path string) io.Writer {
	var w io.Writer = os.Stderr
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			w = io.MultiWriter(os.Stderr, f)
		}
	}
	return w
}

// New builds a slog logger writing text records to stderr and, when path is
// non-empty, appending the same records to a file.
func New(level, path string) *slog.Logger {
	lv := parseLevel(level)
	w := openLogWriter(path)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv}))
}

// NewClient builds a human-readable logger for nfrc using the format:
//
//	2026-11-25 14:36:06.821 [I] message
func NewClient(level, path string) *slog.Logger {
	return NewClientWriter(level, openLogWriter(path))
}

// NewClientWriter builds a client-formatted logger writing to w.
func NewClientWriter(level string, w io.Writer) *slog.Logger {
	return slog.New(&clientHandler{
		w:     w,
		level: parseLevel(level),
		mu:    &sync.Mutex{},
	})
}

type clientHandler struct {
	w     io.Writer
	level slog.Level
	mu    *sync.Mutex
	attrs []slog.Attr
}

func (h *clientHandler) Enabled(_ context.Context, lv slog.Level) bool {
	return lv >= h.level
}

func (h *clientHandler) Handle(_ context.Context, r slog.Record) error {
	tag := "I"
	switch {
	case r.Level >= slog.LevelError:
		tag = "E"
	case r.Level >= slog.LevelWarn:
		tag = "W"
	case r.Level < slog.LevelInfo:
		tag = "D"
	}

	var b strings.Builder
	b.WriteString(r.Time.Format("2006-01-02 15:04:05.000"))
	b.WriteString(" [")
	b.WriteString(tag)
	b.WriteString("] ")
	b.WriteString(r.Message)

	writeAttr := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(fmt.Sprint(a.Value.Any()))
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *clientHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &clientHandler{
		w:     h.w,
		level: h.level,
		mu:    h.mu,
		attrs: merged,
	}
}

func (h *clientHandler) WithGroup(_ string) slog.Handler {
	return h
}
