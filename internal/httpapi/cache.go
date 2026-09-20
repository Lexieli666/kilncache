package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// CacheHandler serves Bazel's HTTP remote cache protocol.
//
// Protocol notes live in docs/protocol.md. The short version: Bazel issues
// GET/HEAD/PUT against /ac/<hash> and /cas/<hash>, treats 200 as a hit, 404 as
// a miss, and any other status as an error that disables the remote cache for
// the rest of the invocation. That last behaviour is why this handler returns
// 404, not 400, for a key it cannot parse: a malformed key costs one object,
// and a 400 would cost a whole build its cache.
type CacheHandler struct {
	backend protocol.Backend
	log     *slog.Logger
	node    string

	// maxObjectBytes mirrors the store's limit so an oversize upload can be
	// refused from the Content-Length alone, before a byte is read.
	maxObjectBytes int64
}

// NewCacheHandler builds the object-protocol handler.
func NewCacheHandler(backend protocol.Backend, log *slog.Logger, node string, maxObjectBytes int64) *CacheHandler {
	return &CacheHandler{backend: backend, log: log, node: node, maxObjectBytes: maxObjectBytes}
}

const contentTypeOctet = "application/octet-stream"

func (h *CacheHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ns, key, ok := parsePath(r.URL.Path)
	if !ok {
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			httpError(w, http.StatusBadRequest, "malformed cache path")
			return
		}
		http.NotFound(w, r)
		return
	}

	hop := inboundHop(r)

	switch r.Method {
	case http.MethodGet:
		h.serveGet(w, r, ns, key, hop, true)
	case http.MethodHead:
		h.serveGet(w, r, ns, key, hop, false)
	case http.MethodPut, http.MethodPost:
		h.servePut(w, r, ns, key, hop)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// inboundHop reads the role another node assigned to this request.
//
// The forwarding header is what makes the role trustworthy: a request with no
// X-Kilncache-Forwarded-By is from a client no matter what it claims its hop
// is, so a client cannot talk this node into skipping replication by setting a
// header. A request that *is* forwarded but names no hop is treated as a
// replica write — terminal — because the conservative reading of an unknown
// peer version is "do not forward further".
func inboundHop(r *http.Request) protocol.Hop {
	if r.Header.Get(protocol.HeaderForwardedBy) == "" {
		return protocol.HopClient
	}
	return protocol.ParseHop(r.Header.Get(protocol.HeaderHop))
}

// parsePath splits /cas/<hash> or /ac/<hash>, tolerating the /cache/ prefix
// some Bazel configurations use and a trailing slash.
func parsePath(p string) (storage.Namespace, string, bool) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimPrefix(p, "cache/")

	nsPart, keyPart, found := strings.Cut(p, "/")
	if !found {
		return "", "", false
	}
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

func (h *CacheHandler) serveGet(w http.ResponseWriter, r *http.Request, ns storage.Namespace, key string, hop protocol.Hop, withBody bool) {
	start := time.Now()

	if !withBody {
		info, err := h.backend.Stat(r.Context(), ns, key, hop)
		if err != nil {
			h.writeGetError(w, r, ns, key, err)
			return
		}
		w.Header().Set(protocol.HeaderSource, info.Source)
		setObjectHeaders(w, info.Size)
		w.WriteHeader(http.StatusOK)
		return
	}

	obj, err := h.backend.Open(r.Context(), ns, key, hop)
	if err != nil {
		h.writeGetError(w, r, ns, key, err)
		return
	}
	defer obj.Close()

	w.Header().Set(protocol.HeaderSource, obj.Source())
	setObjectHeaders(w, obj.Size())
	w.WriteHeader(http.StatusOK)

	n, copyErr := obj.WriteTo(w)
	if copyErr != nil {
		// The status line is already sent, so the failure cannot be signalled
		// in band. The client sees a body shorter than Content-Length and
		// treats it as an error, which is correct; all this side can do is
		// record it.
		h.log.Warn("get body truncated",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.Int64("sent", n), slog.Int64("size", obj.Size()),
			slog.String("source", obj.Source()),
			slog.String("err", copyErr.Error()))
		return
	}

	// Verification happens after the bytes are on the wire, which sounds
	// useless and is not: a failure means this node holds a corrupt object, and
	// finding that out is what lets repair replace it. The client verifies too
	// -- CAS digests are self-describing -- so corruption is caught at both
	// ends, and the count of these events is what "zero corrupted reads" is
	// measured against.
	if err := obj.Verify(); err != nil {
		h.log.Error("served an object that failed local verification",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		return
	}

	h.log.Debug("cache hit",
		slog.String("ns", ns.String()), slog.String("key", key),
		slog.Int64("size", obj.Size()),
		slog.String("source", obj.Source()),
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
	case errors.Is(err, context.Canceled):
		h.log.Debug("read abandoned by client", slog.String("key", key))
		httpError(w, StatusClientClosedRequest, "client closed the connection")
	default:
		// Every holder unreachable lands here. It is deliberately a 503 and not
		// a 404: a partition that looked like a cold cache would make Bazel
		// rebuild everything rather than report a problem.
		h.log.Error("get failed",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.String("err", err.Error()))
		httpError(w, http.StatusServiceUnavailable, "no holder could serve this object")
	}
}

func (h *CacheHandler) servePut(w http.ResponseWriter, r *http.Request, ns storage.Namespace, key string, hop protocol.Hop) {
	if r.Body == nil {
		httpError(w, http.StatusBadRequest, "missing body")
		return
	}
	defer r.Body.Close()

	// Refuse on the declared length before reading anything. A client that
	// announces a 10 GiB upload should be told no immediately, not after ten
	// gigabytes have crossed the network.
	if h.maxObjectBytes > 0 && r.ContentLength > h.maxObjectBytes {
		h.drainBody(r)
		httpError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("object exceeds the per-object limit of %d bytes", h.maxObjectBytes))
		return
	}

	res, err := h.backend.Put(r.Context(), ns, key, r.Body, r.ContentLength, hop)
	if err != nil {
		h.writePutError(w, ns, key, res, err)
		return
	}

	if len(res.Holders) > 0 {
		w.Header().Set(protocol.HeaderHolders, strings.Join(res.Holders, ","))
	}
	w.Header().Set(protocol.HeaderCopies, strconv.Itoa(res.Copies))
	w.Header().Set(protocol.HeaderCopiesWanted, strconv.Itoa(res.Wanted))
	if res.AlreadyStored {
		w.Header().Set(protocol.HeaderAlreadyStored, "true")
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}

	h.log.Debug("stored",
		slog.String("ns", ns.String()), slog.String("key", key),
		slog.Int64("size", res.Size),
		slog.Int("copies", res.Copies),
		slog.String("hop", string(hop)),
		slog.Bool("already_stored", res.AlreadyStored),
		slog.Bool("durable", res.Durable))
}

func (h *CacheHandler) writePutError(w http.ResponseWriter, ns storage.Namespace, key string, res protocol.PutOutcome, err error) {
	switch {
	case errors.Is(err, storage.ErrDigestMismatch):
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
	case errors.Is(err, protocol.ErrInsufficientReplicas):
		// The object may well be on disk here. Saying 201 anyway would mean the
		// replication factor is whatever happened to work, and the repair
		// worker would have no reason to look at this key. 503 tells the client
		// the truth and invites a retry.
		h.log.Warn("could not place the required number of copies",
			slog.String("ns", ns.String()), slog.String("key", key),
			slog.Int("copies", res.Copies), slog.Int("wanted", res.Wanted),
			slog.String("err", err.Error()))
		w.Header().Set(protocol.HeaderCopies, strconv.Itoa(res.Copies))
		w.Header().Set(protocol.HeaderCopiesWanted, strconv.Itoa(res.Wanted))
		httpError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
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
}

// StatusClientClosedRequest is nginx's 499. Go's http package has no constant
// for it. It appears only in this node's own logs and metrics -- by the time it
// is written the client is gone -- and exists so that an abandoned upload is
// visibly different from a server error.
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
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	// Objects are immutable (CAS) or explicitly overwritten (AC); in neither
	// case should an intermediary serve a stale copy.
	h.Set("Cache-Control", "no-store")
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}
