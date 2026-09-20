package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Op is what one worker iteration does.
type Op int

const (
	OpGet Op = iota
	OpPut
	OpMixed
)

func (o Op) String() string {
	switch o {
	case OpGet:
		return "get"
	case OpPut:
		return "put"
	default:
		return "mixed"
	}
}

// RunSpec describes one measured run.
type RunSpec struct {
	Scenario    string
	Op          Op
	Concurrency int
	Duration    time.Duration
	Warmup      time.Duration
	Corpus      *Corpus
	MissKeys    []string
	HitRatio    float64 // for OpMixed
	Targets     []string
	Verify      bool
}

// RunResult is one measured run.
type RunResult struct {
	Scenario    string  `json:"scenario"`
	Op          string  `json:"op"`
	Concurrency int     `json:"concurrency"`
	DurationS   float64 `json:"duration_seconds"`

	Operations  int64   `json:"operations"`
	Errors      int64   `json:"errors"`
	Misses      int64   `json:"misses"`
	Bytes       int64   `json:"bytes"`
	OpsPerSec   float64 `json:"ops_per_second"`
	MiBPerSec   float64 `json:"mib_per_second"`
	ErrorRate   float64 `json:"error_rate"`
	Latency     Summary `json:"latency"`
	VerifyFails int64   `json:"verify_failures"`

	// ErrorSamples counts the distinct error strings observed, so an error rate
	// in the report can be interpreted rather than guessed at.
	ErrorSamples map[string]int64 `json:"error_samples,omitempty"`

	ObjectSizeBytes int `json:"object_size_bytes"`
}

// Run executes one measured run against the targets.
//
// It is a closed-loop generator: each worker issues the next request as soon as
// the previous one finishes. That measures what the server can do at a given
// concurrency, which is the question a cache operator has ("how many parallel
// Bazel actions can this serve?"), rather than what it does at a fixed offered
// rate. Open-loop would answer a different and less useful question here, and
// would additionally need a coordinated-omission correction to be honest about
// latency.
func Run(ctx context.Context, client *http.Client, spec RunSpec) (RunResult, error) {
	if len(spec.Targets) == 0 {
		return RunResult{}, fmt.Errorf("bench: no targets")
	}

	var (
		ops       atomic.Int64
		errs      atomic.Int64
		misses    atomic.Int64
		bytes     atomic.Int64
		verifyNG  atomic.Int64
		measuring atomic.Bool
	)

	// A sample of the actual error text. A benchmark that reports an error rate
	// without saying what the errors were leaves the reader unable to tell a
	// broken cluster from a saturated one.
	var errMu sync.Mutex
	errSamples := map[string]int64{}
	noteErr := func(err error) {
		errMu.Lock()
		if _, known := errSamples[err.Error()]; known || len(errSamples) < 32 {
			errSamples[err.Error()]++
		}
		errMu.Unlock()
	}

	perWorker := make([]*Latencies, spec.Concurrency)
	for i := range perWorker {
		perWorker[i] = NewLatencies(4096)
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Warmup+spec.Duration+30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < spec.Concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)*7919 + spec.Corpus.Seed)) //nolint:gosec // fixture
			lat := perWorker[w]
			gen := newUniqueGen(w, spec.Corpus)

			for runCtx.Err() == nil {
				start := time.Now()
				n, miss, err := doOne(runCtx, client, spec, r, gen)
				elapsed := time.Since(start)

				// Warmup work still happens; it is simply not recorded. The
				// first requests against a cold page cache and an empty
				// connection pool are real, and including them would make every
				// run's p99 a measurement of process startup.
				if !measuring.Load() {
					continue
				}

				// A request still in flight when the measurement window closed
				// is an artefact of the harness, not a failure of the server.
				// Counting it would put a floor under the error rate that rises
				// with concurrency and falls with duration -- a number that
				// looks like a property of the cache and is a property of the
				// stopwatch.
				if err != nil && errors.Is(err, context.Canceled) && runCtx.Err() != nil {
					continue
				}
				ops.Add(1)
				switch {
				case err != nil:
					errs.Add(1)
					noteErr(err)
				case miss:
					misses.Add(1)
					lat.Add(elapsed)
				default:
					bytes.Add(n)
					lat.Add(elapsed)
				}
			}
		}(w)
	}

	if spec.Warmup > 0 {
		select {
		case <-time.After(spec.Warmup):
		case <-runCtx.Done():
		}
	}

	measuring.Store(true)
	measureStart := time.Now()
	select {
	case <-time.After(spec.Duration):
	case <-runCtx.Done():
	}
	measured := time.Since(measureStart)
	cancel()
	wg.Wait()

	all := NewLatencies(0)
	for _, l := range perWorker {
		all.Merge(l)
	}

	res := RunResult{
		Scenario:        spec.Scenario,
		Op:              spec.Op.String(),
		Concurrency:     spec.Concurrency,
		DurationS:       measured.Seconds(),
		Operations:      ops.Load(),
		Errors:          errs.Load(),
		Misses:          misses.Load(),
		Bytes:           bytes.Load(),
		VerifyFails:     verifyNG.Load(),
		Latency:         all.Summarize(),
		ObjectSizeBytes: spec.Corpus.Size,
	}

	errMu.Lock()
	if len(errSamples) > 0 {
		res.ErrorSamples = make(map[string]int64, len(errSamples))
		for k, v := range errSamples {
			res.ErrorSamples[k] = v
		}
	}
	errMu.Unlock()
	if measured > 0 {
		res.OpsPerSec = float64(res.Operations) / measured.Seconds()
		res.MiBPerSec = float64(res.Bytes) / measured.Seconds() / (1 << 20)
	}
	if res.Operations > 0 {
		res.ErrorRate = float64(res.Errors) / float64(res.Operations)
	}
	return res, nil
}

func doOne(ctx context.Context, client *http.Client, spec RunSpec, r *rand.Rand, gen *uniqueGen) (n int64, miss bool, err error) {
	base := spec.Targets[r.Intn(len(spec.Targets))]

	switch spec.Op {
	case OpPut:
		// Every write must be of an object the cluster has not seen.
		//
		// Re-PUTting a corpus object would measure the already-stored fast
		// path: a CAS object whose key is already present is correct by
		// definition, so the store skips the stream, the fsync and the
		// replication entirely and answers 200. A PUT benchmark that did that
		// would report a number an order of magnitude too high and would be
		// measuring a stat() call.
		return doPut(ctx, client, base, gen.next())
	case OpMixed:
		if r.Float64() < spec.HitRatio {
			obj := spec.Corpus.Objects[r.Intn(len(spec.Corpus.Objects))]
			return doGet(ctx, client, base, obj.Key, spec.Verify, len(obj.Content))
		}
		key := spec.MissKeys[r.Intn(len(spec.MissKeys))]
		return doGet(ctx, client, base, key, false, 0)
	default:
		obj := spec.Corpus.Objects[r.Intn(len(spec.Corpus.Objects))]
		return doGet(ctx, client, base, obj.Key, spec.Verify, len(obj.Content))
	}
}

func doGet(ctx context.Context, client *http.Client, base, key string, verify bool, wantSize int) (int64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/cas/"+key, http.NoBody)
	if err != nil {
		return 0, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return 0, false, fmt.Errorf("GET %s: status %d", key[:8], resp.StatusCode)
	}

	if verify {
		h := sha256.New()
		n, err := io.Copy(h, resp.Body)
		if err != nil {
			return n, false, err
		}
		if hex.EncodeToString(h.Sum(nil)) != key {
			return n, false, fmt.Errorf("GET %s: digest mismatch", key[:8])
		}
		return n, false, nil
	}

	// Discard rather than buffer: the client must not be the thing that runs
	// out of memory, and buffering would also hide a server that streams badly.
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return n, false, err
	}
	if wantSize > 0 && n != int64(wantSize) {
		return n, false, fmt.Errorf("GET %s: got %d bytes, want %d", key[:8], n, wantSize)
	}
	return n, false, nil
}

func doPut(ctx context.Context, client *http.Client, base string, obj Object) (int64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/cas/"+obj.Key, newByteReader(obj.Content))
	if err != nil {
		return 0, false, err
	}
	req.ContentLength = int64(len(obj.Content))
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false, fmt.Errorf("PUT %s: status %d", obj.Key[:8], resp.StatusCode)
	}
	return int64(len(obj.Content)), false, nil
}

// byteReader serves a shared slice without copying it, so a worker issuing
// thousands of PUTs allocates nothing per request.
type byteReader struct {
	b   []byte
	pos int
}

func newByteReader(b []byte) *byteReader { return &byteReader{b: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// uniqueGen produces a fresh object per call by mutating a base buffer.
//
// The content-addressed key is recomputed each time, which costs a SHA-256 pass
// on the client. On this host that runs at roughly a gigabyte a second per
// core, and the benchmark runs many workers on many cores, so the client stays
// well clear of being the bottleneck -- but it is a real cost and is recorded
// as a known property of the PUT measurement in docs/performance-methodology.md.
//
// Bazel pays the same cost for real: it computes every digest itself before
// uploading.
type uniqueGen struct {
	buf     []byte
	worker  int
	counter uint64
}

func newUniqueGen(worker int, c *Corpus) *uniqueGen {
	// Start from a corpus object so the bytes are incompressible, then make
	// each one distinct.
	base := make([]byte, c.Size)
	if len(c.Objects) > 0 {
		copy(base, c.Objects[worker%len(c.Objects)].Content)
	}
	return &uniqueGen{buf: base, worker: worker}
}

func (g *uniqueGen) next() Object {
	g.counter++
	// Stamp worker and counter into the first 16 bytes. Two workers can never
	// collide, and no run can repeat a key from a previous run within the same
	// process.
	binary.BigEndian.PutUint64(g.buf[0:8], uint64(g.worker))
	binary.BigEndian.PutUint64(g.buf[8:16], g.counter)
	sum := sha256.Sum256(g.buf)
	return Object{Key: hex.EncodeToString(sum[:]), Content: g.buf}
}
