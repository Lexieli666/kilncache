// Command kilnbench measures KilnCache and writes results nobody has to take
// on trust.
//
// Every run emits JSON containing the host it ran on, the git commit, the
// corpus, the concurrency, whether the cache was warm, whether the client
// shared a host with the servers, and whether netem was active. BENCHMARKS.md
// is generated from those files and never hand-edited, so a number in the
// documentation can always be traced to the run that produced it.
//
// What this deliberately does not do: pick the best run. Every repetition is
// recorded, and the summary reports the median with the full spread beside it.
// A benchmark that reports its best result is an advertisement.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "kilnbench: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	targets     []string
	out         string
	runs        int
	duration    time.Duration
	warmup      time.Duration
	concurrency []int
	scenarios   []string
	smoke       bool
	verify      bool
	seed        int64
	netem       string
	colocated   bool
	profileDir  string
	label       string

	// ramBytes is the host's total memory, used to say whether a corpus could
	// have been served from the page cache.
	ramBytes int64

	// peakMemory is filled by the memory watcher during a scenario.
	peakMemory []NodeSample
}

// replicaFactorBytes returns the host RAM, for the page-cache note.
func (o *options) replicaFactorBytes() int64 { return o.ramBytes }

func run(args []string) error {
	fs := flag.NewFlagSet("kilnbench", flag.ContinueOnError)
	var (
		targetsRaw = fs.String("targets", "http://localhost:8080,http://localhost:8081,http://localhost:8082",
			"comma-separated node URLs to drive")
		out         = fs.String("out", "", "directory for result files (required)")
		runs        = fs.Int("runs", 5, "repetitions per scenario and concurrency; the spec asks for at least 5")
		duration    = fs.Duration("duration", 20*time.Second, "measured duration per run")
		warmup      = fs.Duration("warmup", 5*time.Second, "unmeasured warmup per run")
		concurrency = fs.String("concurrency", "4,16,64,256", "comma-separated concurrency levels")
		scenarios   = fs.String("scenarios", "", "comma-separated scenario names; empty means all")
		smoke       = fs.Bool("smoke", false, "one short run per scenario at low concurrency, for checking the harness")
		verify      = fs.Bool("verify", false, "verify the SHA-256 of every response body (costs throughput; measures correctness)")
		seed        = fs.Int64("seed", 20260920, "corpus seed")
		netem       = fs.String("netem", "", "description of the tc netem conditions in effect, recorded in the result")
		colocated   = fs.Bool("colocated", true, "whether the client shares a host with the servers")
		profileDir  = fs.String("profile-dir", "", "collect CPU and heap profiles from each node into this directory")
		label       = fs.String("label", "", "free-text label recorded in the result")
		renderFrom  = fs.String("render", "", "regenerate the Markdown summary from the results in this directory and exit")
		renderTo    = fs.String("render-out", "BENCHMARKS.md", "where to write the generated Markdown")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *renderFrom != "" {
		return render(*renderFrom, *renderTo)
	}
	if *out == "" {
		return errors.New("-out is required: a benchmark whose raw output is not written is not a measurement")
	}

	opts := options{
		out: *out, runs: *runs, duration: *duration, warmup: *warmup,
		smoke: *smoke, verify: *verify, seed: *seed, netem: *netem,
		colocated: *colocated, profileDir: *profileDir, label: *label,
	}
	for _, t := range strings.Split(*targetsRaw, ",") {
		if t = strings.TrimRight(strings.TrimSpace(t), "/"); t != "" {
			opts.targets = append(opts.targets, t)
		}
	}
	if len(opts.targets) == 0 {
		return errors.New("no targets")
	}
	for _, c := range strings.Split(*concurrency, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		n, err := strconv.Atoi(c)
		if err != nil || n < 1 {
			return fmt.Errorf("bad concurrency %q", c)
		}
		opts.concurrency = append(opts.concurrency, n)
	}
	if *scenarios != "" {
		for _, s := range strings.Split(*scenarios, ",") {
			if s = strings.TrimSpace(s); s != "" {
				opts.scenarios = append(opts.scenarios, s)
			}
		}
	}

	if opts.smoke {
		// A smoke run exists to check the harness, not the system. It says so
		// in the result file so nobody mistakes one for a measurement.
		opts.runs = 1
		opts.duration = 3 * time.Second
		opts.warmup = 1 * time.Second
		opts.concurrency = []int{8}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return execute(ctx, opts)
}

func newClient(concurrency int) *http.Client {
	// The connection pool must not be the bottleneck. Go's default of two idle
	// connections per host would make a 256-way benchmark measure TCP handshake
	// latency and report it as cache latency.
	idle := concurrency * 2
	if idle < 64 {
		idle = 64
	}
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			MaxIdleConns:          idle * 4,
			MaxIdleConnsPerHost:   idle,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       120 * time.Second,
			DisableCompression:    true,
			ForceAttemptHTTP2:     false,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}
