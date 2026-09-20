package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// responseRecorder captures the status code and byte count for the access log.
//
// It deliberately does not wrap Flush/Hijack/ReadFrom behind an interface
// assertion dance: KilnCache streams with io.Copy onto the raw ResponseWriter
// for object bodies, and the recorder is only installed around handlers where
// the extra indirection costs nothing measurable. The bytes counter is what the
// benchmark driver cross-checks against the client's own accounting.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// ReadFrom lets io.Copy reach the underlying writer's sendfile path instead of
// falling back to a user-space buffer loop. Without this method the recorder
// hides the *http.response ReadFrom implementation from io.Copy, which turns
// every large object GET into an extra copy through a 32 KiB buffer.
func (r *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.bytes += n
		return n, err
	}
	n, err := io.Copy(r.ResponseWriter, src)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, so that a
// handler streaming a response can still control when bytes leave.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Status returns the recorded status, defaulting to 200 for handlers that wrote
// nothing and never called WriteHeader.
func (r *responseRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// AccessLog logs one structured line per request. The fields are chosen so that
// the chaos runner can reconstruct what a node did without a tracing system:
// method, path, status, bytes and duration are enough to explain a corrupted
// read or a slow tail if one ever shows up.
func AccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		level := slog.LevelInfo
		switch {
		case rec.Status() >= 500:
			level = slog.LevelError
		case rec.Status() >= 400 && rec.Status() != http.StatusNotFound:
			level = slog.LevelWarn
		case rec.Status() == http.StatusNotFound:
			// A cache miss is the normal case for a cold build, not a problem.
			level = slog.LevelDebug
		}

		log.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.Status()),
			slog.Int64("bytes_out", rec.bytes),
			slog.Int64("bytes_in", r.ContentLength),
			slog.Float64("duration_ms", float64(dur.Microseconds())/1000),
			slog.String("forwarded_by", r.Header.Get(HeaderForwardedBy)),
		)
	})
}

// Recover turns a handler panic into a 500 and a log line rather than taking
// the whole node down. A cache node that dies on one malformed request takes a
// third of the cluster with it.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic in handler",
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec),
				)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
