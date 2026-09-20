package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/Lexieli666/kilncache/internal/protocol"
)

// Header names are defined once, in internal/protocol, because both the front
// door and the peer client have to agree on them exactly. Aliases keep this
// package's call sites readable.
const (
	HeaderForwardedBy = protocol.HeaderForwardedBy
	HeaderNode        = protocol.HeaderNode
	HeaderSource      = protocol.HeaderSource
	HeaderHop         = protocol.HeaderHop
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

	// Cache paths are routed ahead of the mux rather than registered on it.
	//
	// ServeMux path-cleans before matching, so "/cas/../../etc/passwd" becomes
	// a 301 redirect to "/etc/passwd" and "/cas" becomes a 301 to "/cas/".
	// Redirects are the correct general behaviour, and the wrong behaviour
	// here: Bazel treats any non-404 failure from the cache as a reason to
	// disable the remote cache for the rest of the invocation, so a redirect on
	// a garbled key costs a whole build its cache. Routing the prefix ourselves
	// means every malformed cache path is answered as a plain miss.
	if opts.Cache != nil {
		h = routeCache(opts.Cache, h)
	}

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

// routeCache dispatches anything under the cache prefixes to the cache handler,
// without ServeMux's path cleaning. Bazel's HTTP cache is conventionally
// mounted at the root, and some configurations add a /cache/ prefix; both
// spellings address the same objects.
func routeCache(cache, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCachePath(r.URL.Path) {
			cache.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isCachePath(p string) bool {
	s := strings.TrimPrefix(p, "/")
	s = strings.TrimPrefix(s, "cache/")
	switch {
	case s == "cas", s == "ac":
		return true
	case strings.HasPrefix(s, "cas/"), strings.HasPrefix(s, "ac/"):
		return true
	default:
		return false
	}
}
