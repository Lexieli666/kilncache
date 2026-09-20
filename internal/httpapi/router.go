package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
)

// Protocol headers. HeaderForwardedBy carries the name of the node that
// forwarded a request; a node that sees its own name, or any value at all on an
// internal path, refuses to forward again. That single header is what keeps a
// three-node cluster from turning one client request into an infinite loop when
// two nodes disagree about placement.
const (
	HeaderForwardedBy = "X-Kilncache-Forwarded-By"
	HeaderNode        = "X-Kilncache-Node"
	HeaderSource      = "X-Kilncache-Source"
	HeaderReplicaOf   = "X-Kilncache-Replica-Of"
)

// RouterOptions collects everything the router needs to wire endpoints.
type RouterOptions struct {
	Node    string
	Version string
	Health  *Health
	Log     *slog.Logger
	DevMode bool

	// Cache is the object-protocol handler. It is nil during the scaffold
	// phase, where only the health endpoints exist.
	Cache http.Handler

	// Metrics serves the Prometheus exposition format when non-nil.
	Metrics http.Handler
}

// NewRouter builds the node's mux.
func NewRouter(opts RouterOptions) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", opts.Health.HealthzHandler(opts.Node, opts.Version))
	mux.HandleFunc("GET /readyz", opts.Health.ReadyzHandler(opts.Node, opts.Version))

	if opts.Cache != nil {
		mux.Handle("/cas/", opts.Cache)
		mux.Handle("/ac/", opts.Cache)
		// Bazel's HTTP cache has historically been configured with a URL
		// prefix; support the bare /cache/ prefix Bazel uses for the combined
		// layout so that --remote_cache=http://host:8080 works unmodified.
		mux.Handle("/cache/", opts.Cache)
	}

	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics)
	}

	if opts.DevMode {
		// pprof is gated behind an explicit flag. Exposing heap and goroutine
		// dumps on an unauthenticated cache port by default would be a real
		// vulnerability, not a convenience.
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}

	// {$} anchors the pattern to the exact root path. A bare "GET /" would
	// overlap every other prefix pattern and make ServeMux panic at
	// registration, which is the correct behaviour on its part: an ambiguous
	// route table is a bug, not a default.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": "kilncache",
			"node":    opts.Node,
			"version": opts.Version,
			"docs":    "https://github.com/Lexieli666/kilncache",
		})
	})

	var h http.Handler = mux
	h = nodeHeader(opts.Node, h)
	h = AccessLog(opts.Log, h)
	h = Recover(opts.Log, h)
	return h
}

// nodeHeader stamps every response with the node that produced it. When a
// three-node integration test reads an object through node B and asserts that
// the bytes came from node A's replica, this header is the evidence.
func nodeHeader(node string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderNode, node)
		next.ServeHTTP(w, r)
	})
}
