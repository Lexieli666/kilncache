package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ledger is the independent record of what was written.
//
// Independent is the whole point: the expected digest is computed here, from
// the bytes this process generated, and never read back from the server. A
// verifier that asked the server what the digest should be would pass for
// exactly the bug it exists to find.
type ledger struct {
	mu   sync.RWMutex
	keys []string
	want map[string]string // key -> expected sha256 (which is the key, for CAS)
	size map[string]int64

	// refused holds objects the cluster answered 503 to, because a holder was
	// down and the second copy could not be written.
	//
	// These are deliberately kept apart from acknowledged writes. They must NOT
	// count as data loss if they are absent -- the server never promised to
	// keep them. But they are the only objects in the run that are genuinely
	// under-replicated, because an acknowledged write had both copies at the
	// moment it was acknowledged and a stopped container comes back with its
	// named volume intact. Measuring convergence without them would be
	// measuring a property that holds by construction: the interesting question
	// is whether the repair worker notices the single-copy objects a refused
	// write left behind and makes the second copy once the peer returns.
	refused    []string
	refusedSet map[string]struct{}
}

func newLedger() *ledger {
	return &ledger{
		want:       map[string]string{},
		size:       map[string]int64{},
		refusedSet: map[string]struct{}{},
	}
}

func (l *ledger) addRefused(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.refusedSet[key]; ok {
		return
	}
	// An object that was later written successfully is no longer interesting
	// as a refused one.
	if _, ok := l.want[key]; ok {
		return
	}
	l.refusedSet[key] = struct{}{}
	l.refused = append(l.refused, key)
}

// refusedKeys returns the refused objects that were never later acknowledged.
func (l *ledger) refusedKeys() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.refused))
	for _, k := range l.refused {
		if _, acked := l.want[k]; !acked {
			out = append(out, k)
		}
	}
	return out
}

func (l *ledger) add(key string, size int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.want[key]; ok {
		return
	}
	l.want[key] = key
	l.size[key] = size
	l.keys = append(l.keys, key)
}

func (l *ledger) random(r *rand.Rand) (string, int64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.keys) == 0 {
		return "", 0, false
	}
	k := l.keys[r.Intn(len(l.keys))]
	return k, l.size[k], true
}

func (l *ledger) all() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, len(l.keys))
	copy(out, l.keys)
	return out
}

func (l *ledger) count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.keys)
}

// counters are the run's totals. Every absence claim in the report is published
// next to the count of observations that back it.
type counters struct {
	Writes         atomic.Int64
	WriteOK        atomic.Int64
	WriteRefused   atomic.Int64 // 503: the cluster correctly refused an under-replicated write
	WriteError     atomic.Int64
	Reads          atomic.Int64
	ReadHits       atomic.Int64
	ReadMisses     atomic.Int64
	ReadErrors     atomic.Int64
	ReadsVerified  atomic.Int64
	CorruptedReads atomic.Int64
	BytesWritten   atomic.Int64
	BytesRead      atomic.Int64
	ConnRefused    atomic.Int64
	Timeouts       atomic.Int64
	ServerErrors   atomic.Int64
}

type faultEvent struct {
	Node      string    `json:"node"`
	StoppedAt time.Time `json:"stopped_at"`
	StartedAt time.Time `json:"started_at"`
	DownFor   string    `json:"down_for"`
	StopErr   string    `json:"stop_error,omitempty"`
	StartErr  string    `json:"start_error,omitempty"`
}

type report struct {
	Kind        string    `json:"kind"`
	GeneratedAt time.Time `json:"generated_at"`
	Host        any       `json:"host,omitempty"`

	Config struct {
		Nodes         []string `json:"nodes"`
		DurationS     float64  `json:"duration_seconds"`
		Workers       int      `json:"workers"`
		ReadRatio     float64  `json:"read_ratio"`
		ObjectMin     int      `json:"object_min_bytes"`
		ObjectMax     int      `json:"object_max_bytes"`
		FaultEveryS   float64  `json:"fault_every_seconds"`
		FaultDownS    float64  `json:"fault_down_seconds"`
		Seed          int64    `json:"seed"`
		FaultsEnabled bool     `json:"faults_enabled"`
	} `json:"config"`

	Traffic struct {
		Writes        int64   `json:"writes"`
		WritesOK      int64   `json:"writes_ok"`
		WritesRefused int64   `json:"writes_refused_503"`
		WriteErrors   int64   `json:"write_errors"`
		Reads         int64   `json:"reads"`
		ReadHits      int64   `json:"read_hits"`
		ReadMisses    int64   `json:"read_misses"`
		ReadErrors    int64   `json:"read_errors"`
		BytesWritten  int64   `json:"bytes_written"`
		BytesRead     int64   `json:"bytes_read"`
		OpsPerSecond  float64 `json:"ops_per_second"`
		ErrorRate     float64 `json:"error_rate"`
		ConnRefused   int64   `json:"connection_refused"`
		Timeouts      int64   `json:"timeouts"`
		ServerErrors  int64   `json:"server_5xx"`
	} `json:"traffic"`

	// Correctness is the headline. Both numbers are always published together:
	// "zero corrupted reads" with no count of reads checked is not a claim.
	Correctness struct {
		ReadsVerified  int64 `json:"reads_verified"`
		CorruptedReads int64 `json:"corrupted_reads"`
	} `json:"correctness"`

	Faults []faultEvent `json:"faults"`

	Convergence struct {
		Attempted         bool    `json:"attempted"`
		ObjectsChecked    int     `json:"objects_checked"`
		FromRefusedWrites int     `json:"objects_from_refused_writes"`
		FromAckedWrites   int     `json:"objects_from_acknowledged_writes"`
		AtStartReplicated int     `json:"fully_replicated_at_first_check"`
		FullyReplicated   int     `json:"fully_replicated"`
		UnderReplicated   int     `json:"under_replicated"`
		Absent            int     `json:"absent_everywhere"`
		Unreadable        int     `json:"unreadable"`
		ConvergedSeconds  float64 `json:"converged_after_seconds"`
		Converged         bool    `json:"converged"`
		WaitedSeconds     float64 `json:"waited_seconds"`
		Note              string  `json:"note"`
	} `json:"convergence"`

	FinalVerification struct {
		ObjectsChecked int    `json:"objects_checked"`
		Correct        int    `json:"correct"`
		Corrupted      int    `json:"corrupted"`
		Missing        int    `json:"missing"`
		Unreadable     int64  `json:"unreadable"`
		Note           string `json:"note,omitempty"`
	} `json:"final_verification"`

	// NodeStats is each node's own view at the end of the run, read from
	// /stats. It is what distinguishes "this object was evicted under quota
	// pressure" from "this object was lost" -- from outside, both are a 404.
	NodeStats map[string]any `json:"node_stats,omitempty"`

	// Eviction summarises what the cluster reclaimed, so that a missing object
	// can be attributed rather than assumed.
	Eviction struct {
		Observed             bool  `json:"observed"`
		ObjectsEvicted       int64 `json:"objects_evicted"`
		BytesReclaimed       int64 `json:"bytes_reclaimed"`
		AnyNodeOverHighWater bool  `json:"any_node_over_high_water"`
	} `json:"eviction"`

	Verdict string `json:"verdict"`
}

func runChaos(opts options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
		},
	}

	fmt.Fprintf(os.Stderr, "kilnchaos: %d nodes, %v, %d workers, read ratio %.2f, seed %d\n",
		len(nodeOrder), opts.duration, opts.workers, opts.readRatio, opts.seed)
	if opts.noFaults {
		fmt.Fprintln(os.Stderr, "kilnchaos: faults disabled (control run)")
	}

	if err := waitAllReady(ctx, client, 60*time.Second); err != nil {
		return err
	}

	led := newLedger()
	var c counters
	var faultsMu sync.Mutex
	var faults []faultEvent

	trafficCtx, endTraffic := context.WithTimeout(ctx, opts.duration)
	defer endTraffic()

	var wg sync.WaitGroup
	for i := 0; i < opts.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			worker(trafficCtx, client, opts, led, &c, rand.New(rand.NewSource(opts.seed+int64(id)*7919)))
		}(i)
	}

	faultDone := make(chan struct{})
	go func() {
		defer close(faultDone)
		if opts.noFaults {
			return
		}
		injectFaults(trafficCtx, opts, func(e faultEvent) {
			faultsMu.Lock()
			faults = append(faults, e)
			faultsMu.Unlock()
		})
	}()

	start := time.Now()
	wg.Wait()
	<-faultDone
	elapsed := time.Since(start)

	fmt.Fprintf(os.Stderr, "kilnchaos: traffic finished after %v; %d objects written\n",
		elapsed.Round(time.Second), led.count())

	rep := buildReport(opts, &c, faults, elapsed)

	// Everything must be back before convergence can be judged. A node that is
	// still down has not failed to converge; it has not been asked to.
	if err := restoreAll(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "kilnchaos: could not restore all nodes: %v\n", err)
	}
	if err := waitAllReady(context.Background(), client, 120*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "kilnchaos: not all nodes came back: %v\n", err)
	}

	collectNodeStats(context.Background(), client, &rep)
	measureConvergence(context.Background(), client, opts, led, &rep)
	finalVerify(context.Background(), client, opts, led, &c, &rep)

	rep.Correctness.ReadsVerified = c.ReadsVerified.Load()
	rep.Correctness.CorruptedReads = c.CorruptedReads.Load()
	rep.Verdict = verdict(&rep)

	if err := writeReport(opts.out, rep); err != nil {
		return err
	}
	printSummary(&rep)

	if rep.Correctness.CorruptedReads > 0 || rep.FinalVerification.Corrupted > 0 {
		return fmt.Errorf("chaos run found %d corrupted reads during traffic and %d corrupted objects at the end",
			rep.Correctness.CorruptedReads, rep.FinalVerification.Corrupted)
	}
	return nil
}

func buildReport(opts options, c *counters, faults []faultEvent, elapsed time.Duration) report {
	var rep report
	rep.Kind = "chaos"
	rep.GeneratedAt = time.Now().UTC()
	rep.Config.Nodes = opts.nodes
	rep.Config.DurationS = elapsed.Seconds()
	rep.Config.Workers = opts.workers
	rep.Config.ReadRatio = opts.readRatio
	rep.Config.ObjectMin = opts.objectMin
	rep.Config.ObjectMax = opts.objectMax
	rep.Config.FaultEveryS = opts.faultEvery.Seconds()
	rep.Config.FaultDownS = opts.faultDown.Seconds()
	rep.Config.Seed = opts.seed
	rep.Config.FaultsEnabled = !opts.noFaults
	rep.Faults = faults
	if rep.Faults == nil {
		rep.Faults = []faultEvent{}
	}

	rep.Traffic.Writes = c.Writes.Load()
	rep.Traffic.WritesOK = c.WriteOK.Load()
	rep.Traffic.WritesRefused = c.WriteRefused.Load()
	rep.Traffic.WriteErrors = c.WriteError.Load()
	rep.Traffic.Reads = c.Reads.Load()
	rep.Traffic.ReadHits = c.ReadHits.Load()
	rep.Traffic.ReadMisses = c.ReadMisses.Load()
	rep.Traffic.ReadErrors = c.ReadErrors.Load()
	rep.Traffic.BytesWritten = c.BytesWritten.Load()
	rep.Traffic.BytesRead = c.BytesRead.Load()
	rep.Traffic.ConnRefused = c.ConnRefused.Load()
	rep.Traffic.Timeouts = c.Timeouts.Load()
	rep.Traffic.ServerErrors = c.ServerErrors.Load()

	ops := rep.Traffic.Writes + rep.Traffic.Reads
	if elapsed > 0 {
		rep.Traffic.OpsPerSecond = float64(ops) / elapsed.Seconds()
	}
	if ops > 0 {
		rep.Traffic.ErrorRate = float64(rep.Traffic.WriteErrors+rep.Traffic.ReadErrors) / float64(ops)
	}
	return rep
}

func verdict(rep *report) string {
	switch {
	case rep.Correctness.CorruptedReads > 0 || rep.FinalVerification.Corrupted > 0:
		return "FAIL: corrupted data was served"
	case rep.FinalVerification.Missing > 0 && !rep.Eviction.Observed:
		// Only data loss when nothing was evicted. A cache that evicted objects
		// to stay inside its quota has not lost them; it has done its job. An
		// earlier version of this tool conflated the two and reported 1,266
		// evictions as data loss (docs/bugs.md, entry 7).
		return fmt.Sprintf("FAIL: %d acknowledged objects are gone and no eviction was observed",
			rep.FinalVerification.Missing)
	case rep.Convergence.Attempted && !rep.Convergence.Converged:
		return fmt.Sprintf("FAIL: %d objects were still under-replicated after %.0fs",
			rep.Convergence.UnderReplicated, rep.Convergence.WaitedSeconds)
	case rep.Config.FaultsEnabled && rep.Traffic.ErrorRate == 0 && len(rep.Faults) > 0:
		// A chaos run with no errors at all means the faults did not land, so
		// the run proves nothing. Saying so is more useful than a clean report.
		return "INCONCLUSIVE: faults were injected but no client error was observed; the faults may not have landed"
	default:
		return "PASS"
	}
}

func worker(ctx context.Context, client *http.Client, opts options, led *ledger, c *counters, r *rand.Rand) {
	buf := make([]byte, opts.objectMax)
	for {
		if ctx.Err() != nil {
			return
		}
		if r.Float64() < opts.readRatio {
			doRead(ctx, client, opts, led, c, r)
		} else {
			doWrite(ctx, client, opts, led, c, r, buf)
		}
	}
}

func doWrite(ctx context.Context, client *http.Client, opts options, led *ledger, c *counters, r *rand.Rand, buf []byte) {
	size := opts.objectMin
	if opts.objectMax > opts.objectMin {
		size += r.Intn(opts.objectMax - opts.objectMin)
	}
	content := buf[:size]
	// Fill with bytes derived from this worker's RNG so that two objects are
	// never accidentally identical, which would make a mix-up invisible.
	for i := range content {
		content[i] = byte(r.Uint32())
	}
	sum := sha256.Sum256(content)
	key := hex.EncodeToString(sum[:])

	base := nodeOrder[r.Intn(len(nodeOrder))].URL
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/cas/"+key, newRepeatReader(content))
	if err != nil {
		return
	}
	req.ContentLength = int64(size)

	c.Writes.Add(1)
	resp, err := client.Do(req)
	if err != nil {
		classify(c, err)
		c.WriteError.Add(1)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		c.WriteOK.Add(1)
		c.BytesWritten.Add(int64(size))
		// Only acknowledged writes go in the ledger. An object the cluster
		// refused is not one it promised to keep, and demanding it later would
		// make the report claim data loss where the server was honest.
		led.add(key, int64(size))
	case resp.StatusCode == http.StatusServiceUnavailable:
		c.WriteRefused.Add(1)
		// Not acknowledged, so not a durability promise -- but very likely
		// sitting on one node as a single copy, which is exactly what repair
		// should heal. Convergence is measured over these.
		led.addRefused(key)
	default:
		c.WriteError.Add(1)
		if resp.StatusCode >= 500 {
			c.ServerErrors.Add(1)
		}
	}
}

func doRead(ctx context.Context, client *http.Client, opts options, led *ledger, c *counters, r *rand.Rand) {
	key, size, ok := led.random(r)
	if !ok {
		return
	}
	base := nodeOrder[r.Intn(len(nodeOrder))].URL

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/cas/"+key, http.NoBody)
	if err != nil {
		return
	}
	c.Reads.Add(1)
	resp, err := client.Do(req)
	if err != nil {
		classify(c, err)
		c.ReadErrors.Add(1)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		h := sha256.New()
		n, err := io.Copy(h, resp.Body)
		if err != nil {
			c.ReadErrors.Add(1)
			return
		}
		c.ReadHits.Add(1)
		c.BytesRead.Add(n)
		c.ReadsVerified.Add(1)
		// Compared against this process's own record, never the server's.
		if hex.EncodeToString(h.Sum(nil)) != key || n != size {
			c.CorruptedReads.Add(1)
			fmt.Fprintf(os.Stderr, "kilnchaos: CORRUPTED READ key=%s expected %d bytes, got %d with digest %s\n",
				key[:12], size, n, hex.EncodeToString(h.Sum(nil))[:12])
		}
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		c.ReadMisses.Add(1)
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		c.ReadErrors.Add(1)
		if resp.StatusCode >= 500 {
			c.ServerErrors.Add(1)
		}
	}
}

// classify buckets a transport error for the report's "errors by class" line.
//
// The match is case-insensitive because Go's own client reports a timeout as
// "Client.Timeout exceeded while awaiting headers" -- capital T. A
// case-sensitive check silently put those in no bucket at all, so the report
// under-counted exactly the errors an injected network fault produces.
func classify(c *counters, err error) {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"),
		strings.Contains(msg, "connection reset"):
		c.ConnRefused.Add(1)
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		c.Timeouts.Add(1)
	}
}

// repeatReader serves a byte slice without copying it, so a worker can reuse
// one buffer for every write instead of allocating per request.
type repeatReader struct {
	b   []byte
	pos int
}

func newRepeatReader(b []byte) *repeatReader { return &repeatReader{b: b} }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
