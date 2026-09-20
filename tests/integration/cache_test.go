//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/config"
)

func TestSingleNodeRoundTrip(t *testing.T) {
	n := startNode(t, "solo", nil)

	sizes := []int{0, 1, 1023, 4096, 1 << 20}
	for _, size := range sizes {
		content := blob(size, int64(size))
		key := sha256hex(content)

		resp, body := put(t, n.URL(), "/cas/"+key, content)
		assertStatus(t, resp.StatusCode, http.StatusCreated, fmt.Sprintf("PUT %d bytes (%s)", size, body))

		resp, got := get(t, n.URL(), "/cas/"+key)
		assertStatus(t, resp.StatusCode, http.StatusOK, fmt.Sprintf("GET %d bytes", size))
		if !bytes.Equal(got, content) {
			t.Errorf("%d-byte object came back different", size)
		}
		if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(size) {
			t.Errorf("%d-byte object: Content-Length = %q", size, cl)
		}
		if nodeHdr := resp.Header.Get("X-Kilncache-Node"); nodeHdr != "solo" {
			t.Errorf("X-Kilncache-Node = %q, want solo", nodeHdr)
		}

		hr := head(t, n.URL(), "/cas/"+key)
		assertStatus(t, hr.StatusCode, http.StatusOK, fmt.Sprintf("HEAD %d bytes", size))
		if cl := hr.Header.Get("Content-Length"); cl != strconv.Itoa(size) {
			t.Errorf("%d-byte HEAD: Content-Length = %q", size, cl)
		}
	}
}

func TestLargeObjectOverRealSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 64 MiB transfer in short mode")
	}
	n := startNode(t, "solo", nil)

	const size = 64 << 20
	content := blob(size, 2024)
	key := sha256hex(content)

	resp, body := put(t, n.URL(), "/cas/"+key, content)
	assertStatus(t, resp.StatusCode, http.StatusCreated, "PUT 64 MiB: "+body)

	// Stream the response rather than buffering it, so a regression that made
	// the server buffer whole objects would show up here as memory pressure
	// rather than being masked by the client doing the same thing.
	r, err := httpClient().Get(n.URL() + "/cas/" + key)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer r.Body.Close()
	h := sha256.New()
	nBytes, err := io.Copy(h, r.Body)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if nBytes != size {
		t.Fatalf("read %d bytes, want %d", nBytes, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != key {
		t.Fatalf("64 MiB round trip digest = %s, want %s", got, key)
	}
}

// TestClientDisconnectMidUpload is the integration-level falsifier for "an
// interrupted upload is never visible". Unlike the unit test, this really
// severs a TCP connection halfway through a request body.
func TestClientDisconnectMidUpload(t *testing.T) {
	n := startNode(t, "solo", nil)

	content := blob(4<<20, 99)
	key := sha256hex(content)

	conn := mustDial(t, addrOf(n.URL()))
	req := fmt.Sprintf("PUT /cas/%s HTTP/1.1\r\nHost: kilncache\r\nContent-Length: %d\r\n\r\n", key, len(content))
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	// Send a quarter of the body, then hang up.
	if _, err := conn.Write(content[:len(content)/4]); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The object must never become visible -- not now, and not later once the
	// server notices the connection is gone.
	if resp := head(t, n.URL(), "/cas/"+key); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a severed upload is visible: HEAD returned %d", resp.StatusCode)
	}
	assertNoTempFilesEventually(t, n.DataDir, 10*time.Second)
	if resp := head(t, n.URL(), "/cas/"+key); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a severed upload became visible after cleanup: HEAD returned %d", resp.StatusCode)
	}

	// The node must still be healthy and able to serve.
	good := blob(1024, 1)
	goodKey := sha256hex(good)
	r, body := put(t, n.URL(), "/cas/"+goodKey, good)
	assertStatus(t, r.StatusCode, http.StatusCreated, "PUT after a severed upload: "+body)
}

// TestChunkedUploadWithoutContentLength covers the transfer encoding Bazel uses
// when it does not know the size up front.
func TestChunkedUploadWithoutContentLength(t *testing.T) {
	n := startNode(t, "solo", nil)

	content := blob(300000, 5)
	key := sha256hex(content)

	// ContentLength -1 with a non-*bytes.Reader body makes net/http use chunked
	// transfer encoding.
	req, err := http.NewRequest(http.MethodPut, n.URL()+"/cas/"+key, io.NopCloser(bytes.NewReader(content)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatalf("chunked PUT: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	assertStatus(t, resp.StatusCode, http.StatusCreated, "chunked PUT: "+string(body))

	r, got := get(t, n.URL(), "/cas/"+key)
	assertStatus(t, r.StatusCode, http.StatusOK, "GET after chunked PUT")
	if !bytes.Equal(got, content) {
		t.Error("chunked upload round trip changed the bytes")
	}
}

func TestDigestMismatchOverHTTP(t *testing.T) {
	n := startNode(t, "solo", nil)

	content := blob(2048, 31)
	wrongKey := sha256hex(blob(2048, 32))

	resp, body := put(t, n.URL(), "/cas/"+wrongKey, content)
	assertStatus(t, resp.StatusCode, http.StatusBadRequest, "mismatched PUT")
	if !strings.Contains(body, "digest mismatch") {
		t.Errorf("rejection does not explain itself: %q", body)
	}
	if r := head(t, n.URL(), "/cas/"+wrongKey); r.StatusCode != http.StatusNotFound {
		t.Errorf("rejected object is visible: %d", r.StatusCode)
	}
	assertNoTempFiles(t, n.DataDir)
}

// TestRestartPersistsObjects is the falsifier for "a node restart does not lose
// acknowledged writes".
func TestRestartPersistsObjects(t *testing.T) {
	n := startNode(t, "solo", nil)

	written := map[string][]byte{}
	for i := 0; i < 40; i++ {
		content := blob(1000+i*37, int64(i))
		key := sha256hex(content)
		resp, body := put(t, n.URL(), "/cas/"+key, content)
		assertStatus(t, resp.StatusCode, http.StatusCreated, "PUT "+body)
		written[key] = content
	}

	n.Restart()

	for key, content := range written {
		resp, got := get(t, n.URL(), "/cas/"+key)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("object %s missing after restart: %d", key[:8], resp.StatusCode)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("object %s changed across restart", key[:8])
		}
	}
}

// TestConcurrentClientsSameKey drives the same-key race over real sockets.
func TestConcurrentClientsSameKey(t *testing.T) {
	n := startNode(t, "solo", nil)

	content := blob(512<<10, 77)
	key := sha256hex(content)

	const clients = 32
	var wg sync.WaitGroup
	codes := make([]int, clients)
	start := make(chan struct{})
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, err := http.NewRequest(http.MethodPut, n.URL()+"/cas/"+key, bytes.NewReader(content))
			if err != nil {
				codes[i] = -1
				return
			}
			req.ContentLength = int64(len(content))
			resp, err := httpClient().Do(req)
			if err != nil {
				codes[i] = -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusCreated && c != http.StatusOK {
			t.Errorf("client %d got status %d", i, c)
		}
	}
	_, got := get(t, n.URL(), "/cas/"+key)
	if !bytes.Equal(got, content) {
		t.Fatal("content is wrong after concurrent uploads over real sockets")
	}
}

func TestMalformedRequestsOverHTTP(t *testing.T) {
	n := startNode(t, "solo", nil)

	// Reads of nonsense are misses, so a build keeps its cache.
	for _, p := range []string{
		"/cas/short",
		"/cas/" + strings.Repeat("Z", 64),
		"/cas/..%2f..%2fetc%2fpasswd",
		"/blobs/" + sha256hex(nil),
		"/cas",
	} {
		resp, _ := get(t, n.URL(), p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}

	// Writes of nonsense are client errors, so a broken client is told.
	resp, _ := put(t, n.URL(), "/cas/definitely-not-a-sha256", []byte("x"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT to a malformed key = %d, want 400", resp.StatusCode)
	}
}

func TestOversizeUploadRejected(t *testing.T) {
	n := startNode(t, "solo", func(c *config.Config) {
		c.MaxObjectBytes = 64 << 10
	})

	content := blob(256<<10, 3)
	key := sha256hex(content)
	resp, _ := put(t, n.URL(), "/cas/"+key, content)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize PUT = %d, want 413", resp.StatusCode)
	}
	if r := head(t, n.URL(), "/cas/"+key); r.StatusCode != http.StatusNotFound {
		t.Errorf("oversize object stored anyway: %d", r.StatusCode)
	}
}

func TestACNamespaceIsSeparate(t *testing.T) {
	n := startNode(t, "solo", nil)

	key := sha256hex([]byte("an action"))
	casContent := blob(256, 8)
	casKey := sha256hex(casContent)
	acContent := []byte("serialized ActionResult, not content-addressed")

	if r, b := put(t, n.URL(), "/cas/"+casKey, casContent); r.StatusCode != http.StatusCreated {
		t.Fatalf("CAS PUT = %d: %s", r.StatusCode, b)
	}
	if r, b := put(t, n.URL(), "/ac/"+key, acContent); r.StatusCode != http.StatusCreated {
		t.Fatalf("AC PUT = %d: %s", r.StatusCode, b)
	}

	// Overwrite the AC entry: permitted, and documented in ADR-0002.
	updated := []byte("a newer ActionResult for the same action")
	if r, b := put(t, n.URL(), "/ac/"+key, updated); r.StatusCode != http.StatusCreated {
		t.Fatalf("AC overwrite = %d: %s", r.StatusCode, b)
	}
	if _, got := get(t, n.URL(), "/ac/"+key); !bytes.Equal(got, updated) {
		t.Error("AC overwrite did not take effect")
	}
	if _, got := get(t, n.URL(), "/cas/"+casKey); !bytes.Equal(got, casContent) {
		t.Error("the AC writes disturbed the CAS object")
	}
}

func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	n := startNode(t, "solo", nil)

	content := blob(8<<20, 123)
	key := sha256hex(content)
	if r, b := put(t, n.URL(), "/cas/"+key, content); r.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d: %s", r.StatusCode, b)
	}

	// Start a slow read, then shut down while it is in flight. The read must
	// complete with correct bytes rather than being cut off.
	resp, err := httpClient().Get(n.URL() + "/cas/" + key)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, 4096)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		n.Stop()
		close(stopped)
	}()

	time.Sleep(100 * time.Millisecond)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest of body during shutdown: %v", err)
	}
	got := append(first, rest...)
	if !bytes.Equal(got, content) {
		t.Fatalf("in-flight read was truncated during shutdown: got %d bytes, want %d", len(got), len(content))
	}

	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}
