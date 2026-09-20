// Package logging builds the process-wide structured logger.
//
// Every log line is JSON so that the chaos runner and the benchmark driver can
// parse server logs mechanically instead of by eye. The node ID is attached to
// the root logger because in a three-node compose cluster the interleaved
// output of all three nodes is what you actually read.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// Options configures the root logger.
type Options struct {
	Level  string // debug, info, warn, error
	Format string // json or text
	NodeID string
}

// ParseLevel maps a human level name onto a slog.Level. Unknown names fall back
// to info rather than failing: a typo in an env var should not stop a cache node
// from booting.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New returns a logger writing to w.
func New(w io.Writer, opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{Level: ParseLevel(opts.Level)}

	var h slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		h = slog.NewTextHandler(w, handlerOpts)
	} else {
		h = slog.NewJSONHandler(w, handlerOpts)
	}

	l := slog.New(h)
	if opts.NodeID != "" {
		l = l.With(slog.String("node", opts.NodeID))
	}
	return l
}
