package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This package computes the numbers that end up in BENCHMARKS.md. A bug here
// does not make a benchmark report a wrong number about the system; it makes
// the benchmark a wrong number.

func TestCorpusIsDeterministic(t *testing.T) {
	a := NewCorpus("x", 10, 1024, 42)
	b := NewCorpus("x", 10, 1024, 42)
	for i := range a.Objects {
		if a.Objects[i].Key != b.Objects[i].Key {
			t.Fatalf("object %d differs between two corpora built from the same seed", i)
		}
	}
	// A different seed must produce different bytes, or "deterministic" would
	// mean "constant" and every run would measure the same single object.
	c := NewCorpus("x", 10, 1024, 43)
	if c.Objects[0].Key == a.Objects[0].Key {
		t.Error("a different seed produced the same corpus")
	}
}

func TestCorpusKeysAreRealDigests(t *testing.T) {
	c := NewCorpus("x", 5, 4096, 7)
	for i, o := range c.Objects {
		sum := sha256.Sum256(o.Content)
		if hex.EncodeToString(sum[:]) != o.Key {
			t.Errorf("object %d: key is not the SHA-256 of its content", i)
		}
		if len(o.Content) != 4096 {
			t.Errorf("object %d: size %d, want 4096", i, len(o.Content))
		}
	}
	if c.Bytes() != 5*4096 {
		t.Errorf("Bytes() = %d, want %d", c.Bytes(), 5*4096)
	}
}

// TestCorpusContentIsNotConstant guards the benchmark against flattering
// itself: a store that deduplicated or compressed would look excellent on a
// corpus of zeros.
func TestCorpusContentIsNotConstant(t *testing.T) {
	c := NewCorpus("x", 1, 4096, 1)
	content := c.Objects[0].Content
	allSame := true
	for _, b := range content {
		if b != content[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("corpus content is a constant byte; compression or dedup would flatter the result")
	}
}

func TestMissKeysAreValidAndAbsent(t *testing.T) {
	c := NewCorpus("x", 20, 512, 3)
	present := map[string]bool{}
	for _, o := range c.Objects {
		present[o.Key] = true
	}
	misses := c.MissKeys(50, 3)
	if len(misses) != 50 {
		t.Fatalf("MissKeys returned %d", len(misses))
	}
	seen := map[string]bool{}
	for _, k := range misses {
		if len(k) != 64 {
			t.Errorf("miss key %q is not a 64-character digest", k)
		}
		if present[k] {
			t.Errorf("miss key %s collides with a corpus object", k[:8])
		}
		if seen[k] {
			t.Errorf("duplicate miss key %s", k[:8])
		}
		seen[k] = true
	}
}

func TestUniqueGenNeverRepeats(t *testing.T) {
	c := NewCorpus("x", 4, 1024, 5)
	seen := map[string]bool{}
	for w := 0; w < 4; w++ {
		g := newUniqueGen(w, c)
		for i := 0; i < 500; i++ {
			o := g.next()
			if seen[o.Key] {
				t.Fatalf("worker %d iteration %d produced a repeated key", w, i)
			}
			seen[o.Key] = true
			sum := sha256.Sum256(o.Content)
			if hex.EncodeToString(sum[:]) != o.Key {
				t.Fatalf("generated object's key is not its digest")
			}
		}
	}
	if len(seen) != 2000 {
		t.Errorf("generated %d distinct objects, want 2000", len(seen))
	}
}

func TestLatencyPercentiles(t *testing.T) {
	l := NewLatencies(0)
	// 1..100 milliseconds.
	for i := 1; i <= 100; i++ {
		l.Add(time.Duration(i) * time.Millisecond)
	}
	s := l.Summarize()

	if s.Count != 100 {
		t.Errorf("Count = %d", s.Count)
	}
	if s.MinMs != 1 || s.MaxMs != 100 {
		t.Errorf("Min/Max = %v/%v, want 1/100", s.MinMs, s.MaxMs)
	}
	if math.Abs(s.MeanMs-50.5) > 0.01 {
		t.Errorf("Mean = %v, want 50.5", s.MeanMs)
	}
	// Nearest-rank: p50 of 1..100 is the 50th value.
	if s.P50Ms != 50 {
		t.Errorf("P50 = %v, want 50", s.P50Ms)
	}
	if s.P99Ms != 99 {
		t.Errorf("P99 = %v, want 99", s.P99Ms)
	}
	if s.P95Ms != 95 {
		t.Errorf("P95 = %v, want 95", s.P95Ms)
	}
}

// TestPercentilesAreObservedValues: an interpolated p99 is a number that never
// happened, which for a latency tail is the wrong kind of answer.
func TestPercentilesAreObservedValues(t *testing.T) {
	l := NewLatencies(0)
	for _, ms := range []int{1, 1, 1, 1, 1000} {
		l.Add(time.Duration(ms) * time.Millisecond)
	}
	s := l.Summarize()
	for _, got := range []float64{s.P50Ms, s.P95Ms, s.P99Ms, s.MaxMs} {
		if got != 1 && got != 1000 {
			t.Errorf("percentile %v was never observed; only 1 ms and 1000 ms were", got)
		}
	}
	if s.P99Ms != 1000 {
		t.Errorf("P99 = %v; the one slow observation must reach the tail", s.P99Ms)
	}
}

func TestLatencyEmpty(t *testing.T) {
	s := NewLatencies(0).Summarize()
	if s.Count != 0 || s.P99Ms != 0 {
		t.Errorf("empty summary = %+v", s)
	}
}

func TestLatencyMerge(t *testing.T) {
	a, b := NewLatencies(0), NewLatencies(0)
	a.Add(time.Millisecond)
	b.Add(2 * time.Millisecond)
	a.Merge(b)
	if a.Len() != 2 {
		t.Fatalf("Len = %d, want 2", a.Len())
	}
	if s := a.Summarize(); s.MinMs != 1 || s.MaxMs != 2 {
		t.Errorf("merged summary = %+v", s)
	}
}

// TestSummariseReportsMedianNotBest is the falsifier for "this is not an
// advertisement".
func TestSummariseReportsMedianNotBest(t *testing.T) {
	runs := []RunResult{
		{OpsPerSec: 100, MiBPerSec: 1, Latency: Summary{P99Ms: 10}},
		{OpsPerSec: 200, MiBPerSec: 2, Latency: Summary{P99Ms: 5}},
		{OpsPerSec: 900, MiBPerSec: 9, Latency: Summary{P99Ms: 1}},
	}
	median, spread := summarise(runs)
	if median.OpsPerSec != 200 {
		t.Errorf("median = %v, want 200 (not the best, 900)", median.OpsPerSec)
	}
	if spread.OpsPerSecMin != 100 || spread.OpsPerSecMax != 900 {
		t.Errorf("spread = %v..%v, want 100..900", spread.OpsPerSecMin, spread.OpsPerSecMax)
	}
	if spread.P99MsMin != 1 || spread.P99MsMax != 10 {
		t.Errorf("p99 spread = %v..%v, want 1..10", spread.P99MsMin, spread.P99MsMax)
	}
	// The relative range is what tells a reader whether the machine was quiet.
	if math.Abs(spread.RelativeRange-4.0) > 0.001 {
		t.Errorf("RelativeRange = %v, want 4.0 ((900-100)/200)", spread.RelativeRange)
	}
}

// TestPageCacheNoteIsHonest: a large-object throughput number measured against
// a corpus that fits in RAM is memory bandwidth wearing a disk's clothes, and
// the report has to say so.
func TestPageCacheNoteIsHonest(t *testing.T) {
	const ram = 32 << 30
	warm := pageCacheNote(1<<30, ram)
	if !strings.HasPrefix(warm, "WARM") {
		t.Errorf("a 1 GiB corpus against 32 GiB of RAM is not labelled warm: %q", warm)
	}
	if !strings.Contains(warm, "device baseline") {
		t.Error("the warm note does not point at the cold-path floor")
	}

	if got := pageCacheNote(40<<30, ram); !strings.HasPrefix(got, "MIXED") {
		t.Errorf("a 40 GiB corpus against 32 GiB is not labelled mixed: %q", got)
	}
	if got := pageCacheNote(200<<30, ram); !strings.HasPrefix(got, "COLD") {
		t.Errorf("a 200 GiB corpus against 32 GiB is not labelled cold: %q", got)
	}
	if got := pageCacheNote(1<<30, 0); !strings.Contains(got, "unknown") {
		t.Errorf("unknown RAM is not reported as unknown: %q", got)
	}
}

func TestSplitMetric(t *testing.T) {
	cases := map[string]struct {
		name  string
		value float64
		ok    bool
	}{
		`process_resident_memory_bytes 4.8e+07`:       {"process_resident_memory_bytes", 4.8e7, true},
		`go_goroutines 42`:                            {"go_goroutines", 42, true},
		`kilncache_hits_total{node="a",source="x"} 7`: {"kilncache_hits_total", 7, true},
		`# HELP something`:                            {"", 0, false},
		`nonsense`:                                    {"", 0, false},
	}
	for line, want := range cases {
		name, value, ok := splitMetric(line)
		if ok != want.ok {
			t.Errorf("splitMetric(%q) ok = %v, want %v", line, ok, want.ok)
			continue
		}
		if ok && (name != want.name || value != want.value) {
			t.Errorf("splitMetric(%q) = %q,%v want %q,%v", line, name, value, want.name, want.value)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:      "512 B",
		64 << 10: "64 KiB",
		8 << 20:  "8 MiB",
		3 << 30:  "3.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestOpString(t *testing.T) {
	if OpGet.String() != "get" || OpPut.String() != "put" || OpMixed.String() != "mixed" {
		t.Error("op names are wrong")
	}
}

func TestScenariosAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, sc := range scenarios() {
		if sc.Name == "" || sc.Description == "" {
			t.Errorf("scenario %+v is missing a name or description", sc)
		}
		if seen[sc.Name] {
			t.Errorf("duplicate scenario name %q", sc.Name)
		}
		seen[sc.Name] = true
		if sc.ObjectSize <= 0 || sc.ObjectCount <= 0 {
			t.Errorf("scenario %q has a nonsensical corpus: %d objects of %d bytes",
				sc.Name, sc.ObjectCount, sc.ObjectSize)
		}
		if sc.Op == OpMixed && (sc.HitRatio <= 0 || sc.HitRatio >= 1) {
			t.Errorf("mixed scenario %q has hit ratio %v", sc.Name, sc.HitRatio)
		}
	}
}

func TestRunRequiresOutputDirectory(t *testing.T) {
	if err := run([]string{"-duration", "1s"}); err == nil {
		t.Fatal("run without -out = nil error")
	}
}

func TestRunRejectsBadConcurrency(t *testing.T) {
	if err := run([]string{"-out", t.TempDir(), "-concurrency", "nope"}); err == nil {
		t.Fatal("run accepted a non-numeric concurrency")
	}
	if err := run([]string{"-out", t.TempDir(), "-concurrency", "0"}); err == nil {
		t.Fatal("run accepted a concurrency of zero")
	}
}

// TestRenderProducesASourcedDocument is the falsifier for the claim that
// BENCHMARKS.md is generated rather than written: if render ever stops emitting
// a source link next to a table, check-numbers.sh fails the build.
func TestRenderProducesASourcedDocument(t *testing.T) {
	dir := t.TempDir()
	rep := Report{
		Kind:        "benchmark",
		GeneratedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		Host: map[string]any{
			"cpu_model": "Test CPU", "cpu_cores": float64(8),
			"mem_total_kb": float64(32 << 20), "kernel": "Linux test",
		},
		Setup: Setup{
			Targets: []string{"http://a"}, Nodes: 3, ReplicaCount: 2,
			GitCommit: "0123456789abcdef", RunsPerPoint: 5,
			DurationS: 15, WarmupS: 5, Concurrency: []int{4, 64},
			ClientColocated: true,
		},
		Scenarios: []ScenarioResult{{
			Name:        "get-64k",
			Description: "small reads",
			Corpus: map[string]any{
				"object_size_bytes": float64(65536), "objects": float64(100),
				"total_bytes": float64(6553600), "seed": float64(1),
			},
			PageCacheNote: "WARM: everything fits",
			Points: []Point{{
				Concurrency: 64,
				Median:      RunResult{OpsPerSec: 46058, MiBPerSec: 2878.6, Latency: Summary{P50Ms: 1.14, P99Ms: 5.13}},
				Spread:      Spread{OpsPerSecMin: 45894, OpsPerSecMax: 46263},
			}},
			Memory: &MemoryObservation{Scenario: "get-64k", ObjectSize: 65536, PeakRSSMiB: 101.9},
		}},
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bench-latest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "BENCHMARKS.md")
	if err := render(dir, out); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(got)

	for _, want := range []string{
		"Do not edit by hand",
		"median of 5 runs",
		"get-64k",
		"46058",
		"Source:",
		"bench-latest.json",
		"WARM: everything fits",
		"Memory does not scale with object size",
		"0123456789ab",
		"What is not measured here",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("generated document is missing %q", want)
		}
	}

	// Every table must have a source link somewhere above it in its section.
	if strings.Count(doc, "Source:") < 2 {
		t.Errorf("generated document has fewer source links than tables")
	}
}

func TestRenderFailsOnMissingResults(t *testing.T) {
	if err := render(t.TempDir(), filepath.Join(t.TempDir(), "out.md")); err == nil {
		t.Fatal("render with no bench-latest.json = nil error")
	}
}

func TestShortCommit(t *testing.T) {
	if got := shortCommit("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("shortCommit = %q", got)
	}
	if got := shortCommit(""); got != "unknown" {
		t.Errorf("shortCommit(\"\") = %q", got)
	}
	if got := shortCommit("abc"); got != "abc" {
		t.Errorf("shortCommit(short) = %q", got)
	}
}

func TestByteReader(t *testing.T) {
	src := []byte("hello world")
	r := newByteReader(src)
	buf := make([]byte, 4)
	var out []byte
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(out, src) {
		t.Errorf("read %q, want %q", out, src)
	}
}

func TestMaxIntAndMinInt(t *testing.T) {
	if maxInt([]int{4, 64, 16}) != 64 {
		t.Error("maxInt")
	}
	if maxInt(nil) != 0 {
		t.Error("maxInt(nil)")
	}
	if minInt(3, 5) != 3 || minInt(5, 3) != 3 {
		t.Error("minInt")
	}
}
