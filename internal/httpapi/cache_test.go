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

func newCacheFixture(t *testing.T, maxObject int64) *cacheFixture {
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
		Cache:   NewCacheHandler(&storeBackend{store: store}, log, "node-a", maxObject),
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
