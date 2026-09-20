package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The chaos runner's own logic decides whether a run passes, so a bug here
// invalidates the result rather than merely reporting it wrongly. That has
// already happened once: the verdict counted evicted objects as data loss
// (docs/bugs.md, entry 9).

func TestLedgerRecordsAcknowledgedWrites(t *testing.T) {
	l := newLedger()
	l.add("aaa", 10)
	l.add("bbb", 20)
	l.add("aaa", 999) // duplicate: first size wins, no second entry

	if l.count() != 2 {
		t.Fatalf("count = %d, want 2", l.count())
	}
	keys := l.all()
	if len(keys) != 2 {
		t.Fatalf("all() = %v", keys)
	}
	r := rand.New(rand.NewSource(1))
	k, size, ok := l.random(r)
	if !ok {
		t.Fatal("random() on a non-empty ledger returned nothing")
	}
	if k == "aaa" && size != 10 {
		t.Errorf("size for aaa = %d, want the first recorded size 10", size)
	}
}

func TestLedgerRandomOnEmpty(t *testing.T) {
	l := newLedger()
	if _, _, ok := l.random(rand.New(rand.NewSource(1))); ok {
		t.Error("random() on an empty ledger reported success")
	}
}

// TestRefusedWritesAreTrackedSeparately is the distinction the convergence
// measurement rests on. An acknowledged write had two copies at the moment it
// was acknowledged, so it is replicated by construction and proves nothing
// about repair. A refused write left a single copy behind, and that is what
// repair has to fix.
func TestRefusedWritesAreTrackedSeparately(t *testing.T) {
	l := newLedger()
	l.addRefused("r1")
	l.addRefused("r2")
	l.addRefused("r1") // idempotent

	refused := l.refusedKeys()
	if len(refused) != 2 {
		t.Fatalf("refusedKeys = %v, want 2 entries", refused)
	}
	if l.count() != 0 {
		t.Error("a refused write was counted as acknowledged")
	}

	// Once the same object is written successfully, it is no longer an
	// interesting under-replicated case.
	l.add("r1", 5)
	refused = l.refusedKeys()
	if len(refused) != 1 || refused[0] != "r2" {
		t.Errorf("refusedKeys after r1 succeeded = %v, want only r2", refused)
	}

	// And a refusal recorded after a success must not resurrect it.
	l.addRefused("r1")
	if got := l.refusedKeys(); len(got) != 1 {
		t.Errorf("refusedKeys = %v after re-refusing an acknowledged key", got)
	}
}

func TestLedgerIsConcurrencySafe(t *testing.T) {
	l := newLedger()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < 200; i++ {
				l.add(string(rune('a'+w))+string(rune('0'+i%10)), int64(i))
				l.addRefused("refused-" + string(rune('a'+w)))
				l.random(r)
				l.all()
			}
		}(w)
	}
	wg.Wait()
	if l.count() == 0 {
		t.Error("nothing was recorded")
	}
}

func TestVerdictCorruptionAlwaysFails(t *testing.T) {
	var rep report
	rep.Correctness.CorruptedReads = 1
	rep.Eviction.Observed = true // eviction never excuses corruption
	if v := verdict(&rep); !strings.HasPrefix(v, "FAIL") {
		t.Errorf("verdict = %q, want FAIL for a corrupted read", v)
	}

	rep = report{}
	rep.FinalVerification.Corrupted = 1
	if v := verdict(&rep); !strings.HasPrefix(v, "FAIL") {
		t.Errorf("verdict = %q, want FAIL for a corrupted object", v)
	}
}

// TestVerdictDistinguishesEvictionFromLoss is the falsifier for bug 9.
func TestVerdictDistinguishesEvictionFromLoss(t *testing.T) {
	// Missing objects with no eviction observed: real data loss.
	var loss report
	loss.FinalVerification.Missing = 5
	loss.Eviction.Observed = false
	if v := verdict(&loss); !strings.Contains(v, "FAIL") {
		t.Errorf("verdict = %q, want FAIL when objects vanished and nothing was evicted", v)
	}

	// The same count with eviction active: the cache did its job.
	var evicted report
	evicted.FinalVerification.Missing = 5
	evicted.Eviction.Observed = true
	evicted.Eviction.ObjectsEvicted = 1000
	if v := verdict(&evicted); v != "PASS" {
		t.Errorf("verdict = %q, want PASS when eviction explains the missing objects", v)
	}
}

func TestVerdictRequiresConvergence(t *testing.T) {
	var rep report
	rep.Convergence.Attempted = true
	rep.Convergence.Converged = false
	rep.Convergence.UnderReplicated = 7
	rep.Convergence.WaitedSeconds = 120
	v := verdict(&rep)
	if !strings.Contains(v, "FAIL") || !strings.Contains(v, "under-replicated") {
		t.Errorf("verdict = %q, want a convergence failure", v)
	}
}

// TestVerdictFlagsAFaultlessRunAsInconclusive: a chaos run that injected faults
// and saw no client error at all did not actually exercise anything. Reporting
// that as a pass would be the most flattering possible lie.
func TestVerdictFlagsAFaultlessRunAsInconclusive(t *testing.T) {
	var rep report
	rep.Config.FaultsEnabled = true
	rep.Faults = []faultEvent{{Node: "node-a"}}
	rep.Traffic.ErrorRate = 0
	if v := verdict(&rep); !strings.Contains(v, "INCONCLUSIVE") {
		t.Errorf("verdict = %q, want INCONCLUSIVE when faults produced no errors", v)
	}

	// With errors observed, the same run is a genuine pass.
	rep.Traffic.ErrorRate = 0.12
	if v := verdict(&rep); v != "PASS" {
		t.Errorf("verdict = %q, want PASS", v)
	}

	// A control run with faults disabled and no errors is fine.
	var control report
	control.Config.FaultsEnabled = false
	if v := verdict(&control); v != "PASS" {
		t.Errorf("control run verdict = %q, want PASS", v)
	}
}

func TestSubsampleIsBoundedAndSpread(t *testing.T) {
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = string(rune('a' + i%26))
	}
	got := subsample(keys, 10)
	if len(got) > 10 {
		t.Errorf("subsample returned %d, want at most 10", len(got))
	}
	if len(got) == 0 {
		t.Error("subsample returned nothing")
	}
	if n := len(subsample(keys, 0)); n != 0 {
		t.Errorf("subsample(_, 0) = %d entries", n)
	}
	if n := len(subsample(nil, 10)); n != 0 {
		t.Errorf("subsample(nil, _) = %d entries", n)
	}
	// Asking for more than exist returns everything.
	short := []string{"a", "b"}
	if n := len(subsample(short, 10)); n != 2 {
		t.Errorf("subsample of 2 keys asking for 10 = %d", n)
	}
}

func TestParseNodes(t *testing.T) {
	names, err := parseNodes("node-a=http://a:8080, node-b=http://b:8080/ ")
	if err != nil {
		t.Fatalf("parseNodes: %v", err)
	}
	if len(names) != 2 || names[0] != "node-a" || names[1] != "node-b" {
		t.Errorf("names = %v", names)
	}
	if nodeOrder[1].URL != "http://b:8080" {
		t.Errorf("trailing slash not trimmed: %q", nodeOrder[1].URL)
	}
	if _, err := parseNodes("node-a"); err == nil {
		t.Error("parseNodes accepted an entry with no url")
	}
}

func TestClassifyErrors(t *testing.T) {
	var c counters
	classify(&c, errors.New("dial tcp 127.0.0.1:8080: connect: connection refused"))
	classify(&c, errors.New("context deadline exceeded"))
	classify(&c, errors.New("Client.Timeout exceeded while awaiting headers"))
	classify(&c, errors.New("something else entirely"))

	if c.ConnRefused.Load() != 1 {
		t.Errorf("ConnRefused = %d, want 1", c.ConnRefused.Load())
	}
	if c.Timeouts.Load() != 2 {
		t.Errorf("Timeouts = %d, want 2", c.Timeouts.Load())
	}
}

func TestAsInt64(t *testing.T) {
	// JSON numbers arrive as float64; the others are defensive.
	cases := map[any]int64{
		float64(42): 42,
		int64(7):    7,
		int(3):      3,
		"nope":      0,
		nil:         0,
	}
	for in, want := range cases {
		if got := asInt64(in); got != want {
			t.Errorf("asInt64(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestRepeatReader(t *testing.T) {
	src := []byte("0123456789")
	r := newRepeatReader(src)
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

func TestBuildReportComputesRates(t *testing.T) {
	var c counters
	c.Writes.Store(100)
	c.WriteOK.Store(80)
	c.WriteError.Store(20)
	c.Reads.Store(400)
	c.ReadHits.Store(390)
	c.ReadErrors.Store(10)

	opts := options{nodes: []string{"a"}, workers: 4, readRatio: 0.8, seed: 42}
	rep := buildReport(opts, &c, nil, 10*time.Second)

	if rep.Traffic.OpsPerSecond != 50 {
		t.Errorf("OpsPerSecond = %v, want 50 (500 ops in 10s)", rep.Traffic.OpsPerSecond)
	}
	want := 30.0 / 500.0
	if rep.Traffic.ErrorRate != want {
		t.Errorf("ErrorRate = %v, want %v", rep.Traffic.ErrorRate, want)
	}
	if rep.Faults == nil {
		t.Error("Faults is nil; it must serialise as [] rather than null")
	}
	if rep.Config.Seed != 42 {
		t.Errorf("seed not recorded: %d", rep.Config.Seed)
	}
}

func TestWriteReportProducesTimestampedAndLatest(t *testing.T) {
	dir := t.TempDir()
	var rep report
	rep.Kind = "chaos"
	rep.GeneratedAt = time.Date(2026, 9, 20, 8, 20, 57, 0, time.UTC)
	rep.Verdict = "PASS"
	rep.Correctness.ReadsVerified = 1234

	if err := writeReport(dir, rep); err != nil {
		t.Fatalf("writeReport: %v", err)
	}

	stamped := filepath.Join(dir, "chaos-20260920-082057.json")
	latest := filepath.Join(dir, "chaos-latest.json")
	for _, p := range []string{stamped, latest} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var got report
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s is not valid JSON: %v", p, err)
		}
		if got.Verdict != "PASS" || got.Correctness.ReadsVerified != 1234 {
			t.Errorf("%s round-tripped wrong: %+v", p, got)
		}
	}
}

func TestRunRequiresAnOutputDirectory(t *testing.T) {
	// A chaos run whose report is not written is not evidence.
	if err := run([]string{"-duration", "1s"}); err == nil {
		t.Fatal("run without -out = nil error")
	}
}
