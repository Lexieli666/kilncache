package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthStartsLiveButNotReady(t *testing.T) {
	h := NewHealth()
	if !h.Live() {
		t.Error("new Health is not live")
	}
	ready, reason := h.Ready()
	if ready {
		t.Error("new Health is ready before SetReady")
	}
	if reason == "" {
		t.Error("not-ready state carries no reason")
	}
}

// TestReadyzSeparateFromHealthz is the falsifier for the claim that liveness
// and readiness are independent. If they are ever collapsed, a draining node
// would report dead and be restarted mid-drain, and this test fails.
func TestReadyzSeparateFromHealthz(t *testing.T) {
	h := NewHealth()
	h.SetReady()
	h.SetNotReady("shutting down")

	if !h.Live() {
		t.Error("SetNotReady also cleared liveness")
	}

	rec := httptest.NewRecorder()
	h.HealthzHandler("a", "test").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 while draining", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ReadyzHandler("a", "test").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d, want 503 while draining", rec.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body is not JSON: %v", err)
	}
	if body.Reason != "shutting down" {
		t.Errorf("reason = %q, want %q", body.Reason, "shutting down")
	}
}

func TestSetDeadFailsBothProbes(t *testing.T) {
	h := NewHealth()
	h.SetReady()
	h.SetDead("index is unrecoverable")

	if h.Live() {
		t.Error("still live after SetDead")
	}
	rec := httptest.NewRecorder()
	h.HealthzHandler("a", "test").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz status = %d, want 503 after SetDead", rec.Code)
	}
}

func TestReadyzOKWhenReady(t *testing.T) {
	h := NewHealth()
	h.SetReady()
	rec := httptest.NewRecorder()
	h.ReadyzHandler("node-a", "v1").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body is not JSON: %v", err)
	}
	if body.Node != "node-a" || body.Status != "ready" {
		t.Errorf("body = %+v", body)
	}
	if body.UptimeS < 0 {
		t.Errorf("uptime = %v, want >= 0", body.UptimeS)
	}
}
