package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newQuotaStore(t *testing.T, maxBytes int64, high, low float64) (*Store, *Evictor) {
	t.Helper()
	s, err := Open(Options{
		Root:        t.TempDir(),
		VerifyReads: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	e, err := NewEvictor(EvictorOptions{
		Store: s,
		Index: s.Index(),
		Quota: Quota{MaxBytes: maxBytes, HighWater: high, LowWater: low},
	})
	if err != nil {
		t.Fatalf("NewEvictor: %v", err)
	}
	s.AttachEvictor(e)
	t.Cleanup(e.Close)
	return s, e
}

func TestQuotaValidation(t *testing.T) {
	cases := []Quota{
		{MaxBytes: 0, HighWater: 0.9, LowWater: 0.8},
		{MaxBytes: -1, HighWater: 0.9, LowWater: 0.8},
		{MaxBytes: 100, HighWater: 0, LowWater: 0.8},
		{MaxBytes: 100, HighWater: 1.5, LowWater: 0.8},
		{MaxBytes: 100, HighWater: 0.8, LowWater: 0.9},
		{MaxBytes: 100, HighWater: 0.8, LowWater: 0.8},
		{MaxBytes: 100, HighWater: 0.8, LowWater: 0},
	}
	for _, q := range cases {
		if err := q.Valid(); err == nil {
			t.Errorf("Quota%+v accepted", q)
		}
	}
	good := Quota{MaxBytes: 1000, HighWater: 0.9, LowWater: 0.8}
	if err := good.Valid(); err != nil {
		t.Errorf("valid quota rejected: %v", err)
	}
	if good.HighBytes() != 900 || good.LowBytes() != 800 {
		t.Errorf("HighBytes=%d LowBytes=%d, want 900/800", good.HighBytes(), good.LowBytes())
	}
}

// TestEvictionScorePrefersLargeAndCold pins the policy's shape: at equal age,
// the larger object goes first; at equal size, the colder one goes first.
func TestEvictionScorePrefersLargeAndCold(t *testing.T) {
	p := DefaultEvictionPolicy()
	now := time.Now()

	small := Entry{Size: 64 << 10, LastAccess: now}
	large := Entry{Size: 64 << 20, LastAccess: now}
	if p.Score(large) >= p.Score(small) {
		t.Error("at equal age, the larger object does not score lower")
	}

	old := Entry{Size: 1 << 20, LastAccess: now.Add(-24 * time.Hour)}
	recent := Entry{Size: 1 << 20, LastAccess: now}
	if p.Score(old) >= p.Score(recent) {
		t.Error("at equal size, the older object does not score lower")
	}

	// The size penalty must not overwhelm recency entirely, or large objects
	// become uncacheable. A 64 MiB object read now must outlive a 64 KiB object
	// last read a week ago.
	hugeRecent := Entry{Size: 64 << 20, LastAccess: now}
	tinyAncient := Entry{Size: 64 << 10, LastAccess: now.Add(-7 * 24 * time.Hour)}
	if p.Score(hugeRecent) <= p.Score(tinyAncient) {
		t.Error("the size penalty swamps recency: a large fresh object scores below a tiny week-old one")
	}
}

func TestEvictionScoreHandlesZeroSize(t *testing.T) {
	p := DefaultEvictionPolicy()
	now := time.Now()
	if got := p.Score(Entry{Size: 0, LastAccess: now}); got != now.UnixNano() {
		t.Errorf("a zero-byte object carries a size penalty: %d vs %d", got, now.UnixNano())
	}
	// A zero-valued policy must not divide by zero.
	var zero EvictionPolicy
	_ = zero.Score(Entry{Size: 1 << 20, LastAccess: now})
}

// TestSweepStaysWithinQuota is the Phase 3 acceptance criterion: under a tiny
// quota the disk stays within the documented tolerance.
//
// The documented tolerance is the high-water mark plus one object: eviction
// runs after a write, so usage can briefly exceed the high-water mark by the
// size of the object that crossed it. That is stated here and in
// docs/runbook.md rather than left as a surprise.
func TestSweepStaysWithinQuota(t *testing.T) {
	const maxBytes = 1 << 20 // 1 MiB
	const objSize = 16 << 10 // 16 KiB
	s, e := newQuotaStore(t, maxBytes, 0.90, 0.70)
	ctx := context.Background()

	for i := 0; i < 400; i++ {
		content := deterministicBytes(t, objSize, int64(i))
		key := sha256hex(content)
		if _, err := s.Put(ctx, NamespaceCAS, key, bytes.NewReader(content), objSize); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		if _, _, err := e.Sweep(ctx); err != nil {
			t.Fatalf("Sweep after %d: %v", i, err)
		}

		_, used, err := s.Index().Totals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tolerance := e.quota.HighBytes() + objSize
		if used > tolerance {
			t.Fatalf("after %d writes, usage %d exceeds the documented tolerance %d (high water %d + one object %d)",
				i+1, used, tolerance, e.quota.HighBytes(), objSize)
		}
	}

	// And what the index claims must match what is on disk.
	_, indexed, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := diskUsage(t, s)
	if indexed != onDisk {
		t.Errorf("index claims %d bytes, disk holds %d", indexed, onDisk)
	}
	t.Logf("400 writes of %d bytes under a %d-byte quota: final usage %d (high water %d, low water %d), %d objects evicted",
		objSize, maxBytes, indexed, e.quota.HighBytes(), e.quota.LowBytes(), e.Snapshot().Evicted)
}

// TestHotObjectsOutliveColdOnes is the other half of the acceptance criterion.
func TestHotObjectsOutliveColdOnes(t *testing.T) {
	const maxBytes = 512 << 10
	const objSize = 8 << 10
	s, e := newQuotaStore(t, maxBytes, 0.90, 0.60)
	ctx := context.Background()

	// Write a working set that fits comfortably, and keep reading it.
	hot := make([]string, 12)
	for i := range hot {
		content := deterministicBytes(t, objSize, int64(1000+i))
		hot[i] = sha256hex(content)
		if _, err := s.Put(ctx, NamespaceCAS, hot[i], bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
	}

	// Now stream cold objects through, touching the hot set between writes.
	for i := 0; i < 200; i++ {
		content := deterministicBytes(t, objSize, int64(i))
		key := sha256hex(content)
		if _, err := s.Put(ctx, NamespaceCAS, key, bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
		for _, h := range hot {
			if obj, err := s.Get(NamespaceCAS, h); err == nil {
				_ = obj.Close()
			}
		}
		if _, _, err := e.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	survived := 0
	for _, h := range hot {
		if _, err := s.Stat(NamespaceCAS, h); err == nil {
			survived++
		}
	}
	t.Logf("%d of %d continuously-read objects survived %d cold writes under a %d-byte quota",
		survived, len(hot), 200, maxBytes)
	if survived != len(hot) {
		t.Errorf("only %d of %d hot objects survived; eviction is not access-aware", survived, len(hot))
	}
}

// TestSweepDrainsToLowWater checks the two-mark behaviour: a sweep does real
// work rather than evicting exactly one object per write forever.
func TestSweepDrainsToLowWater(t *testing.T) {
	const maxBytes = 200 << 10
	const objSize = 4 << 10
	s, e := newQuotaStore(t, maxBytes, 0.90, 0.50)
	ctx := context.Background()

	for i := 0; i < 60; i++ {
		content := deterministicBytes(t, objSize, int64(i))
		if _, err := s.Put(ctx, NamespaceCAS, sha256hex(content), bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
	}
	_, before, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before <= e.quota.HighBytes() {
		t.Skipf("usage %d never reached the high-water mark %d; adjust the fixture", before, e.quota.HighBytes())
	}

	n, freed, err := e.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, after, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sweep evicted %d objects (%d bytes): %d -> %d, low water %d",
		n, freed, before, after, e.quota.LowBytes())

	if after > e.quota.LowBytes() {
		t.Errorf("after a sweep, usage %d is still above the low-water mark %d", after, e.quota.LowBytes())
	}
	if n < 2 {
		t.Errorf("sweep evicted %d objects; a two-mark policy should evict a batch, not one at a time", n)
	}
}

func TestSweepIsANoOpBelowHighWater(t *testing.T) {
	s, e := newQuotaStore(t, 1<<20, 0.90, 0.70)
	ctx := context.Background()
	content := deterministicBytes(t, 1024, 1)
	if _, err := s.Put(ctx, NamespaceCAS, sha256hex(content), bytes.NewReader(content), 1024); err != nil {
		t.Fatal(err)
	}
	n, freed, err := e.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || freed != 0 {
		t.Errorf("sweep below the high-water mark evicted %d objects / %d bytes", n, freed)
	}
	if e.Snapshot().Sweeps != 0 {
		t.Error("a no-op sweep was counted as a sweep")
	}
}

// TestEvictionRemovesFileAndIndexRowTogether is the falsifier for a silent disk
// leak: an index row removed without its file makes the file invisible to
// eviction forever while it still occupies space.
func TestEvictionRemovesFileAndIndexRowTogether(t *testing.T) {
	const maxBytes = 64 << 10
	const objSize = 4 << 10
	s, e := newQuotaStore(t, maxBytes, 0.80, 0.40)
	ctx := context.Background()

	for i := 0; i < 60; i++ {
		content := deterministicBytes(t, objSize, int64(i))
		if _, err := s.Put(ctx, NamespaceCAS, sha256hex(content), bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	_, indexed, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk := diskUsage(t, s); indexed != onDisk {
		t.Errorf("index says %d bytes, disk has %d; eviction left one of them behind", indexed, onDisk)
	}

	// Every indexed object must still be openable.
	if err := s.Index().All(ctx, func(entry Entry) error {
		if _, err := s.Stat(entry.Namespace, entry.Key); err != nil {
			return fmt.Errorf("index claims %s/%s but the file is gone: %w",
				entry.Namespace, entry.Key[:8], err)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
}

func TestEvictorRunRespondsToWake(t *testing.T) {
	const maxBytes = 64 << 10
	const objSize = 8 << 10
	s, e := newQuotaStore(t, maxBytes, 0.80, 0.40)

	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx, time.Hour) // long ticker: only Wake should drive this

	for i := 0; i < 40; i++ {
		content := deterministicBytes(t, objSize, int64(i))
		if _, err := s.Put(context.Background(), NamespaceCAS, sha256hex(content), bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(20 * time.Second)
	var used int64
	for time.Now().Before(deadline) {
		_, u, err := s.Index().Totals(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		used = u
		if used <= e.quota.HighBytes() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	e.Wait()

	if used > e.quota.HighBytes() {
		t.Errorf("the background evictor did not drain: usage %d, high water %d", used, e.quota.HighBytes())
	}
	if e.Snapshot().Sweeps == 0 {
		t.Error("no sweep ran despite writes waking the evictor")
	}
}

func TestEvictorRunStopsOnContextCancel(t *testing.T) {
	_, e := newQuotaStore(t, 1<<20, 0.9, 0.7)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestNewEvictorRejectsBadOptions(t *testing.T) {
	s, err := Open(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := NewEvictor(EvictorOptions{Index: s.Index(), Quota: Quota{MaxBytes: 1, HighWater: 0.9, LowWater: 0.8}}); err == nil {
		t.Error("NewEvictor without a store = nil error")
	}
	if _, err := NewEvictor(EvictorOptions{Store: s, Quota: Quota{MaxBytes: 1, HighWater: 0.9, LowWater: 0.8}}); err == nil {
		t.Error("NewEvictor without an index = nil error")
	}
	if _, err := NewEvictor(EvictorOptions{Store: s, Index: s.Index(), Quota: Quota{}}); err == nil {
		t.Error("NewEvictor with an invalid quota = nil error")
	}
}

func TestSweepAfterCloseIsRejected(t *testing.T) {
	_, e := newQuotaStore(t, 1<<20, 0.9, 0.7)
	e.Close()
	if _, _, err := e.Sweep(context.Background()); err == nil {
		t.Error("Sweep after Close = nil error")
	}
	// Close is idempotent; shutdown paths overlap.
	e.Close()
}

// diskUsage sums the sizes of every object file under the store root.
func diskUsage(t *testing.T, s *Store) int64 {
	t.Helper()
	var total int64
	for _, ns := range []Namespace{NamespaceCAS, NamespaceAC} {
		if err := s.Walk(ns, func(st Stat) error {
			total += st.Size
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", ns, err)
		}
	}
	return total
}

// TestWriteQuotaReport emits the measured quota behaviour as a raw result file.
func TestWriteQuotaReport(t *testing.T) {
	dir := os.Getenv("KILNCACHE_RESULTS_DIR")
	if dir == "" {
		t.Skip("set KILNCACHE_RESULTS_DIR to write the quota report (make quota-report)")
	}
	const maxBytes = 4 << 20
	const objSize = 32 << 10
	const writes = 600

	s, e := newQuotaStore(t, maxBytes, 0.90, 0.70)
	ctx := context.Background()

	hot := make([]string, 10)
	for i := range hot {
		content := deterministicBytes(t, objSize, int64(9000+i))
		hot[i] = sha256hex(content)
		if _, err := s.Put(ctx, NamespaceCAS, hot[i], bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
	}

	var peak int64
	for i := 0; i < writes; i++ {
		content := deterministicBytes(t, objSize, int64(i))
		if _, err := s.Put(ctx, NamespaceCAS, sha256hex(content), bytes.NewReader(content), objSize); err != nil {
			t.Fatal(err)
		}
		for _, h := range hot {
			if obj, err := s.Get(NamespaceCAS, h); err == nil {
				_ = obj.Close()
			}
		}
		if _, _, err := e.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if _, used, err := s.Index().Totals(ctx); err == nil && used > peak {
			peak = used
		}
	}

	survived := 0
	for _, h := range hot {
		if _, err := s.Stat(NamespaceCAS, h); err == nil {
			survived++
		}
	}
	_, finalUsed, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap := e.Snapshot()

	report := map[string]any{
		"kind":                       "quota-eviction",
		"generated_at":               time.Now().UTC().Format(time.RFC3339),
		"max_bytes":                  maxBytes,
		"high_water_bytes":           e.quota.HighBytes(),
		"low_water_bytes":            e.quota.LowBytes(),
		"object_size_bytes":          objSize,
		"cold_writes":                writes,
		"hot_working_set":            len(hot),
		"hot_objects_surviving":      survived,
		"peak_bytes_used":            peak,
		"peak_over_high_water_bytes": peak - e.quota.HighBytes(),
		"documented_tolerance_bytes": e.quota.HighBytes() + objSize,
		"final_bytes_used":           finalUsed,
		"index_bytes_equals_disk":    finalUsed == diskUsage(t, s),
		"objects_evicted":            snap.Evicted,
		"bytes_reclaimed":            snap.BytesReclaimed,
		"sweeps":                     snap.Sweeps,
		"eviction_failures":          snap.Failures,
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "quota-eviction.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
	t.Logf("  quota %d, high %d, low %d; peak usage %d (%d over high water, tolerance %d)",
		maxBytes, e.quota.HighBytes(), e.quota.LowBytes(), peak,
		peak-e.quota.HighBytes(), e.quota.HighBytes()+objSize)
	t.Logf("  %d of %d continuously-read objects survived %d cold writes; %d objects evicted",
		survived, len(hot), writes, snap.Evicted)
}
