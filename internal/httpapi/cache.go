package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Lexieli666/kilncache/internal/storage"
)

// ObjectStore is the storage surface the HTTP layer needs. It is an interface
// so that the handler can be tested against a store that fails on demand, and
// so that Phase 2's replicating store can be substituted without touching this
// file.
type ObjectStore interface {
	Put(ctx context.Context, ns storage.Namespace, key string, r io.Reader, declaredSize int64) (storage.PutResult, error)
	Get(ns storage.Namespace, key string) (*storage.Object, error)
	Stat(ns storage.Namespace, key string) (storage.Stat, error)
}

// CacheHandler serves Bazel's HTTP remote cache protocol.
//
// Protocol notes live in docs/protocol.md. The short version: Bazel issues
// GET/HEAD/PUT against /ac/<hash> and /cas/<hash>, treats 200 as a hit, 404 as
// a miss, and any other status as an error that disables the cache for the
// invocation. That last behaviour is why this handler is careful to return 404
// and not 400 for a key it cannot parse: a malformed key is a miss, not a
// reason to turn the cache off for a whole build.
type CacheHandler struct {
	store ObjectStore
	log   *slog.Logger

	// maxObjectBytes mirrors the store's limit so the handler can reject an
	// oversize upload from the Content-Length alone, before reading a byte.
	maxObjectBytes int64
}

// NewCacheHandler builds the object-protocol handler.
func NewCacheHandler(store ObjectStore, log *slog.Logger, maxObjectBytes int64) *CacheHandler {
	return &CacheHandler{store: store, log: log, maxObjectBytes: maxObjectBytes}
}

const contentTypeOctet = "application/octet-stream"

func (h *CacheHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ns, key, ok := parsePath(r.URL.Path)
	if !ok {
		// Unparseable path: treat as a miss for GET/HEAD so a build continues,
		// and as a client error for PUT so a broken client is told.
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			httpError(w, http.StatusBadRequest, "malformed cache path")
			return
		}
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.serveGet(w, r, ns, key, true)
	case http.MethodHead:
		h.serveGet(w, r, ns, key, false)
	case http.MethodPut, http.MethodPost:
		h.servePut(w, r, ns, key)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// parsePath splits /cas/<hash> or /ac/<hash>, tolerating the /cache/ prefix
// that some Bazel configurations use and a trailing slash.
//
// It returns ok=false rather than an error because every caller does the same
// thing with a failure, and because the distinction between "not a cache path"
// and "a cache path with a bad key" is not one the protocol makes.
func parsePath(p string) (storage.Namespace, string, bool) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimPrefix(p, "cache/")

	nsPart, keyPart, found := strings.Cut(p, "/")
	if !found {
		return "", "", false
	}
	// Anything after the key is not part of this protocol.
	if strings.Contains(keyPart, "/") {
		return "", "", false
	}
	ns, err := storage.ParseNamespace(nsPart)
	if err != nil {
		return "", "", false
	}
	if storage.ValidateKey(keyPart) != nil {
		return "", "", false
	}
	return ns, keyPart, true
}

func (h *CacheHandler) serveGet(w http.ResponseWriter, r *http.Request, ns storage.Namespace, key string, withBody bool) {
	start := time.Now()

	if !withBody {
		st, err := h.store.Stat(ns, key)
		if err != nil {
			h.writeGetError(w, r, ns, key, err)
			return
		}
		setObjectHeaders(w, st.Size)
		w.WriteHeader(http.StatusOK)
		return
	}

	obj, err := h.store.Get(ns, key)
	if err != nil {
		h.writeGetError(w, r, ns, key, err)
		return
	}
	defer obj.Close()

	setObjectHeaders(w, obj.Size)
	w.WriteHeader(http.StatusOK)

	n, copyErr := obj.WriteTo(w)
	if copyErr != nil {
		// The status line is already sent, so there is no way to signal the
		// failure in-band. The client sees a body shorter than Content-Length
		// and treats it as an error, which is the correct outcome; all this
		// side can do is record it.
		h.log.Warn("get body truncated",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.Int64("sent", n), slog.Int64("size", obj.Size),
			slog.String("err", copyErr.Error()))
		return
	}

	// Verification happens after the bytes are on the wire, which sounds
	// useless and is not: a failure here means this node has a corrupt local
	// object, and finding that out is what lets repair replace it. The client
	// also verifies -- CAS digests are self-describing -- so a corrupt object
	// is caught on both ends, and the count of these events is what the
	// "zero corrupted reads" claim is measured against.
	if err := obj.Verify(); err != nil {
		h.log.Error("served an object that failed local verification",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		return
	}

	h.log.Debug("cache hit",
		slog.String("ns", ns.String()), slog.String("key", key),
		slog.Int64("size", obj.Size),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000))
}

func (h *CacheHandler) writeGetError(w http.ResponseWriter, r *http.Request, ns storage.Namespace, key string, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		h.log.Debug("cache miss", slog.String("ns", ns.String()), slog.String("key", key))
		http.NotFound(w, r)
	case errors.Is(err, storage.ErrBadKey), errors.Is(err, storage.ErrBadNamespace):
		http.NotFound(w, r)
	case errors.Is(err, storage.ErrClosed):
		httpError(w, http.StatusServiceUnavailable, "node is shutting down")
	default:
		h.log.Error("get failed",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		httpError(w, http.StatusInternalServerError, "read failed")
	}
}

func (h *CacheHandler) servePut(w http.ResponseWriter, r *http.Request, ns storage.Namespace, key string) {
	if r.Body == nil {
		httpError(w, http.StatusBadRequest, "missing body")
		return
	}
	defer r.Body.Close()

	// Reject on the declared length before reading anything. A client that
	// announces a 10 GiB upload should be told no immediately, not after ten
	// gigabytes have crossed the network.
	if h.maxObjectBytes > 0 && r.ContentLength > h.maxObjectBytes {
		h.drainBody(r)
		httpError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("object exceeds the per-object limit of %d bytes", h.maxObjectBytes))
		return
	}

	res, err := h.store.Put(r.Context(), ns, key, r.Body, r.ContentLength)
	if err != nil {
		h.writePutError(w, r, ns, key, err)
		return
	}

	w.Header().Set(HeaderSource, "local")
	if res.AlreadyStored {
		w.Header().Set("X-Kilncache-Already-Stored", "true")
	}
	// Bazel accepts any 2xx. 201 for a new object and 200 for one that was
	// already present is more informative than 200 for both, and costs nothing.
	if res.AlreadyStored {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}

	h.log.Debug("stored",
		slog.String("ns", ns.String()), slog.String("key", key),
		slog.Int64("size", res.Size),
		slog.Bool("already_stored", res.AlreadyStored),
		slog.Bool("durable", res.Durable))
}

func (h *CacheHandler) writePutError(w http.ResponseWriter, r *http.Request, ns storage.Namespace, key string, err error) {
	switch {
	case errors.Is(err, storage.ErrDigestMismatch):
		// 400: the client sent bytes that do not match the key it chose. This
		// is a client bug or a corrupted transfer, and retrying unchanged will
		// not help.
		h.log.Warn("rejected digest mismatch",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrTooLarge):
		httpError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, storage.ErrShortWrite):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrBadKey), errors.Is(err, storage.ErrBadNamespace):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, storage.ErrClosed):
		httpError(w, http.StatusServiceUnavailable, "node is shutting down")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client went away mid-upload. Nothing was published. Writing a
		// status to a closed connection is harmless and keeps the access log
		// honest about what happened.
		h.log.Debug("upload abandoned by client",
			slog.String("ns", ns.String()), slog.String("key", key))
		httpError(w, StatusClientClosedRequest, "client closed the connection")
	case isBodyReadError(err):
		h.log.Debug("upload interrupted",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		httpError(w, http.StatusBadRequest, "upload interrupted")
	default:
		h.log.Error("put failed",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		httpError(w, http.StatusInternalServerError, "write failed")
	}
	_ = r
}

// StatusClientClosedRequest is nginx's 499. Go's http package has no constant
// for it. It appears only in this node's own access log -- by the time it is
// written the client is gone -- and exists so that an abandoned upload is
// visibly different from a server error in the metrics.
const StatusClientClosedRequest = 499

func isBodyReadError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream body:") ||
		strings.Contains(msg, "unexpected EOF") ||
		strings.Contains(msg, "connection reset")
}

func (h *CacheHandler) drainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	// Bounded: a client that was told no does not get to keep sending. 64 KiB
	// is enough to let a small request finish so the connection can be reused;
	// beyond that, closing is cheaper than reading.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
}

func setObjectHeaders(w http.ResponseWriter, size int64) {
	h := w.Header()
	h.Set("Content-Type", contentTypeOctet)
	h.Set("Content-Length", fmt.Sprintf("%d", size))
	// Objects are immutable (CAS) or explicitly overwritten (AC); in neither
	// case should an intermediary revalidate, and in neither case is a stale
	// copy acceptable. no-store is the honest answer for both.
	h.Set("Cache-Control", "no-store")
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}
