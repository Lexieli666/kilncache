package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/config"
)

func testConfig() config.Config {
	c := config.Defaults()
	c.ListenAddr = "127.0.0.1:0"
	c.ShutdownTimeout = 2 * time.Second
	c.Peers = []config.Peer{{Name: c.NodeName, URL: "http://127.0.0.1:0"}}
	c.ReplicaCount = 1
	return c
}

func TestServerServesAndShutsDown(t *testing.T) {
	cfg := testConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	health := NewHealth()
	health.SetReady()

	srv, err := NewServer(cfg, log, health, NewRouter(RouterOptions{
		Node: "node-a", Version: "test", Health: health, Log: log,
	}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	url := "http://" + srv.Addr() + "/healthz"
	waitFor(t, func() bool {
		resp, err := http.Get(url) //nolint:noctx // short-lived probe in a test
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if ready, _ := health.Ready(); ready {
		t.Error("node still reports ready after shutdown")
	}
}

// TestServerReportsBindFailure is the falsifier for "a node that cannot bind
// fails fast instead of pretending to be up".
func TestServerReportsBindFailure(t *testing.T) {
	cfg := testConfig()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	health := NewHealth()

	first, err := NewServer(cfg, log, health, http.NotFoundHandler())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer first.Close()

	cfg.ListenAddr = first.Addr()
	if _, err := NewServer(cfg, log, health, http.NotFoundHandler()); err == nil {
		t.Fatal("second NewServer on a taken port = nil error, want error")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 10s")
}
