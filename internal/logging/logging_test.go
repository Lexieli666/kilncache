package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		"info":     slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"":         slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestNewEmitsJSONWithNode is the falsifier for the claim "server logs are
// machine-readable JSON carrying the node ID": if the handler ever reverts to
// text, json.Unmarshal fails here.
func TestNewEmitsJSONWithNode(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: "info", Format: "json", NodeID: "node-a"})
	l.Info("hello", slog.Int("size", 7))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	if rec["node"] != "node-a" {
		t.Errorf("node = %v, want node-a", rec["node"])
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["size"] != float64(7) {
		t.Errorf("size = %v, want 7", rec["size"])
	}
}

func TestNewBelowLevelIsSuppressed(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, Options{Level: "warn", Format: "json"})
	l.Info("should not appear")
	if buf.Len() != 0 {
		t.Errorf("info line emitted at warn level: %q", buf.String())
	}
}
