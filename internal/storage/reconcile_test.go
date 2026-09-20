package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestReconcileNeverMarksAMissingFileValid is the headline Phase 3 acceptance
// criterion. It deletes an object file behind the store's back -- which is what
// an unclean shutdown, a filesystem repair, or a well-meaning operator with rm
// all look like -- and asserts that after a restart the index does not claim
// the object exists.
//
// Answering HEAD with 200 for an object the node lost is the one answer a cache
// must never give: Bazel would skip the action and then fail to download the
// output.
func TestReconcileNeverMarksAMissingFileValid(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	s, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 20)
	for i := range keys {
		content := deterministicBytes(t, 1024+i, int64(i))
		keys[i] = sha256hex(content)
		if _, err := s.Put(ctx, NamespaceCAS, keys[i], bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Delete half the files behind the store's back.
	deleted := map[string]bool{}
	for i := 0; i < len(keys); i += 2 {
		s2, _ := Open(Options{Root: root, DisableIndex: true})
		_, path := s2.ObjectPath(NamespaceCAS, keys[i])
		_ = s2.Close()
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
		deleted[keys[i]] = true
	}

	s3, err := Open(Options{Root: root})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s3.Close()

	for _, k := range keys {
		_, indexErr := s3.Index().Get(ctx, NamespaceCAS, k)
		_, statErr := s3.Stat(NamespaceCAS, k)

		if deleted[k] {
			if indexErr == nil {
				t.Errorf("object %s was deleted from disk but the index still claims it", k[:8])
			}
			if !errors.Is(statErr, ErrNotFound) {
				t.Errorf("Stat of a deleted object %s = %v, want ErrNotFound", k[:8], statErr)
			}
			continue
		}
		if indexErr != nil {
			t.Errorf("object %s survived on disk but is missing from the index: %v", k[:8], indexErr)
		}
		if statErr != nil {
			t.Errorf("object %s: Stat = %v", k[:8], statErr)
		}
	}

	n, bytesIndexed, err := s3.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(keys)-len(deleted)) {
		t.Errorf("index holds %d objects, want %d", n, len(keys)-len(deleted))
	}
	if onDisk := diskUsage(t, s3); bytesIndexed != onDisk {
		t.Errorf("index claims %d bytes, disk holds %d", bytesIndexed, onDisk)
	}
}

// TestReconcileAdoptsUnknownFiles covers the other direction: files that exist
// on disk but are absent from the index (a repair worker's copy, a restored
// backup, an index lost entirely) must be adopted, not ignored. An unadopted
// file occupies disk that quota accounting does not know about.
func TestReconcileAdoptsUnknownFiles(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	s, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	content := deterministicBytes(t, 4096, 1)
	key := sha256hex(content)
	if _, err := s.Put(ctx, NamespaceCAS, key, bytes.NewReader(content), 4096); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Blow the index away entirely, as a corrupted database would have to be.
	if err := os.Remove(indexPath(root)); err != nil {
		t.Fatalf("remove index: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(indexPath(root) + suffix)
	}

	s2, err := Open(Options{Root: root})
	if err != nil {
		t.Fatalf("reopen with no index: %v", err)
	}
	defer s2.Close()

	e, err := s2.Index().Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatalf("object was not adopted after the index was destroyed: %v", err)
	}
	if e.Size != 4096 {
		t.Errorf("adopted size = %d, want 4096", e.Size)
	}
	// And the bytes must still be readable and correct.
	obj, err := s2.Get(NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	var buf bytes.Buffer
	if _, err := obj.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Error("adopted object has the wrong bytes")
	}
}

// TestReconcilePreservesAccessHistory: a restart must not make every object
// look equally hot, or the first eviction after a restart is effectively random.
func TestReconcilePreservesAccessHistory(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	s, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	cold := deterministicBytes(t, 1024, 1)
	hot := deterministicBytes(t, 1024, 2)
	coldKey, hotKey := sha256hex(cold), sha256hex(hot)
	for _, pair := range []struct {
		k string
		b []byte
	}{{coldKey, cold}, {hotKey, hot}} {
		if _, err := s.Put(ctx, NamespaceCAS, pair.k, bytes.NewReader(pair.b), 1024); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 10; i++ {
		obj, err := s.Get(NamespaceCAS, hotKey)
		if err != nil {
			t.Fatal(err)
		}
		_ = obj.Close()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	hotEntry, err := s2.Index().Get(ctx, NamespaceCAS, hotKey)
	if err != nil {
		t.Fatal(err)
	}
	coldEntry, err := s2.Index().Get(ctx, NamespaceCAS, coldKey)
	if err != nil {
		t.Fatal(err)
	}
	if !hotEntry.LastAccess.After(coldEntry.LastAccess) {
		t.Errorf("after a restart the read object is not hotter: hot %v, cold %v",
			hotEntry.LastAccess, coldEntry.LastAccess)
	}
	if hotEntry.AccessCount <= coldEntry.AccessCount {
		t.Errorf("access counts did not survive the restart: hot %d, cold %d",
			hotEntry.AccessCount, coldEntry.AccessCount)
	}
}

// TestReconcileCorrectsSize: a file whose size does not match the index means
// the index is describing something that is no longer there. The disk wins.
func TestReconcileCorrectsSize(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	s, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	content := deterministicBytes(t, 8192, 3)
	key := sha256hex(content)
	if _, err := s.Put(ctx, NamespaceCAS, key, bytes.NewReader(content), 8192); err != nil {
		t.Fatal(err)
	}
	_, path := s.ObjectPath(NamespaceCAS, key)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Truncate the file, as an interrupted filesystem operation would.
	if err := os.Truncate(path, 100); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	e, err := s2.Index().Get(ctx, NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != 100 {
		t.Errorf("index size = %d after truncation, want 100 (the disk is authoritative)", e.Size)
	}

	// The truncated object is still *indexed* -- it exists -- but reading it
	// must fail verification, because its bytes no longer match its key. That
	// is what makes this recoverable: repair can replace it.
	s3, err := Open(Options{Root: root, VerifyReads: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	obj, err := s3.Get(NamespaceCAS, key)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	var sink bytes.Buffer
	if _, err := obj.WriteTo(&sink); err != nil {
		t.Fatal(err)
	}
	if err := obj.Verify(); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("a truncated object verified successfully: %v", err)
	}
}

func TestReconcileReportsWhatItDid(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	s, err := Open(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 5; i++ {
		content := deterministicBytes(t, 512, int64(i))
		if _, err := s.Put(ctx, NamespaceCAS, sha256hex(content), bytes.NewReader(content), 512); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesOnDisk != 5 {
		t.Errorf("FilesOnDisk = %d, want 5", res.FilesOnDisk)
	}
	if res.BytesOnDisk != 5*512 {
		t.Errorf("BytesOnDisk = %d, want %d", res.BytesOnDisk, 5*512)
	}
	if res.RemovedStale != 0 || res.AddedToIndex != 0 {
		t.Errorf("a no-op reconcile reported added=%d removed=%d", res.AddedToIndex, res.RemovedStale)
	}
	if res.DurationMillis < 0 {
		t.Errorf("DurationMillis = %d", res.DurationMillis)
	}

	when, err := s.Index().GetMeta(ctx, "reconciled_at")
	if err != nil {
		t.Fatalf("reconciliation time was not recorded: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, when); err != nil {
		t.Errorf("reconciled_at = %q, not a timestamp: %v", when, err)
	}
}

func TestStoreWithIndexDisabled(t *testing.T) {
	s, err := Open(Options{Root: t.TempDir(), DisableIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.Index() != nil {
		t.Fatal("DisableIndex left an index open")
	}
	content := deterministicBytes(t, 256, 1)
	key := sha256hex(content)
	if _, err := s.Put(context.Background(), NamespaceCAS, key, bytes.NewReader(content), 256); err != nil {
		t.Fatalf("Put without an index: %v", err)
	}
	if _, err := s.Stat(NamespaceCAS, key); err != nil {
		t.Errorf("Stat without an index: %v", err)
	}
	if err := s.Delete(NamespaceCAS, key); err != nil {
		t.Errorf("Delete without an index: %v", err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Errorf("Reconcile without an index: %v", err)
	}
}
