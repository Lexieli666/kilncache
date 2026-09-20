package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lexieli666/kilncache/internal/buildinfo"
)

// Scenario is one thing worth measuring.
type Scenario struct {
	Name        string
	Description string
	Op          Op
	ObjectSize  int
	ObjectCount int
	HitRatio    float64
	Preload     bool // write the corpus before measuring
	Verify      bool
}

// scenarios covers the four questions the spec asks about, plus a miss-path
// case. Object counts are chosen so each corpus is large enough that the page
// cache does not serve the whole thing from memory, but small enough to
// preload in a reasonable time.
func scenarios() []Scenario {
	return []Scenario{
		{
			Name:        "get-64k",
			Description: "64 KiB cache hits: the common case, a Bazel action fetching a small output.",
			Op:          OpGet, ObjectSize: 64 << 10, ObjectCount: 4000, Preload: true,
		},
		{
			Name:        "get-8m",
			Description: "8 MiB cache hits: large object throughput, a static library or a debug binary.",
			Op:          OpGet, ObjectSize: 8 << 20, ObjectCount: 120, Preload: true,
		},
		{
			Name:        "put-replicated",
			Description: "1 MiB writes with replica count 2: every PUT places a second copy before it returns.",
			Op:          OpPut, ObjectSize: 1 << 20, ObjectCount: 2000, Preload: false,
		},
		{
			Name:        "mixed-80-20",
			Description: "64 KiB reads, 80% hits and 20% misses: the shape of a partially warm cache.",
			Op:          OpMixed, ObjectSize: 64 << 10, ObjectCount: 4000, HitRatio: 0.8, Preload: true,
		},
		{
			Name:        "get-64k-verified",
			Description: "64 KiB hits with every response body re-hashed by the client. The cost of proving correctness.",
			Op:          OpGet, ObjectSize: 64 << 10, ObjectCount: 4000, Preload: true, Verify: true,
		},
	}
}

// Report is the whole benchmark session.
type Report struct {
	Kind        string           `json:"kind"`
	GeneratedAt time.Time        `json:"generated_at"`
	Label       string           `json:"label,omitempty"`
	Smoke       bool             `json:"smoke_run"`
	Host        map[string]any   `json:"host"`
	Setup       Setup            `json:"setup"`
	Scenarios   []ScenarioResult `json:"scenarios"`
}

// Setup records everything that would change the numbers.
type Setup struct {
	Targets         []string `json:"targets"`
	Nodes           int      `json:"nodes"`
	ReplicaCount    int      `json:"replica_count"`
	GitCommit       string   `json:"git_commit"`
	Version         string   `json:"version"`
	GoVersion       string   `json:"go_version"`
	RunsPerPoint    int      `json:"runs_per_point"`
	DurationS       float64  `json:"measured_seconds_per_run"`
	WarmupS         float64  `json:"warmup_seconds_per_run"`
	Concurrency     []int    `json:"concurrency_levels"`
	CacheWarm       bool     `json:"cache_warm"`
	ClientColocated bool     `json:"client_colocated_with_servers"`
	Netem           string   `json:"netem,omitempty"`
	VerifyReads     bool     `json:"client_verifies_digests"`
	Seed            int64    `json:"corpus_seed"`
}

// ScenarioResult holds every repetition, not a selected best.
type ScenarioResult struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Corpus      map[string]any `json:"corpus"`
	Points      []Point        `json:"points"`

	// Memory records resident memory across the scenario, which is the evidence
	// for the claim that memory does not scale with object size.
	Memory *MemoryObservation `json:"memory,omitempty"`

	// PageCacheNote says plainly whether this scenario's corpus could have been
	// served entirely from the page cache. A large-object throughput number
	// measured against a corpus that fits in RAM is a measurement of memory
	// bandwidth wearing a disk's clothes, and saying so is the difference
	// between a result and a boast.
	PageCacheNote string `json:"page_cache_note"`
}

// Point is one concurrency level with all its repetitions.
type Point struct {
	Concurrency int         `json:"concurrency"`
	Runs        []RunResult `json:"runs"`
	Median      RunResult   `json:"median"`
	Spread      Spread      `json:"spread"`
}

// Spread reports how much the repetitions disagreed. A median with no spread
// beside it hides whether the machine was quiet.
type Spread struct {
	OpsPerSecMin  float64 `json:"ops_per_second_min"`
	OpsPerSecMax  float64 `json:"ops_per_second_max"`
	MiBPerSecMin  float64 `json:"mib_per_second_min"`
	MiBPerSecMax  float64 `json:"mib_per_second_max"`
	P99MsMin      float64 `json:"p99_ms_min"`
	P99MsMax      float64 `json:"p99_ms_max"`
	RelativeRange float64 `json:"ops_relative_range"`
}

func execute(ctx context.Context, opts options) error {
	if err := os.MkdirAll(opts.out, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	client := newClient(256)
	if err := waitReady(ctx, client, opts.targets); err != nil {
		return err
	}

	host := hostInfo()
	if kb, ok := host["mem_total_kb"].(float64); ok {
		opts.ramBytes = int64(kb) * 1024
	}

	rep := Report{
		Kind:        "benchmark",
		GeneratedAt: time.Now().UTC(),
		Label:       opts.label,
		Smoke:       opts.smoke,
		Host:        host,
		Setup: Setup{
			Targets: opts.targets, Nodes: len(opts.targets),
			ReplicaCount:    replicaCountOf(ctx, client, opts.targets),
			GitCommit:       gitCommit(),
			Version:         buildinfo.String(),
			GoVersion:       buildinfo.GoVersion(),
			RunsPerPoint:    opts.runs,
			DurationS:       opts.duration.Seconds(),
			WarmupS:         opts.warmup.Seconds(),
			Concurrency:     opts.concurrency,
			CacheWarm:       true,
			ClientColocated: opts.colocated,
			Netem:           opts.netem,
			VerifyReads:     opts.verify,
			Seed:            opts.seed,
		},
	}

	selected := scenarios()
	if len(opts.scenarios) > 0 {
		want := map[string]bool{}
		for _, s := range opts.scenarios {
			want[s] = true
		}
		var filtered []Scenario
		for _, s := range selected {
			if want[s.Name] {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("no scenario matched %v", opts.scenarios)
		}
		selected = filtered
	}

	for i := range selected {
		sc := selected[i]
		res, err := runScenario(ctx, client, &opts, sc)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", sc.Name, err)
		}
		rep.Scenarios = append(rep.Scenarios, res)
		if ctx.Err() != nil {
			break
		}
	}

	return writeReport(opts, rep)
}

func runScenario(ctx context.Context, client *http.Client, opts *options, sc Scenario) (ScenarioResult, error) {
	count := sc.ObjectCount
	if opts.smoke {
		count = minInt(count, 64)
	}
	corpus := NewCorpus(sc.Name, count, sc.ObjectSize, opts.seed)

	fmt.Fprintf(os.Stderr, "== %s: %d objects of %s\n", sc.Name, len(corpus.Objects), humanBytes(int64(sc.ObjectSize)))

	if sc.Preload {
		if err := preload(ctx, client, opts.targets, corpus); err != nil {
			return ScenarioResult{}, err
		}
	}

	out := ScenarioResult{
		Name: sc.Name, Description: sc.Description, Corpus: corpus.Describe(),
		PageCacheNote: pageCacheNote(corpus.Bytes(), opts.replicaFactorBytes()),
	}

	// Memory is watched across the whole scenario at the highest concurrency,
	// and the before/after samples bracket it.
	memBefore := sampleNodes(ctx, client, opts.targets)

	for _, conc := range opts.concurrency {
		point := Point{Concurrency: conc}
		// Watch memory and, at the highest concurrency, take profiles while the
		// load is actually running. A CPU profile of an idle process is a
		// flamegraph of the scheduler.
		heaviest := conc == maxInt(opts.concurrency)
		watchCtx, stopWatch := context.WithCancel(ctx)
		collectPeak := watchMemory(watchCtx, client, opts.targets)

		for i := 0; i < opts.runs; i++ {
			if ctx.Err() != nil {
				break
			}
			if heaviest && i == 0 && opts.profileDir != "" {
				// Profile the first run at the heaviest point, concurrently
				// with it. The profile window is deliberately shorter than the
				// run so it sits inside the measured period.
				profSeconds := int(opts.duration.Seconds()) - 2
				if profSeconds < 3 {
					profSeconds = 3
				}
				go func() {
					if err := collectProfiles(ctx, client, opts.targets, opts.profileDir, sc.Name, profSeconds); err != nil {
						fmt.Fprintf(os.Stderr, "   profile collection failed: %v\n", err)
					}
				}()
			}
			spec := RunSpec{
				Scenario: sc.Name, Op: sc.Op, Concurrency: conc,
				Duration: opts.duration, Warmup: opts.warmup,
				Corpus: corpus, MissKeys: corpus.MissKeys(1024, opts.seed),
				HitRatio: sc.HitRatio, Targets: opts.targets,
				Verify: sc.Verify || opts.verify,
			}
			r, err := Run(ctx, client, spec)
			if err != nil {
				stopWatch()
				collectPeak()
				return ScenarioResult{}, err
			}
			point.Runs = append(point.Runs, r)
			fmt.Fprintf(os.Stderr, "   c=%-4d run %d/%d: %8.0f ops/s  %7.1f MiB/s  p50 %6.2f ms  p99 %7.2f ms  err %.4f\n",
				conc, i+1, opts.runs, r.OpsPerSec, r.MiBPerSec, r.Latency.P50Ms, r.Latency.P99Ms, r.ErrorRate)
		}
		stopWatch()
		peak := collectPeak()
		if heaviest {
			opts.peakMemory = peak
		}

		if len(point.Runs) == 0 {
			continue
		}
		point.Median, point.Spread = summarise(point.Runs)
		out.Points = append(out.Points, point)
	}

	out.Memory = &MemoryObservation{
		Scenario: sc.Name, ObjectSize: sc.ObjectSize,
		Concurrency: maxInt(opts.concurrency),
		Before:      memBefore,
		Peak:        opts.peakMemory,
		After:       sampleNodes(ctx, client, opts.targets),
	}
	for _, s := range out.Memory.Peak {
		if mib := s.RSSBytes / (1 << 20); mib > out.Memory.PeakRSSMiB {
			out.Memory.PeakRSSMiB = mib
		}
	}
	return out, nil
}

// pageCacheNote states the relationship between the corpus and host memory.
func pageCacheNote(corpusBytes, ramBytes int64) string {
	if ramBytes <= 0 {
		return "host memory unknown; whether the corpus fits in the page cache could not be determined"
	}
	ratio := float64(corpusBytes) / float64(ramBytes)
	switch {
	case ratio < 0.5:
		return fmt.Sprintf(
			"WARM: the %s corpus is %.1f%% of the host's %s of RAM, so it can be served entirely "+
				"from the page cache. These are warm-cache numbers and do not measure the disk. "+
				"The cold-path floor is the fio device baseline in device-baseline.json.",
			humanBytes(corpusBytes), ratio*100, humanBytes(ramBytes))
	case ratio < 1.5:
		return fmt.Sprintf(
			"MIXED: the %s corpus is %.0f%% of the host's %s of RAM, so reads are partly served "+
				"from the page cache and partly from the device.",
			humanBytes(corpusBytes), ratio*100, humanBytes(ramBytes))
	default:
		return fmt.Sprintf(
			"COLD-ISH: the %s corpus is %.1fx the host's %s of RAM, so most reads reach the device.",
			humanBytes(corpusBytes), ratio, humanBytes(ramBytes))
	}
}

func maxInt(xs []int) int {
	m := 0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// summarise picks the median run by throughput and reports the spread.
//
// Median rather than best: a benchmark that reports its best run is an
// advertisement. The spread is published beside it so a reader can see whether
// the machine was quiet.
func summarise(runs []RunResult) (RunResult, Spread) {
	sorted := append([]RunResult(nil), runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OpsPerSec < sorted[j].OpsPerSec })
	median := sorted[len(sorted)/2]

	sp := Spread{
		OpsPerSecMin: sorted[0].OpsPerSec,
		OpsPerSecMax: sorted[len(sorted)-1].OpsPerSec,
		MiBPerSecMin: sorted[0].MiBPerSec,
		MiBPerSecMax: sorted[len(sorted)-1].MiBPerSec,
		P99MsMin:     sorted[0].Latency.P99Ms,
		P99MsMax:     sorted[len(sorted)-1].Latency.P99Ms,
	}
	for _, r := range sorted {
		if r.Latency.P99Ms < sp.P99MsMin {
			sp.P99MsMin = r.Latency.P99Ms
		}
		if r.Latency.P99Ms > sp.P99MsMax {
			sp.P99MsMax = r.Latency.P99Ms
		}
		if r.MiBPerSec < sp.MiBPerSecMin {
			sp.MiBPerSecMin = r.MiBPerSec
		}
		if r.MiBPerSec > sp.MiBPerSecMax {
			sp.MiBPerSecMax = r.MiBPerSec
		}
	}
	if median.OpsPerSec > 0 {
		sp.RelativeRange = (sp.OpsPerSecMax - sp.OpsPerSecMin) / median.OpsPerSec
	}
	return median, sp
}

// preload writes the corpus and confirms it is readable.
//
// Confirming matters: a scenario that measured GET throughput against objects
// that were never stored would be measuring the 404 path and reporting it as a
// hit rate.
func preload(ctx context.Context, client *http.Client, targets []string, c *Corpus) error {
	fmt.Fprintf(os.Stderr, "   preloading %s...\n", humanBytes(c.Bytes()))
	start := time.Now()

	sem := make(chan struct{}, 32)
	errCh := make(chan error, len(c.Objects))
	done := make(chan struct{})
	var inflight int

	for i := range c.Objects {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		inflight++
		go func(obj Object, base string) {
			defer func() { <-sem; done <- struct{}{} }()
			if _, _, err := doPut(ctx, client, base, obj); err != nil {
				errCh <- err
			}
		}(c.Objects[i], targets[i%len(targets)])
	}
	for i := 0; i < inflight; i++ {
		<-done
	}
	close(errCh)
	for err := range errCh {
		return fmt.Errorf("preload: %w", err)
	}

	// Spot-check that the objects really are readable.
	for _, i := range []int{0, len(c.Objects) / 2, len(c.Objects) - 1} {
		obj := c.Objects[i]
		n, miss, err := doGet(ctx, client, targets[0], obj.Key, true, len(obj.Content))
		if err != nil {
			return fmt.Errorf("preload verification: %w", err)
		}
		if miss || n != int64(len(obj.Content)) {
			return fmt.Errorf("preload verification: object %s is not readable after preload", obj.Key[:8])
		}
	}
	fmt.Fprintf(os.Stderr, "   preloaded in %v\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func waitReady(ctx context.Context, client *http.Client, targets []string) error {
	deadline := time.Now().Add(60 * time.Second)
	for _, t := range targets {
		ok := false
		for time.Now().Before(deadline) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, t+"/readyz", http.NoBody)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ok = true
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !ok {
			return fmt.Errorf("node %s is not ready", t)
		}
	}
	return nil
}

// replicaCountOf reads the cluster's configured replica count from a node, so
// the result file records what was actually running rather than what the
// operator believed.
func replicaCountOf(ctx context.Context, client *http.Client, targets []string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targets[0]+"/stats", http.NoBody)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var body struct {
		Cluster struct {
			ReplicaCount int `json:"replica_count"`
		} `json:"cluster"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil {
		return 0
	}
	return body.Cluster.ReplicaCount
}

// hostInfo runs scripts/hostinfo.sh and embeds its output.
//
// The script path is resolved against the repository root rather than the
// working directory: `go test` runs with the package directory as its cwd, so a
// relative path silently fails there and the result file loses the one thing
// that makes its numbers interpretable. The same mistake, in the other
// direction, once wrote benchmark results into internal/<pkg>/bench/results/.
func hostInfo() map[string]any {
	script := "scripts/hostinfo.sh"
	if root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		script = filepath.Join(strings.TrimSpace(string(root)), script)
	}
	out, err := exec.Command("bash", script, "-").Output()
	if err != nil {
		return map[string]any{"error": script + " failed: " + err.Error()}
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return map[string]any{"error": "hostinfo output is not JSON"}
	}
	return m
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func writeReport(opts options, rep Report) error {
	name := fmt.Sprintf("bench-%s.json", rep.GeneratedAt.Format("20060102-150405"))
	if rep.Smoke {
		name = "bench-smoke.json"
	}
	path := filepath.Join(opts.out, name)

	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)

	if !rep.Smoke {
		latest := filepath.Join(opts.out, "bench-latest.json")
		if err := os.WriteFile(latest, append(b, '\n'), 0o644); err != nil {
			return fmt.Errorf("write latest: %w", err)
		}
	}
	printSummary(rep)
	return nil
}

func printSummary(rep Report) {
	fmt.Println()
	fmt.Println("=== kilnbench summary (median of each point) ===")
	for _, sc := range rep.Scenarios {
		fmt.Printf("\n%s  (%s objects)\n", sc.Name, humanBytes(int64(toInt(sc.Corpus["object_size_bytes"]))))
		fmt.Printf("  %-6s %10s %10s %9s %9s %9s %9s\n", "conc", "ops/s", "MiB/s", "p50 ms", "p95 ms", "p99 ms", "err")
		for _, p := range sc.Points {
			fmt.Printf("  %-6d %10.0f %10.1f %9.2f %9.2f %9.2f %9.4f\n",
				p.Concurrency, p.Median.OpsPerSec, p.Median.MiBPerSec,
				p.Median.Latency.P50Ms, p.Median.Latency.P95Ms, p.Median.Latency.P99Ms,
				p.Median.ErrorRate)
		}
	}
	fmt.Println()
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
