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

// NewClient builds a human-readable, mobile-friendly logger for nfrc using the
// compact format:
//
//	01:12:36 [I] message
//
// Terminal output renders in crisp ANSI blue (info), yellow (warn), and red
// (error), while optional log files always receive clean plain text without
// escape codes.
func NewClient(level, path string) *slog.Logger {
	var fileW io.Writer
	if path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			fileW = f
		}
	}
	return slog.New(&clientHandler{
		w:     os.Stderr,
		fileW: fileW,
		color: initConsoleColor(),
		level: parseLevel(level),
		mu:    &sync.Mutex{},
	})
}

// NewClientWriter builds a plain (uncolored) client-formatted logger writing to w.
func NewClientWriter(level string, w io.Writer) *slog.Logger {
	return slog.New(&clientHandler{
		w:     w,
		color: false,
		level: parseLevel(level),
		mu:    &sync.Mutex{},
	})
}

// NewClientColorWriter builds a client-formatted logger writing to w with
// explicit ANSI color control (useful for tests and forced-color terminals).
func NewClientColorWriter(level string, w io.Writer, color bool) *slog.Logger {
	return slog.New(&clientHandler{
		w:     w,
		color: color,
		level: parseLevel(level),
		mu:    &sync.Mutex{},
	})
}

type clientHandler struct {
	w     io.Writer
	fileW io.Writer
	color bool
	level slog.Level
	mu    *sync.Mutex
	attrs []slog.Attr
}

func (h *clientHandler) Enabled(_ context.Context, lv slog.Level) bool {
	return lv >= h.level
}

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[90m"
	ansiBlue  = "\x1b[36m"
	ansiWarn  = "\x1b[33m"
	ansiErr   = "\x1b[91m"
	ansiDebug = "\x1b[90m"
)

func (h *clientHandler) Handle(_ context.Context, r slog.Record) error {
	tag := "I"
	clr := ansiBlue
	switch {
	case r.Level >= slog.LevelError:
		tag = "E"
		clr = ansiErr
	case r.Level >= slog.LevelWarn:
		tag = "W"
		clr = ansiWarn
	case r.Level < slog.LevelInfo:
		tag = "D"
		clr = ansiDebug
	}

	prefix := r.Time.Format("15:04:05") + " [" + tag + "] "
	const indent = "  "

	var plain strings.Builder
	lines := strings.Split(r.Message, "\n")
	for i, line := range lines {
		if i == 0 {
			plain.WriteString(prefix)
		} else {
			plain.WriteString("\n")
			plain.WriteString(indent)
		}
		plain.WriteString(line)
	}

	writeAttr := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		plain.WriteByte(' ')
		plain.WriteString(a.Key)
		plain.WriteByte('=')
		plain.WriteString(fmt.Sprint(a.Value.Any()))
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})
	plain.WriteByte('\n')
	plainStr := plain.String()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fileW != nil {
		_, _ = io.WriteString(h.fileW, plainStr)
	}
	if h.color {
		// Layer ANSI colors: dimmed timestamp, level-tinted tag, and crisp text
		// (warning/error lines stay highlighted in full for quick scanning).
		ts := r.Time.Format("15:04:05")
		body := strings.TrimPrefix(strings.TrimSuffix(plainStr, "\n"), prefix)
		var colored strings.Builder
		colored.WriteString(ansiDim)
		colored.WriteString(ts)
		colored.WriteString(ansiReset)
		colored.WriteByte(' ')
		colored.WriteString(clr)
		colored.WriteByte('[')
		colored.WriteString(tag)
		colored.WriteString("]")
		if r.Level >= slog.LevelWarn || r.Level < slog.LevelInfo {
			colored.WriteByte(' ')
			colored.WriteString(body)
			colored.WriteString(ansiReset)
		} else {
			colored.WriteString(ansiReset)
			colored.WriteByte(' ')
			colored.WriteString(body)
		}
		colored.WriteByte('\n')
		_, err := io.WriteString(h.w, colored.String())
		return err
	}
	_, err := io.WriteString(h.w, plainStr)
	return err
}

func (h *clientHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &clientHandler{
		w:     h.w,
		fileW: h.fileW,
		color: h.color,
		level: h.level,
		mu:    h.mu,
		attrs: merged,
	}
}

func (h *clientHandler) WithGroup(_ string) slog.Handler {
	return h
}
