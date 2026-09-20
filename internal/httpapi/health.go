// Package httpapi implements KilnCache's front door: the subset of Bazel's HTTP
// remote cache protocol that this project supports, plus health, metrics and
// (in dev mode) pprof endpoints.
package httpapi

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Health tracks the two distinct questions an orchestrator asks.
//
// Liveness ("is the process wedged?") and readiness ("should traffic go here?")
// are deliberately separate. During graceful shutdown a node reports not-ready
// immediately so that peers and load balancers stop sending it work, while
// still reporting live until in-flight requests drain. Collapsing the two would
// make every rolling restart drop requests.
type Health struct {
	live    atomic.Bool
	ready   atomic.Bool
	started time.Time

	// readyErr carries the reason readiness is false, so /readyz explains
	// itself instead of returning a bare 503 that an operator has to guess at.
	readyErr atomic.Pointer[string]
}

// NewHealth returns a Health that is live but not yet ready.
func NewHealth() *Health {
	h := &Health{started: time.Now()}
	h.live.Store(true)
	h.SetNotReady("starting up")
	return h
}

// SetReady marks the node as able to serve traffic.
func (h *Health) SetReady() {
	h.readyErr.Store(nil)
	h.ready.Store(true)
}

// SetNotReady marks the node as unable to serve traffic, with a reason.
func (h *Health) SetNotReady(reason string) {
	r := reason
	h.readyErr.Store(&r)
	h.ready.Store(false)
}

// SetDead marks the process as unrecoverable; an orchestrator should restart it.
func (h *Health) SetDead(reason string) {
	h.SetNotReady(reason)
	h.live.Store(false)
}

// Live reports liveness.
func (h *Health) Live() bool { return h.live.Load() }

// Ready reports readiness and, when not ready, the reason.
func (h *Health) Ready() (bool, string) {
	if h.ready.Load() {
		return true, ""
	}
	if r := h.readyErr.Load(); r != nil {
		return false, *r
	}
	return false, "not ready"
}

type healthResponse struct {
	Status  string  `json:"status"`
	Node    string  `json:"node"`
	Version string  `json:"version"`
	UptimeS float64 `json:"uptime_seconds"`
	Reason  string  `json:"reason,omitempty"`
}

// HealthzHandler answers liveness probes.
func (h *Health) HealthzHandler(node, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := healthResponse{
			Status:  "ok",
			Node:    node,
			Version: version,
			UptimeS: time.Since(h.started).Seconds(),
		}
		code := http.StatusOK
		if !h.Live() {
			resp.Status = "dead"
			if reason := h.readyErr.Load(); reason != nil {
				resp.Reason = *reason
			}
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, resp)
	}
}

// ReadyzHandler answers readiness probes.
func (h *Health) ReadyzHandler(node, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ready, reason := h.Ready()
		resp := healthResponse{
			Status:  "ready",
			Node:    node,
			Version: version,
			UptimeS: time.Since(h.started).Seconds(),
		}
		code := http.StatusOK
		if !ready {
			resp.Status = "not ready"
			resp.Reason = reason
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, resp)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	// A failure to write the probe response is the client's problem, not ours;
	// there is nothing useful left to do with the error on a hijacked socket.
	_ = json.NewEncoder(w).Encode(v)
}
