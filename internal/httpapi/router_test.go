package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testRouter(t *testing.T, opts RouterOptions) http.Handler {
	t.Helper()
	if opts.Health == nil {
		opts.Health = NewHealth()
		opts.Health.SetReady()
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Node == "" {
		opts.Node = "node-a"
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	return NewRouter(opts)
}

func TestRouterHealthEndpoints(t *testing.T) {
	r := testRouter(t, RouterOptions{})
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
	}
}

func TestRouterStampsNodeHeader(t *testing.T) {
	r := testRouter(t, RouterOptions{Node: "node-c"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if got := rec.Header().Get(HeaderNode); got != "node-c" {
		t.Errorf("%s = %q, want node-c", HeaderNode, got)
	}
}

func TestRouterRoot(t *testing.T) {
	r := testRouter(t, RouterOptions{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["service"] != "kilncache" {
		t.Errorf("service = %q", body["service"])
	}
}

func TestRouterUnknownPathIs404(t *testing.T) {
	r := testRouter(t, RouterOptions{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

// TestPprofOffByDefault is the falsifier for the claim in the README that pprof
// is only reachable in dev mode. If the gate is ever removed, an unauthenticated
// heap dump becomes available on the cache port and this test fails.
func TestPprofOffByDefault(t *testing.T) {
	r := testRouter(t, RouterOptions{DevMode: false})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /debug/pprof/ without dev mode = %d, want 404", rec.Code)
	}
}

func TestPprofOnInDevMode(t *testing.T) {
	r := testRouter(t, RouterOptions{DevMode: true})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /debug/pprof/ in dev mode = %d, want 200", rec.Code)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	h := Recover(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logged.String(), "panic") {
		t.Errorf("panic was not logged: %q", logged.String())
	}
}

func TestAccessLogRecordsStatusAndBytes(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := AccessLog(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", http.NoBody))

	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logged.String())), &line); err != nil {
		t.Fatalf("access log is not JSON: %v (%q)", err, logged.String())
	}
	if line["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", line["status"])
	}
	if line["bytes_out"] != float64(5) {
		t.Errorf("bytes_out = %v, want 5", line["bytes_out"])
	}
}
