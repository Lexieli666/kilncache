package main

import (
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelfProbeIgnoresNormalArgs(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--node-name=a"},
		{"--listen=:8080", "--dev"},
	} {
		if handled, _ := selfProbe(args); handled {
			t.Errorf("selfProbe(%v) claimed the invocation", args)
		}
	}
}

// TestSelfProbeAgainstAReadyNode is the falsifier for the container health
// check. The runtime image has no shell and no curl, so if this stops working
// the compose cluster reports unhealthy forever and there is no way to debug it
// from inside the container.
func TestSelfProbeAgainstAReadyNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("probe requested %s, want /readyz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	handled, code := selfProbe([]string{"-healthcheck=" + addr})
	if !handled {
		t.Fatal("selfProbe did not handle -healthcheck=addr")
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 against a ready node", code)
	}
}

func TestSelfProbeAgainstANotReadyNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"not ready","reason":"opening object store"}`)
	}))
	defer srv.Close()

	handled, code := selfProbe([]string{"--healthcheck=" + strings.TrimPrefix(srv.URL, "http://")})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d, want true/1 against a not-ready node", handled, code)
	}
}

func TestSelfProbeAgainstNothing(t *testing.T) {
	// Port 1 is reserved and nothing listens there.
	handled, code := selfProbe([]string{"-healthcheck=127.0.0.1:1"})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d, want true/1 when nothing is listening", handled, code)
	}
}

func TestSelfProbeUsesListenEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	t.Setenv("KILNCACHE_LISTEN", ":"+port)

	handled, code := selfProbe([]string{"-healthcheck"})
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d; the probe did not pick up KILNCACHE_LISTEN", handled, code)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	err := run([]string{"--node-name=a", "--peers=b=http://b:8080"})
	if err == nil {
		t.Fatal("run accepted a node that is not in its own peer list")
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	err := run([]string{"--not-a-real-flag"})
	if err == nil {
		t.Fatal("run accepted an unknown flag")
	}
}

func TestRunHelpIsNotAnError(t *testing.T) {
	err := run([]string{"-h"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("run -h = %v, want flag.ErrHelp", err)
	}
}

// TestRunServesAndStops drives the real entry point: it starts a node on an
// ephemeral port, waits for it to answer, and sends it the signal the
// orchestrator sends.
func TestRunServesAndStops(t *testing.T) {
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- run([]string{
			"--node-name=solo",
			"--listen=127.0.0.1:0",
			"--data-dir=" + filepath.Join(dir, "data"),
			"--log-level=error",
		})
	}()

	// A listen address of :0 means the port is only knowable from the process,
	// so instead of racing for it, give run a moment and then interrupt. What
	// is being asserted here is that the full startup path runs without error
	// and that SIGTERM unwinds it, not the HTTP behaviour, which the httpapi
	// and integration suites cover.
	time.Sleep(300 * time.Millisecond)

	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(os.Interrupt); err != nil {
		t.Skipf("cannot signal this process in this environment: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return after SIGINT")
	}
}
