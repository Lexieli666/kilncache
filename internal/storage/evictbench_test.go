package storage

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestEvictionThroughput measures how fast a sweep can reclaim space.
//
// This is the number that decides whether a quota is enforced at all. Eviction
// has to keep up with the write rate; when it cannot, usage climbs past the
// high-water mark and stays there, which is precisely what a 10-minute chaos
// run found (docs/bugs.md, entry 8).
//
// It runs on a real filesystem with real fsyncs, so the number is a property of
// the machine as well as of the code. The host line is in hostinfo.json beside
// the result.
func TestEvictionThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping eviction throughput measurement in short mode")
	}
	const (
		objects  = 6000
		objSize  = 16 << 10
		maxBytes = int64(objects) * objSize
	)

	s, err := Open(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Quota set so that a single sweep must evict roughly 80% of the corpus.
	e, err := NewEvictor(EvictorOptions{
		Store: s, Index: s.Index(),
		Quota: Quota{MaxBytes: maxBytes, HighWater: 0.30, LowWater: 0.20},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.AttachEvictor(e)
	defer e.Close()

	ctx := context.Background()
	content := deterministicBytes(t, objSize, 1)
	fillStart := time.Now()
	for i := 0; i < objects; i++ {
		// Distinct keys without hashing a distinct payload each time: the
		// measurement is about deletion, not about ingest.
		key := fakeKey(i)
		if _, err := s.Put(ctx, NamespaceAC, key, bytes.NewReader(content), objSize); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	fillDur := time.Since(fillStart)

	_, before, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	n, freed, err := e.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	dur := time.Since(start)

	_, after, err := s.Index().Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("the sweep evicted nothing; the fixture is wrong")
	}

	evictPerSec := float64(n) / dur.Seconds()
	mibPerSec := float64(freed) / dur.Seconds() / (1 << 20)
	writePerSec := float64(objects) / fillDur.Seconds()

	t.Logf("wrote   %d objects of %d bytes in %v (%.0f objects/s)",
		objects, objSize, fillDur.Round(time.Millisecond), writePerSec)
	t.Logf("evicted %d objects, %d bytes in %v (%.0f objects/s, %.1f MiB/s)",
		n, freed, dur.Round(time.Millisecond), evictPerSec, mibPerSec)
	t.Logf("usage   %d -> %d (low water %d)", before, after, e.quota.LowBytes())

	if after > e.quota.LowBytes() {
		t.Errorf("sweep left usage at %d, above the low-water mark %d", after, e.quota.LowBytes())
	}

	// Eviction must be able to outpace ingest, or the quota is decorative. The
	// bound is deliberately loose -- this is a regression guard, not a budget.
	if evictPerSec < writePerSec/2 {
		t.Errorf("eviction manages %.0f objects/s against an ingest rate of %.0f objects/s; "+
			"a quota cannot be enforced at that ratio", evictPerSec, writePerSec)
	}

	writeJSONResult(t, "eviction-throughput.json", map[string]any{
		"kind":                  "eviction-throughput",
		"generated_at":          time.Now().UTC().Format(time.RFC3339),
		"objects":               objects,
		"object_size_bytes":     objSize,
		"write_objects_per_sec": writePerSec,
		"evicted_objects":       n,
		"evicted_bytes":         freed,
		"sweep_seconds":         dur.Seconds(),
		"evict_objects_per_sec": evictPerSec,
		"evict_mib_per_sec":     mibPerSec,
		"usage_before":          before,
		"usage_after":           after,
		"low_water_bytes":       e.quota.LowBytes(),
	})
}

// fakeKey returns a distinct, valid-looking key for index and AC tests. AC keys
// are not content-addressed, so nothing has to hash to them.
func fakeKey(i int) string {
	const hexDigits = "0123456789abcdef"
	var b [HashLen]byte
	for j := 0; j < HashLen; j++ {
		b[j] = '0'
	}
	for j := HashLen - 1; j >= 0 && i > 0; j-- {
		b[j] = hexDigits[i&0xf]
		i >>= 4
	}
	return string(b[:])
}
