package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := OpenIndex(IndexOptions{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestIndexUpsertAndGet(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	key := sha256hex([]byte("a"))

	if err := idx.Upsert(ctx, NamespaceCAS, key, 1234); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	e, err := idx.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Size != 1234 || e.Key != key || e.Namespace != NamespaceCAS {
		t.Errorf("entry = %+v", e)
	}
	if e.AccessCount != 1 {
		t.Errorf("AccessCount = %d, want 1", e.AccessCount)
	}
}

func TestIndexGetMissing(t *testing.T) {
	idx := newTestIndex(t)
	if _, err := idx.Get(context.Background(), NamespaceCAS, sha256hex([]byte("nope"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
}

// TestIndexUpsertPreservesAccessHistory: a duplicate PUT must not reset an
// object's access history, or a hot object that several builds keep
// re-uploading would look brand new and cold to eviction's ranking.
func TestIndexUpsertPreservesAccessHistory(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	key := sha256hex([]byte("hot"))

	if err := idx.Upsert(ctx, NamespaceCAS, key, 100); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		idx.Touch(NamespaceCAS, key)
	}
	if err := idx.FlushTouches(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := idx.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.Upsert(ctx, NamespaceCAS, key, 100); err != nil {
		t.Fatal(err)
	}
	after, err := idx.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccessCount < before.AccessCount {
		t.Errorf("AccessCount dropped from %d to %d across a duplicate PUT",
			before.AccessCount, after.AccessCount)
	}
	if after.CreatedAt != before.CreatedAt {
		t.Errorf("CreatedAt changed across a duplicate PUT")
	}
}

func TestIndexTotals(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := idx.Upsert(ctx, NamespaceCAS, sha256hex([]byte{byte(i)}), int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	n, bytes, err := idx.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("objects = %d, want 10", n)
	}
	want := int64(0)
	for i := 0; i < 10; i++ {
		want += int64(100 + i)
	}
	if bytes != want {
		t.Errorf("bytes = %d, want %d", bytes, want)
	}
}

// TestIndexRemoveCancelsPendingTouch is the falsifier for a phantom row: if a
// queued touch were flushed after the row was deleted, the UPDATE would match
// nothing -- but if the order were reversed, or if Remove did not clear the
// queue, an evicted object could reappear in the index claiming disk that no
// file occupies.
func TestIndexRemoveCancelsPendingTouch(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	key := sha256hex([]byte("doomed"))

	if err := idx.Upsert(ctx, NamespaceCAS, key, 500); err != nil {
		t.Fatal(err)
	}
	idx.Touch(NamespaceCAS, key)
	if err := idx.Remove(ctx, NamespaceCAS, key); err != nil {
		t.Fatal(err)
	}
	if err := idx.FlushTouches(ctx); err != nil {
		t.Fatalf("FlushTouches after Remove: %v", err)
	}
	if _, err := idx.Get(ctx, NamespaceCAS, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed object is back in the index: %v", err)
	}
	n, bytes, err := idx.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || bytes != 0 {
		t.Errorf("totals after remove = %d objects / %d bytes, want 0/0", n, bytes)
	}
}

func TestIndexColdestNIsOrdered(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()

	base := time.Now()
	for i := 0; i < 20; i++ {
		key := sha256hex([]byte{byte(i), 0xAA})
		if err := idx.Upsert(ctx, NamespaceCAS, key, 1000); err != nil {
			t.Fatal(err)
		}
		// Touch in ascending order so later keys are hotter.
		idx.Touch(NamespaceCAS, key)
		if err := idx.FlushTouches(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	_ = base

	coldest, err := idx.ColdestN(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(coldest) != 5 {
		t.Fatalf("ColdestN(5) returned %d", len(coldest))
	}
	for i := 1; i < len(coldest); i++ {
		if coldest[i].LastAccess.Before(coldest[i-1].LastAccess) {
			t.Errorf("ColdestN is not ordered by last access: %v then %v",
				coldest[i-1].LastAccess, coldest[i].LastAccess)
		}
	}
	if n, err := idx.ColdestN(ctx, 0); err != nil || n != nil {
		t.Errorf("ColdestN(0) = %v, %v; want nil, nil", n, err)
	}
}

func TestIndexTouchesAreBatched(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	key := sha256hex([]byte("batched"))
	if err := idx.Upsert(ctx, NamespaceCAS, key, 10); err != nil {
		t.Fatal(err)
	}

	before, err := idx.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	for i := 0; i < 100; i++ {
		idx.Touch(NamespaceCAS, key)
	}
	// Nothing written yet: a cache hit must not cost a database write.
	mid, err := idx.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	if !mid.LastAccess.Equal(before.LastAccess) {
		t.Error("a Touch wrote to the database before a flush")
	}
	if got := idx.Snapshot().TouchesWritten; got != 0 {
		t.Errorf("TouchesWritten = %d before flush, want 0", got)
	}

	if err := idx.FlushTouches(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := idx.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastAccess.After(before.LastAccess) {
		t.Errorf("last access did not advance after a flush: %v then %v",
			before.LastAccess, after.LastAccess)
	}
	// 100 touches of one key collapse to one write. That collapsing is the
	// point of batching, not an accident.
	if got := idx.Snapshot().TouchesWritten; got != 1 {
		t.Errorf("TouchesWritten = %d, want 1 (100 touches of one key)", got)
	}
}

func TestIndexTouchQueueIsBounded(t *testing.T) {
	idx, err := OpenIndex(IndexOptions{
		Path:       filepath.Join(t.TempDir(), "index.db"),
		TouchLimit: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	for i := 0; i < 100; i++ {
		idx.Touch(NamespaceCAS, sha256hex([]byte{byte(i), byte(i >> 8), 7}))
	}
	snap := idx.Snapshot()
	if snap.TouchesDropped == 0 {
		t.Error("the touch queue accepted 100 distinct keys with a limit of 8")
	}
	if snap.TouchesQueued > 8 {
		t.Errorf("TouchesQueued = %d, want at most the limit of 8", snap.TouchesQueued)
	}
}

func TestIndexConcurrentTouches(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	keys := make([]string, 50)
	for i := range keys {
		keys[i] = sha256hex([]byte{byte(i), 0x11})
		if err := idx.Upsert(ctx, NamespaceCAS, keys[i], 100); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				idx.Touch(NamespaceCAS, keys[(w+i)%len(keys)])
			}
		}(w)
	}
	// Flush concurrently with the touches, which is what the background flusher
	// really does.
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		for i := 0; i < 20; i++ {
			_ = idx.FlushTouches(ctx)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	<-flushDone

	if err := idx.FlushTouches(ctx); err != nil {
		t.Fatalf("final flush: %v", err)
	}
}

func TestIndexReplaceAll(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()

	if err := idx.Upsert(ctx, NamespaceCAS, sha256hex([]byte("old")), 999); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	entries := []Entry{
		{Namespace: NamespaceCAS, Key: sha256hex([]byte("a")), Size: 10, CreatedAt: now, LastAccess: now, AccessCount: 3},
		{Namespace: NamespaceAC, Key: sha256hex([]byte("b")), Size: 20, CreatedAt: now, LastAccess: now},
	}
	if err := idx.ReplaceAll(ctx, entries); err != nil {
		t.Fatal(err)
	}

	n, bytes, err := idx.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || bytes != 30 {
		t.Errorf("totals = %d/%d, want 2/30", n, bytes)
	}
	if _, err := idx.Get(ctx, NamespaceCAS, sha256hex([]byte("old"))); !errors.Is(err, ErrNotFound) {
		t.Error("ReplaceAll left a row that was not in the new set")
	}
	// A zero access count would sort as if the object had never been read;
	// ReplaceAll normalises it to 1.
	e, err := idx.Get(ctx, NamespaceAC, sha256hex([]byte("b")))
	if err != nil {
		t.Fatal(err)
	}
	if e.AccessCount != 1 {
		t.Errorf("AccessCount = %d, want 1", e.AccessCount)
	}
}

func TestIndexMeta(t *testing.T) {
	idx := newTestIndex(t)
	ctx := context.Background()
	if _, err := idx.GetMeta(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetMeta absent = %v, want ErrNotFound", err)
	}
	if err := idx.SetMeta(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetMeta(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	v, err := idx.GetMeta(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v2" {
		t.Errorf("GetMeta = %q, want v2", v)
	}
}

func TestIndexSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	ctx := context.Background()

	idx, err := OpenIndex(IndexOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	key := sha256hex([]byte("persisted"))
	if err := idx.Upsert(ctx, NamespaceCAS, key, 4242); err != nil {
		t.Fatal(err)
	}
	// Queue a touch and close: Close must flush it, which is what makes
	// batching acceptable.
	idx.Touch(NamespaceCAS, key)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	idx2, err := OpenIndex(IndexOptions{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer idx2.Close()

	e, err := idx2.Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if e.Size != 4242 {
		t.Errorf("size = %d, want 4242", e.Size)
	}
	if e.AccessCount != 2 {
		t.Errorf("AccessCount = %d, want 2 (the queued touch was flushed on Close)", e.AccessCount)
	}
}

func TestOpenIndexRejectsEmptyPath(t *testing.T) {
	if _, err := OpenIndex(IndexOptions{}); err == nil {
		t.Error("OpenIndex with no path = nil error")
	}
}

func TestIndexClosedRejectsWrites(t *testing.T) {
	idx, err := OpenIndex(IndexOptions{Path: filepath.Join(t.TempDir(), "i.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(context.Background(), NamespaceCAS, sha256hex(nil), 1); !errors.Is(err, ErrClosed) {
		t.Errorf("Upsert after Close = %v, want ErrClosed", err)
	}
	// Double close is not an error: shutdown paths overlap.
	if err := idx.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestIndexUsesWAL pins the journal mode. Without WAL, the eviction sweep's
// read transaction blocks every concurrent write, which is exactly the
// behaviour the mode was chosen to avoid.
func TestIndexUsesWAL(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(IndexOptions{Path: filepath.Join(dir, "index.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	var mode string
	if err := idx.w.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	if err := idx.Upsert(context.Background(), NamespaceCAS, sha256hex(nil), 1); err != nil {
		t.Fatal(err)
	}
	// A -wal sidecar file is the observable consequence.
	if _, err := os.Stat(filepath.Join(dir, "index.db-wal")); err != nil {
		t.Errorf("no WAL sidecar file: %v", err)
	}
}

// TestIndexScanIsNotBlockedByWrites is the falsifier for the WAL choice.
//
// The repair auditor scans the whole index while requests are writing to it. In
// rollback-journal mode a reader and a writer exclude each other, so the scan
// would stall behind every write and repair convergence would become a function
// of write load rather than of how much is broken. This measures the scan both
// idle and under a continuous writer and fails if the difference is large.
//
// It also records the absolute cost, which is the answer to "is the index the
// bottleneck in repair convergence?" -- it is not, and the number here is why.
func TestIndexScanIsNotBlockedByWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping index scan timing in short mode")
	}
	idx := newTestIndex(t)
	ctx := context.Background()

	const n = 20000
	for i := 0; i < n; i++ {
		if err := idx.Upsert(ctx, NamespaceCAS, fmt.Sprintf("%064x", i), 1024); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	idle := 0
	if err := idx.All(ctx, func(Entry) error { idle++; return nil }); err != nil {
		t.Fatal(err)
	}
	idleDur := time.Since(start)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := idx.Upsert(ctx, NamespaceCAS, fmt.Sprintf("%064x", n+i), 1024); err != nil {
				return
			}
			writes.Add(1)
		}
	}()
	time.Sleep(50 * time.Millisecond)

	start = time.Now()
	loaded := 0
	if err := idx.All(ctx, func(Entry) error { loaded++; return nil }); err != nil {
		t.Fatal(err)
	}
	loadedDur := time.Since(start)
	close(stop)
	wg.Wait()

	ratio := float64(loadedDur) / float64(idleDur)
	t.Logf("index scan: %d rows in %v idle, %d rows in %v under %d concurrent writes (%.2fx)",
		idle, idleDur.Round(time.Millisecond), loaded, loadedDur.Round(time.Millisecond),
		writes.Load(), ratio)

	if writes.Load() == 0 {
		t.Fatal("no concurrent writes happened; the test proves nothing")
	}
	// Generous: this is a smoke test against a reader/writer exclusion
	// regression, not a latency budget. Rollback-journal mode would show up as
	// a multiple, not as a few percent.
	if ratio > 5 {
		t.Errorf("a scan under write load is %.1fx slower than idle; readers and writers are excluding each other", ratio)
	}

	writeJSONResult(t, "index-scan.json", map[string]any{
		"kind":                  "index-scan",
		"generated_at":          time.Now().UTC().Format(time.RFC3339),
		"journal_mode":          "wal",
		"rows_idle":             idle,
		"scan_idle_ms":          float64(idleDur.Microseconds()) / 1000,
		"rows_under_write_load": loaded,
		"scan_under_write_ms":   float64(loadedDur.Microseconds()) / 1000,
		"concurrent_writes":     writes.Load(),
		"slowdown_ratio":        ratio,
		"note": "The repair auditor scans the whole index while requests write to it. " +
			"In rollback-journal mode a reader and a writer exclude each other, so this " +
			"ratio would be a multiple rather than a few percent.",
	})
}

// writeJSONResult saves a raw result file when KILNCACHE_RESULTS_DIR is set, so
// that a number quoted in a document has a committed file behind it
// (CONTRIBUTING rule 1). It is a no-op otherwise, keeping the normal test run
// free of side effects.
func writeJSONResult(t *testing.T, name string, v any) {
	t.Helper()
	dir := os.Getenv("KILNCACHE_RESULTS_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("create results dir: %v", err)
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Errorf("marshal result: %v", err)
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Errorf("write %s: %v", path, err)
		return
	}
	t.Logf("wrote %s", path)
}
