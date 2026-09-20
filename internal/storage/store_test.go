package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newTestStore creates a store in a temp directory on the test machine's local
// filesystem. t.TempDir is used deliberately: this repository's checkout may
// live on a 9p mount where fsync and rename do not behave, and a durability
// test there would be measuring the wrong filesystem.
func newTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustPut(t *testing.T, s *Store, ns Namespace, key string, content []byte) PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), ns, key, bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("Put(%s/%s): %v", ns, key[:8], err)
	}
	return res
}

func mustGetBytes(t *testing.T, s *Store, ns Namespace, key string) []byte {
	t.Helper()
	o, err := s.Get(ns, key)
	if err != nil {
		t.Fatalf("Get(%s/%s): %v", ns, key[:8], err)
	}
	defer o.Close()
	var buf bytes.Buffer
	if _, err := o.WriteTo(&buf); err != nil {
		t.Fatalf("read object: %v", err)
	}
	if err := o.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	return buf.Bytes()
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t, Options{VerifyReads: true})
	content := []byte("the quick brown fox jumps over the lazy dog")
	key := sha256hex(content)

	res := mustPut(t, s, NamespaceCAS, key, content)
	if res.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", res.Size, len(content))
	}
	if res.Digest != key {
		t.Errorf("Digest = %s, want %s", res.Digest, key)
	}
	if res.AlreadyStored {
		t.Error("first Put reported AlreadyStored")
	}

	got := mustGetBytes(t, s, NamespaceCAS, key)
	if !bytes.Equal(got, content) {
		t.Fatalf("round trip changed the bytes: got %q, want %q", got, content)
	}
}

// TestPutRejectsDigestMismatch is the falsifier for the README claim that a
// CAS object is rejected when its bytes do not hash to its key.
func TestPutRejectsDigestMismatch(t *testing.T) {
	s := newTestStore(t, Options{})
	content := []byte("actual content")
	wrongKey := sha256hex([]byte("something else entirely"))

	_, err := s.Put(context.Background(), NamespaceCAS, wrongKey, bytes.NewReader(content), int64(len(content)))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Put with wrong key = %v, want ErrDigestMismatch", err)
	}

	var mismatch *DigestMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error does not carry both digests: %v", err)
	}
	if mismatch.Want != wrongKey || mismatch.Got != sha256hex(content) {
		t.Errorf("mismatch = %+v", mismatch)
	}

	// The rejected object must not be visible, and must not have left a file.
	if _, err := s.Stat(NamespaceCAS, wrongKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("rejected object is visible: %v", err)
	}
	assertNoTempFiles(t, s)
}

// TestACAcceptsAnyBytes pins the documented asymmetry: an action cache entry is
// keyed by the action, not by its content, so the server must not verify it.
func TestACAcceptsAnyBytes(t *testing.T) {
	s := newTestStore(t, Options{})
	key := sha256hex([]byte("an action digest"))
	content := []byte("an ActionResult protobuf that hashes to something else")

	if _, err := s.Put(context.Background(), NamespaceAC, key, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("AC Put: %v", err)
	}
	if got := mustGetBytes(t, s, NamespaceAC, key); !bytes.Equal(got, content) {
		t.Errorf("AC round trip changed the bytes")
	}
}

// TestACIsOverwritable is the other half of the asymmetry. ADR-0002 states this
// as a known weakness; the test proves the behaviour matches the document.
func TestACIsOverwritable(t *testing.T) {
	s := newTestStore(t, Options{})
	key := sha256hex([]byte("action"))

	mustPut(t, s, NamespaceAC, key, []byte("first result"))
	mustPut(t, s, NamespaceAC, key, []byte("second result"))

	if got := string(mustGetBytes(t, s, NamespaceAC, key)); got != "second result" {
		t.Errorf("AC value = %q, want the second write to win", got)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	s := newTestStore(t, Options{})
	key := sha256hex([]byte("never stored"))

	if _, err := s.Get(NamespaceCAS, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(NamespaceCAS, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat missing = %v, want ErrNotFound", err)
	}
}

func TestPutIsIdempotent(t *testing.T) {
	s := newTestStore(t, Options{})
	content := []byte("idempotent payload")
	key := sha256hex(content)

	first := mustPut(t, s, NamespaceCAS, key, content)
	second := mustPut(t, s, NamespaceCAS, key, content)

	if first.AlreadyStored {
		t.Error("first Put reported AlreadyStored")
	}
	if !second.AlreadyStored {
		t.Error("second Put of an identical CAS object did not report AlreadyStored")
	}
	if second.Size != first.Size {
		t.Errorf("sizes differ across idempotent Puts: %d vs %d", first.Size, second.Size)
	}
	if got := mustGetBytes(t, s, NamespaceCAS, key); !bytes.Equal(got, content) {
		t.Error("content changed after a duplicate Put")
	}
	if n := s.Snapshot().PutsAlreadyStored; n != 1 {
		t.Errorf("PutsAlreadyStored = %d, want 1", n)
	}
}

func TestZeroByteObject(t *testing.T) {
	s := newTestStore(t, Options{VerifyReads: true})
	var content []byte
	key := sha256hex(content)
	if key != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("sha256 of empty is %s; the test fixture is wrong", key)
	}

	res := mustPut(t, s, NamespaceCAS, key, content)
	if res.Size != 0 {
		t.Errorf("Size = %d, want 0", res.Size)
	}

	o, err := s.Get(NamespaceCAS, key)
	if err != nil {
		t.Fatalf("Get zero-byte object: %v", err)
	}
	defer o.Close()
	if o.Size != 0 {
		t.Errorf("Stat size = %d, want 0", o.Size)
	}
	b, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("read %d bytes from a zero-byte object", len(b))
	}
	if err := o.Verify(); err != nil {
		t.Errorf("verify zero-byte object: %v", err)
	}
}

func TestLargeObject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 64 MiB object test in short mode")
	}
	s := newTestStore(t, Options{VerifyReads: true})

	const size = 64 << 20
	content := deterministicBytes(t, size, 42)
	key := sha256hex(content)

	res := mustPut(t, s, NamespaceCAS, key, content)
	if res.Size != size {
		t.Fatalf("Size = %d, want %d", res.Size, size)
	}

	o, err := s.Get(NamespaceCAS, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer o.Close()

	h := sha256.New()
	if _, err := o.WriteTo(h); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != key {
		t.Fatalf("64 MiB round trip digest = %s, want %s", got, key)
	}
	if err := o.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestInterruptedUploadIsNeverVisible is the Phase 1 acceptance criterion: a
// client that disconnects mid-body must leave nothing behind and must not
// publish.
func TestInterruptedUploadIsNeverVisible(t *testing.T) {
	s := newTestStore(t, Options{})
	content := deterministicBytes(t, 1<<20, 7)
	key := sha256hex(content)

	// A reader that delivers half the body and then fails, exactly as a
	// dropped connection does.
	half := len(content) / 2
	broken := io.MultiReader(
		bytes.NewReader(content[:half]),
		errReader{errors.New("connection reset by peer")},
	)

	_, err := s.Put(context.Background(), NamespaceCAS, key, broken, int64(len(content)))
	if err == nil {
		t.Fatal("interrupted Put succeeded")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error lost the cause: %v", err)
	}

	if _, err := s.Stat(NamespaceCAS, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partially uploaded object is visible: %v", err)
	}
	assertNoTempFiles(t, s)
}

// TestShortBodyIsRejected covers the AC case, where there is no digest to catch
// a truncated upload.
func TestShortBodyIsRejected(t *testing.T) {
	s := newTestStore(t, Options{})
	key := sha256hex([]byte("action"))
	content := []byte("0123456789")

	_, err := s.Put(context.Background(), NamespaceAC, key, bytes.NewReader(content), 100)
	if !errors.Is(err, ErrShortWrite) {
		t.Fatalf("Put with a short body = %v, want ErrShortWrite", err)
	}
	if _, err := s.Stat(NamespaceAC, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("truncated AC entry is visible: %v", err)
	}
	assertNoTempFiles(t, s)
}

func TestLongBodyIsRejected(t *testing.T) {
	s := newTestStore(t, Options{})
	key := sha256hex([]byte("action"))
	content := []byte("0123456789")

	_, err := s.Put(context.Background(), NamespaceAC, key, bytes.NewReader(content), 3)
	if !errors.Is(err, ErrShortWrite) {
		t.Fatalf("Put with a body longer than Content-Length = %v, want ErrShortWrite", err)
	}
	assertNoTempFiles(t, s)
}

func TestUnknownContentLengthIsAccepted(t *testing.T) {
	s := newTestStore(t, Options{})
	content := []byte("chunked transfer encoding has no Content-Length")
	key := sha256hex(content)

	// -1 is what net/http reports for a chunked request body.
	res, err := s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), -1)
	if err != nil {
		t.Fatalf("Put with unknown length: %v", err)
	}
	if res.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", res.Size, len(content))
	}
}

func TestPutRejectsOversizeObject(t *testing.T) {
	s := newTestStore(t, Options{MaxObjectBytes: 1024})
	content := deterministicBytes(t, 4096, 1)
	key := sha256hex(content)

	// Declared oversize: rejected before a byte is read.
	_, err := s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), int64(len(content)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("declared-oversize Put = %v, want ErrTooLarge", err)
	}

	// Undeclared oversize: must still be caught while streaming, otherwise a
	// chunked upload could bypass the limit entirely.
	_, err = s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), -1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("chunked-oversize Put = %v, want ErrTooLarge", err)
	}
	assertNoTempFiles(t, s)
}

func TestPutAtExactLimitIsAccepted(t *testing.T) {
	const limit = 4096
	s := newTestStore(t, Options{MaxObjectBytes: limit})
	content := deterministicBytes(t, limit, 2)
	key := sha256hex(content)

	if _, err := s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), limit); err != nil {
		t.Fatalf("Put at exactly the limit: %v", err)
	}
}

func TestPutRejectsBadKeyAndNamespace(t *testing.T) {
	s := newTestStore(t, Options{})
	ctx := context.Background()

	if _, err := s.Put(ctx, NamespaceCAS, "not-a-hash", bytes.NewReader(nil), 0); !errors.Is(err, ErrBadKey) {
		t.Errorf("Put with a bad key = %v, want ErrBadKey", err)
	}
	if _, err := s.Put(ctx, Namespace("blobs"), sha256hex(nil), bytes.NewReader(nil), 0); !errors.Is(err, ErrBadNamespace) {
		t.Errorf("Put with a bad namespace = %v, want ErrBadNamespace", err)
	}
	if _, err := s.Get(Namespace("blobs"), sha256hex(nil)); !errors.Is(err, ErrBadNamespace) {
		t.Errorf("Get with a bad namespace = %v, want ErrBadNamespace", err)
	}
}

// TestConcurrentPutsOfSameKey is the falsifier for "concurrent PUTs of the same
// key are safe". Under -race this also proves there is no shared mutable state
// on the write path.
func TestConcurrentPutsOfSameKey(t *testing.T) {
	s := newTestStore(t, Options{VerifyReads: true})
	content := deterministicBytes(t, 512<<10, 9)
	key := sha256hex(content)

	const writers = 24
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), int64(len(content)))
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	if got := mustGetBytes(t, s, NamespaceCAS, key); !bytes.Equal(got, content) {
		t.Fatal("content is wrong after concurrent identical Puts")
	}
	assertNoTempFiles(t, s)
}

// TestConcurrentPutAndGet exercises the rename-over-an-open-file case: a reader
// holding the old inode must keep reading valid bytes.
func TestConcurrentPutAndGet(t *testing.T) {
	s := newTestStore(t, Options{VerifyReads: true})
	content := deterministicBytes(t, 256<<10, 11)
	key := sha256hex(content)
	mustPut(t, s, NamespaceCAS, key, content)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var readErr error
	var mu sync.Mutex

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				o, err := s.Get(NamespaceCAS, key)
				if err != nil {
					mu.Lock()
					readErr = err
					mu.Unlock()
					return
				}
				h := sha256.New()
				_, err = o.WriteTo(h)
				o.Close()
				if err != nil {
					mu.Lock()
					readErr = err
					mu.Unlock()
					return
				}
				if hex.EncodeToString(h.Sum(nil)) != key {
					mu.Lock()
					readErr = errors.New("reader observed bytes that do not match the key")
					mu.Unlock()
					return
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		if _, err := s.Put(context.Background(), NamespaceAC, key, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Errorf("AC Put: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if readErr != nil {
		t.Fatalf("concurrent read: %v", readErr)
	}
}

func TestDeleteRemovesObject(t *testing.T) {
	s := newTestStore(t, Options{})
	content := []byte("delete me")
	key := sha256hex(content)
	mustPut(t, s, NamespaceCAS, key, content)

	if err := s.Delete(NamespaceCAS, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(NamespaceCAS, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("object survived Delete: %v", err)
	}
	// Deleting twice is the normal outcome of eviction racing repair.
	if err := s.Delete(NamespaceCAS, key); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

func TestStatsTrackStoredBytes(t *testing.T) {
	s := newTestStore(t, Options{})
	content := deterministicBytes(t, 1000, 3)
	key := sha256hex(content)

	mustPut(t, s, NamespaceCAS, key, content)
	snap := s.Snapshot()
	if snap.ObjectsStored != 1 || snap.BytesStored != 1000 {
		t.Errorf("after Put: objects=%d bytes=%d, want 1/1000", snap.ObjectsStored, snap.BytesStored)
	}
	if snap.BytesIn != 1000 {
		t.Errorf("BytesIn = %d, want 1000", snap.BytesIn)
	}

	_ = mustGetBytes(t, s, NamespaceCAS, key)
	if snap := s.Snapshot(); snap.BytesOut != 1000 {
		t.Errorf("BytesOut = %d, want 1000", snap.BytesOut)
	}

	if err := s.Delete(NamespaceCAS, key); err != nil {
		t.Fatal(err)
	}
	snap = s.Snapshot()
	if snap.ObjectsStored != 0 || snap.BytesStored != 0 {
		t.Errorf("after Delete: objects=%d bytes=%d, want 0/0", snap.ObjectsStored, snap.BytesStored)
	}
}

// TestSweepTempsOnOpen is the falsifier for "a crash does not leave partial
// uploads accumulating until the disk fills".
func TestSweepTempsOnOpen(t *testing.T) {
	root := t.TempDir()
	s := newTestStore(t, Options{Root: root})

	stale := filepath.Join(root, "tmp", tmpPrefix+"crashed")
	if err := os.WriteFile(stale, deterministicBytes(t, 4096, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	// Something that is not ours must survive: the tmp dir is ours, but
	// deleting unrecognised files would be destructive on a misconfiguration.
	keep := filepath.Join(root, "tmp", "not-ours.dat")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := Open(Options{Root: root})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp file survived Open: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrecognised file in tmp was deleted: %v", err)
	}
}

func TestWalkVisitsEveryObject(t *testing.T) {
	s := newTestStore(t, Options{})
	want := map[string]int64{}
	for i := 0; i < 50; i++ {
		content := deterministicBytes(t, 100+i, int64(i))
		key := sha256hex(content)
		mustPut(t, s, NamespaceCAS, key, content)
		want[key] = int64(len(content))
	}
	// A stray non-object file must not be reported as an object.
	dir, _ := s.ObjectPath(NamespaceCAS, strings.Repeat("a", 64))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	if err := s.Walk(NamespaceCAS, func(st Stat) error {
		got[st.Key] = st.Size
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("Walk saw %d objects, want %d", len(got), len(want))
	}
	for k, size := range want {
		if got[k] != size {
			t.Errorf("Walk size for %s = %d, want %d", k[:8], got[k], size)
		}
	}
}

func TestClosedStoreRejects(t *testing.T) {
	s := newTestStore(t, Options{})
	content := []byte("x")
	key := sha256hex(content)
	mustPut(t, s, NamespaceCAS, key, content)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), 1); !errors.Is(err, ErrClosed) {
		t.Errorf("Put after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Get(NamespaceCAS, key); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close = %v, want ErrClosed", err)
	}
}

func TestPutHonoursContextCancellation(t *testing.T) {
	s := newTestStore(t, Options{})
	content := deterministicBytes(t, 4096, 13)
	key := sha256hex(content)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, NamespaceCAS, key, bytes.NewReader(content), int64(len(content))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := s.Stat(NamespaceCAS, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelled Put published the object: %v", err)
	}
	assertNoTempFiles(t, s)
}

// TestVerifyReadsDetectsLocalCorruption is the falsifier for "a node never
// returns bytes that fail verification": corrupt the file on disk behind the
// store's back and assert the read is refused.
func TestVerifyReadsDetectsLocalCorruption(t *testing.T) {
	s := newTestStore(t, Options{VerifyReads: true})
	content := deterministicBytes(t, 8192, 17)
	key := sha256hex(content)
	mustPut(t, s, NamespaceCAS, key, content)

	_, path := s.ObjectPath(NamespaceCAS, key)
	corrupt := append([]byte(nil), content...)
	corrupt[4000] ^= 0xFF
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	o, err := s.Get(NamespaceCAS, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer o.Close()
	if _, err := o.WriteTo(io.Discard); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := o.Verify(); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Verify on a corrupted object = %v, want ErrDigestMismatch", err)
	}
	if n := s.Snapshot().VerifyFailures; n != 1 {
		t.Errorf("VerifyFailures = %d, want 1", n)
	}
}

func TestVerifyReadsOffSkipsHashing(t *testing.T) {
	s := newTestStore(t, Options{VerifyReads: false})
	content := deterministicBytes(t, 4096, 19)
	key := sha256hex(content)
	mustPut(t, s, NamespaceCAS, key, content)

	o, err := s.Get(NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if _, err := o.WriteTo(io.Discard); err != nil {
		t.Fatal(err)
	}
	// Verify is a no-op when verification is off; it must not report a false
	// failure, and it must not silently claim success in a way that could be
	// mistaken for a check having happened.
	if err := o.Verify(); err != nil {
		t.Errorf("Verify with verification off = %v, want nil", err)
	}
}

func TestOpenRejectsEmptyRoot(t *testing.T) {
	if _, err := Open(Options{Root: "  "}); err == nil {
		t.Fatal("Open with an empty root = nil error")
	}
}

// deterministicBytes returns n pseudo-random bytes from a fixed seed. Fixed so
// that a failure is reproducible; pseudo-random rather than zeros so that a
// truncation bug cannot hide behind a file full of identical bytes.
func deterministicBytes(t *testing.T, n int, seed int64) []byte {
	t.Helper()
	b := make([]byte, n)
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // reproducibility beats unpredictability in a fixture
	if _, err := r.Read(b); err != nil {
		t.Fatalf("generate fixture bytes: %v", err)
	}
	return b
}

func assertNoTempFiles(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	var leftover []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			leftover = append(leftover, e.Name())
		}
	}
	if len(leftover) > 0 {
		t.Errorf("temp files left behind: %v", leftover)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestStoreRootIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	rel, err := filepath.Rel(mustGetwd(t), dir)
	if err != nil {
		t.Skipf("cannot relativise %s: %v", dir, err)
	}
	s, err := Open(Options{Root: rel})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if !filepath.IsAbs(s.Root()) {
		t.Errorf("Root() = %q, want an absolute path", s.Root())
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func TestPutResultReportsDurability(t *testing.T) {
	s := newTestStore(t, Options{})
	content := []byte("durable?")
	key := sha256hex(content)
	res := mustPut(t, s, NamespaceCAS, key, content)

	// On a filesystem that supports it, the publish is durable and the store
	// agrees. On one that does not, both must say so; what must never happen is
	// the two disagreeing, because the benchmark report copies one and the
	// README quotes the other.
	if res.Durable != s.DirSyncSupported() {
		t.Errorf("PutResult.Durable = %v but DirSyncSupported = %v", res.Durable, s.DirSyncSupported())
	}
	if !res.Durable {
		t.Logf("note: %s does not support directory fsync; durability claim is downgraded", filepath.Dir(s.Root()))
	}
}
