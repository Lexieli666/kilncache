package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type cacheFixture struct {
	handler http.Handler
	store   *storage.Store
}

// recordingObserver captures what the handler reported, so the metrics
// contract can be asserted directly instead of by scraping an exposition
// format and parsing it back.
type recordingObserver struct {
	mu        sync.Mutex
	requests  []observedRequest
	forwarded []string
	inFlight  int
	maxFlight int
}

type observedRequest struct {
	op, ns, status string
}

func (o *recordingObserver) ObserveRequest(op, ns, status string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, observedRequest{op, ns, status})
}

func (o *recordingObserver) ObserveForwarded(hop string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.forwarded = append(o.forwarded, hop)
}

func (o *recordingObserver) InFlight(string) func() {
	o.mu.Lock()
	o.inFlight++
	if o.inFlight > o.maxFlight {
		o.maxFlight = o.inFlight
	}
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		o.inFlight--
		o.mu.Unlock()
	}
}

func (o *recordingObserver) snapshot() ([]observedRequest, []string, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	reqs := append([]observedRequest(nil), o.requests...)
	fwd := append([]string(nil), o.forwarded...)
	return reqs, fwd, o.inFlight
}

func newCacheFixture(t *testing.T, maxObject int64) *cacheFixture {
	return newCacheFixtureWithObserver(t, maxObject, nil)
}

func newCacheFixtureWithObserver(t *testing.T, maxObject int64, obs RequestObserver) *cacheFixture {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := storage.Open(storage.Options{
		Root:           t.TempDir(),
		MaxObjectBytes: maxObject,
		VerifyReads:    true,
		Logger:         log,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	health := NewHealth()
	health.SetReady()
	h := NewRouter(RouterOptions{
		Node:    "node-a",
		Version: "test",
		Health:  health,
		Log:     log,
		Cache:   NewCacheHandler(&storeBackend{store: store}, log, "node-a", maxObject, obs),
	})
	return &cacheFixture{handler: h, store: store}
}

func (f *cacheFixture) do(t *testing.T, method, path string, body []byte, contentLength int64) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader = http.NoBody
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.ContentLength = contentLength
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *cacheFixture) put(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPut, path, body, int64(len(body)))
}

func TestCASPutThenGet(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := []byte("a compiled object file, notionally")
	key := hashOf(content)

	rec := f.put(t, "/cas/"+key, content)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201; body %q", rec.Code, rec.Body.String())
	}

	rec = f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("GET body = %q, want %q", rec.Body.String(), content)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(content))
	}
	if got := rec.Header().Get("Content-Type"); got != contentTypeOctet {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestHeadReportsSizeWithoutBody(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := bytes.Repeat([]byte("x"), 4096)
	key := hashOf(content)
	f.put(t, "/cas/"+key, content)

	rec := f.do(t, http.MethodHead, "/cas/"+key, nil, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != "4096" {
		t.Errorf("Content-Length = %q, want 4096", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d body bytes", rec.Body.Len())
	}
}

func TestHeadOfMissingIs404(t *testing.T) {
	f := newCacheFixture(t, 0)
	rec := f.do(t, http.MethodHead, "/cas/"+hashOf([]byte("absent")), nil, 0)
	if rec.Code != http.StatusNotFound {
		t.Errorf("HEAD missing = %d, want 404", rec.Code)
	}
}

func TestGetMissingIs404(t *testing.T) {
	f := newCacheFixture(t, 0)
	rec := f.do(t, http.MethodGet, "/cas/"+hashOf([]byte("absent")), nil, 0)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET missing = %d, want 404", rec.Code)
	}
}

// TestMalformedKeyIsAMissNotAnError encodes a real property of Bazel: a non-404
// error response makes Bazel disable the remote cache for the rest of the
// invocation. A malformed key must therefore read as a miss, not as a failure.
func TestMalformedKeyIsAMissNotAnError(t *testing.T) {
	f := newCacheFixture(t, 0)
	bad := []string{
		"/cas/short",
		"/cas/" + strings.Repeat("z", 64),
		"/cas/" + strings.Repeat("A", 64),
		"/cas/../../etc/passwd",
		"/cas/",
		"/cas",
		"/blobs/" + hashOf(nil),
		"/cas/" + hashOf(nil) + "/extra",
	}
	for _, path := range bad {
		rec := f.do(t, http.MethodGet, path, nil, 0)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 so Bazel keeps the cache enabled", path, rec.Code)
		}
	}
}

// TestMalformedKeyOnPutIsAClientError is the other side: a client writing to a
// nonsense key has a bug and should be told, since nothing it retries will work.
func TestMalformedKeyOnPutIsAClientError(t *testing.T) {
	f := newCacheFixture(t, 0)
	rec := f.put(t, "/cas/not-a-valid-key", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT to a malformed key = %d, want 400", rec.Code)
	}
}

// TestDigestMismatchIsRejectedAndNotVisible is the headline Phase 1 falsifier.
func TestDigestMismatchIsRejectedAndNotVisible(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := []byte("these are the real bytes")
	wrongKey := hashOf([]byte("but this is a different key"))

	rec := f.put(t, "/cas/"+wrongKey, content)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a mismatched digest = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "digest mismatch") {
		t.Errorf("response does not explain the rejection: %q", rec.Body.String())
	}

	rec = f.do(t, http.MethodGet, "/cas/"+wrongKey, nil, 0)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rejected object is readable: %d", rec.Code)
	}
}

func TestACAcceptsUnverifiedBytes(t *testing.T) {
	f := newCacheFixture(t, 0)
	key := hashOf([]byte("some action"))
	content := []byte("an ActionResult that does not hash to the key")

	rec := f.put(t, "/ac/"+key, content)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT /ac = %d, want 201; body %q", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/ac/"+key, nil, 0)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("GET /ac = %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestDuplicatePutIsIdempotent(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := []byte("stored twice")
	key := hashOf(content)

	first := f.put(t, "/cas/"+key, content)
	if first.Code != http.StatusCreated {
		t.Fatalf("first PUT = %d", first.Code)
	}
	second := f.put(t, "/cas/"+key, content)
	if second.Code != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200 (already stored)", second.Code)
	}
	if second.Header().Get("X-Kilncache-Already-Stored") != "true" {
		t.Error("duplicate PUT did not advertise that the object was already stored")
	}

	rec := f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Error("content changed after a duplicate PUT")
	}
}

func TestZeroByteObjectOverHTTP(t *testing.T) {
	f := newCacheFixture(t, 0)
	key := hashOf(nil)

	rec := f.put(t, "/cas/"+key, []byte{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT empty = %d, want 201; body %q", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET empty = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("empty object returned %d bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length = %q, want 0", got)
	}
}

func TestOversizePutIsRejectedFromContentLength(t *testing.T) {
	const limit = 1024
	f := newCacheFixture(t, limit)
	content := bytes.Repeat([]byte("y"), 4096)
	key := hashOf(content)

	rec := f.put(t, "/cas/"+key, content)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize PUT = %d, want 413", rec.Code)
	}
	rec = f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	if rec.Code != http.StatusNotFound {
		t.Errorf("oversize object is readable: %d", rec.Code)
	}
}

func TestUnsupportedMethod(t *testing.T) {
	f := newCacheFixture(t, 0)
	rec := f.do(t, http.MethodDelete, "/cas/"+hashOf(nil), nil, 0)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q", allow)
	}
}

func TestCachePrefixIsAccepted(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := []byte("stored through the /cache prefix")
	key := hashOf(content)

	if rec := f.put(t, "/cache/cas/"+key, content); rec.Code != http.StatusCreated {
		t.Fatalf("PUT /cache/cas = %d, want 201", rec.Code)
	}
	// The same object must be readable through either spelling: Bazel may be
	// configured with or without the prefix, and the two must not be separate
	// namespaces.
	if rec := f.do(t, http.MethodGet, "/cas/"+key, nil, 0); rec.Code != http.StatusOK {
		t.Fatalf("GET /cas after PUT /cache/cas = %d, want 200", rec.Code)
	}
}

func TestPathParsing(t *testing.T) {
	key := hashOf([]byte("k"))
	cases := []struct {
		path    string
		wantNS  storage.Namespace
		wantKey string
		wantOK  bool
	}{
		{"/cas/" + key, storage.NamespaceCAS, key, true},
		{"/ac/" + key, storage.NamespaceAC, key, true},
		{"/cache/cas/" + key, storage.NamespaceCAS, key, true},
		{"/cas/" + key + "/", storage.NamespaceCAS, key, true},
		{"/CAS/" + key, storage.NamespaceCAS, key, true},
		{"/cas/" + key + "/extra", "", "", false},
		{"/cas/" + strings.ToUpper(key), "", "", false},
		{"/cas", "", "", false},
		{"/", "", "", false},
		{"", "", "", false},
		{"/blob/" + key, "", "", false},
	}
	for _, tc := range cases {
		ns, k, ok := parsePath(tc.path)
		if ok != tc.wantOK {
			t.Errorf("parsePath(%q) ok = %v, want %v", tc.path, ok, tc.wantOK)
			continue
		}
		if ok && (ns != tc.wantNS || k != tc.wantKey) {
			t.Errorf("parsePath(%q) = %v,%s want %v,%s", tc.path, ns, k, tc.wantNS, tc.wantKey)
		}
	}
}

// TestConcurrentPutsOverHTTP runs the same-key race through the full handler,
// which is where a shared buffer or a shared temp name would show up.
func TestConcurrentPutsOverHTTP(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := bytes.Repeat([]byte("z"), 128<<10)
	key := hashOf(content)

	const writers = 16
	var wg sync.WaitGroup
	codes := make([]int, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPut, "/cas/"+key, bytes.NewReader(content))
			req.ContentLength = int64(len(content))
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusCreated && c != http.StatusOK {
			t.Errorf("writer %d: status %d", i, c)
		}
	}
	rec := f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatal("content is wrong after concurrent PUTs through the handler")
	}
}

func TestNamespacesDoNotCollide(t *testing.T) {
	f := newCacheFixture(t, 0)
	content := []byte("payload")
	key := hashOf(content)

	f.put(t, "/cas/"+key, content)
	f.put(t, "/ac/"+key, []byte("a totally different action result"))

	casRec := f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	acRec := f.do(t, http.MethodGet, "/ac/"+key, nil, 0)
	if !bytes.Equal(casRec.Body.Bytes(), content) {
		t.Error("the AC write clobbered the CAS object")
	}
	if bytes.Equal(acRec.Body.Bytes(), content) {
		t.Error("the AC read returned the CAS object")
	}
}

func TestStatusForEachErrorClass(t *testing.T) {
	f := newCacheFixture(t, 64)
	key := hashOf([]byte("x"))

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		length int64
		want   int
	}{
		{"digest mismatch", http.MethodPut, "/cas/" + key, []byte("not x"), 5, http.StatusBadRequest},
		{"too large", http.MethodPut, "/cas/" + key, bytes.Repeat([]byte("q"), 128), 128, http.StatusRequestEntityTooLarge},
		{"short body", http.MethodPut, "/ac/" + key, []byte("ab"), 10, http.StatusBadRequest},
		{"missing get", http.MethodGet, "/cas/" + hashOf([]byte("nope")), nil, 0, http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := f.do(t, tc.method, tc.path, tc.body, tc.length)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body %q)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestLargeObjectOverHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-MiB HTTP object test in short mode")
	}
	f := newCacheFixture(t, 0)
	content := make([]byte, 8<<20)
	for i := range content {
		content[i] = byte(i * 31)
	}
	key := hashOf(content)

	if rec := f.put(t, "/cas/"+key, content); rec.Code != http.StatusCreated {
		t.Fatalf("PUT 8 MiB = %d", rec.Code)
	}
	rec := f.do(t, http.MethodGet, "/cas/"+key, nil, 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 8 MiB = %d", rec.Code)
	}
	if got := hashOf(rec.Body.Bytes()); got != key {
		t.Fatalf("8 MiB round trip digest = %s, want %s", got, key)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Errorf("Content-Length = %q, want %d", got, len(content))
	}
}

// TestMetricsObserveEveryRequest is the falsifier for the claim that the
// dashboard shows what the cache did. A status class that never reaches the
// metrics is a status class nobody sees during an incident.
func TestMetricsObserveEveryRequest(t *testing.T) {
	obs := &recordingObserver{}
	f := newCacheFixtureWithObserver(t, 64, obs)

	content := []byte("metrics")
	key := hashOf(content)

	f.put(t, "/cas/"+key, content)                               // 201
	f.do(t, http.MethodGet, "/cas/"+key, nil, 0)                 // 200
	f.do(t, http.MethodGet, "/cas/"+hashOf([]byte("x")), nil, 0) // 404
	f.put(t, "/cas/"+hashOf([]byte("y")), content)               // 400, digest mismatch
	f.put(t, "/cas/"+key, bytes.Repeat([]byte("z"), 512))        // 413, too large

	reqs, fwd, inFlight := obs.snapshot()
	if len(reqs) != 5 {
		t.Fatalf("observed %d requests, want 5: %+v", len(reqs), reqs)
	}
	if inFlight != 0 {
		t.Errorf("in-flight gauge left at %d after all requests completed", inFlight)
	}
	if len(fwd) != 5 {
		t.Errorf("observed %d hop labels, want 5", len(fwd))
	}

	gotStatus := map[string]bool{}
	for _, r := range reqs {
		gotStatus[r.status] = true
		if r.ns != "cas" {
			t.Errorf("namespace label = %q, want cas", r.ns)
		}
		if r.op != "get" && r.op != "put" {
			t.Errorf("op label = %q", r.op)
		}
	}
	for _, want := range []string{"201", "200", "404", "400", "413"} {
		if !gotStatus[want] {
			t.Errorf("status %s never reached the metrics; observed %+v", want, reqs)
		}
	}
}

// TestMetricsRecordHopRole: a forwarded request must be distinguishable from a
// client one, or peer traffic and client traffic are indistinguishable on the
// dashboard.
func TestMetricsRecordHopRole(t *testing.T) {
	obs := &recordingObserver{}
	f := newCacheFixtureWithObserver(t, 0, obs)

	content := []byte("hop")
	key := hashOf(content)

	req := httptest.NewRequest(http.MethodPut, "/cas/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req.Header.Set(protocol.HeaderForwardedBy, "node-b")
	req.Header.Set(protocol.HeaderHop, string(protocol.HopReplica))
	f.handler.ServeHTTP(httptest.NewRecorder(), req)

	f.do(t, http.MethodGet, "/cas/"+key, nil, 0)

	_, fwd, _ := obs.snapshot()
	if len(fwd) != 2 {
		t.Fatalf("observed %d hop labels, want 2: %v", len(fwd), fwd)
	}
	if fwd[0] != string(protocol.HopReplica) {
		t.Errorf("forwarded PUT recorded hop %q, want %q", fwd[0], protocol.HopReplica)
	}
	if fwd[1] != string(protocol.HopClient) {
		t.Errorf("client GET recorded hop %q, want the client hop", fwd[1])
	}
}
