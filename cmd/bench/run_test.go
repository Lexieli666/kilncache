//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/node"
)

// TestBenchmarkAgainstRealNodes runs the whole benchmark driver against a real
// three-node cluster in this process.
//
// It is the falsifier for the harness itself. The numbers this tool produces go
// straight into BENCHMARKS.md, so a bug here does not report a wrong number
// about the system — it *is* the wrong number. Two bugs of exactly that kind
// have already been found: PUT scenarios that re-uploaded corpus objects and so
// measured the already-stored fast path, and an error rate that was really
// requests cancelled by the end of the measurement window.
func TestBenchmarkAgainstRealNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the benchmark control run in short mode")
	}

	names := []string{"node-a", "node-b", "node-c"}
	ports := make([]int, len(names))
	for i := range ports {
		ports[i] = reserveBenchPort(t)
	}
	peers := make([]config.Peer, len(names))
	for i := range names {
		peers[i] = config.Peer{Name: names[i], URL: fmt.Sprintf("http://127.0.0.1:%d", ports[i])}
	}

	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := range names {
		cfg := config.Defaults()
		cfg.NodeName = names[i]
		cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", ports[i])
		cfg.DataDir = filepath.Join(root, names[i])
		cfg.Peers = peers
		cfg.ReplicaCount = 2
		cfg.MaxBytes = 512 << 20
		cfg.DevMode = true // the driver collects profiles from /debug/pprof
		cfg.ShutdownTimeout = 2 * time.Second

		nd, err := node.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("start %s: %v", names[i], err)
		}
		go func() { _ = nd.Run(ctx) }()
		t.Cleanup(func() { _ = nd.Close() })
	}

	targets := make([]string, 0, len(names))
	for i := range names {
		targets = append(targets, fmt.Sprintf("http://127.0.0.1:%d", ports[i]))
	}

	out := t.TempDir()
	profiles := t.TempDir()
	err := run([]string{
		"-targets", strings.Join(targets, ","),
		"-out", out,
		"-profile-dir", profiles,
		"-runs", "3",
		"-duration", "2s",
		"-warmup", "1s",
		"-concurrency", "4,16",
		"-scenarios", "get-64k,put-replicated,mixed-80-20",
		"-label", "control run",
	})
	if err != nil {
		t.Fatalf("benchmark run: %v", err)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var reportPath string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bench-2") {
			reportPath = filepath.Join(out, e.Name())
		}
	}
	if reportPath == "" {
		t.Fatalf("no timestamped report written; got %v", entries)
	}

	b, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	if len(rep.Scenarios) != 3 {
		t.Fatalf("ran %d scenarios, want 3", len(rep.Scenarios))
	}
	if rep.Setup.ReplicaCount != 2 {
		t.Errorf("replica count recorded as %d; the report must describe what actually ran", rep.Setup.ReplicaCount)
	}
	if rep.Setup.GitCommit == "" || rep.Setup.GitCommit == "unknown" {
		t.Error("no git commit recorded; the result cannot be traced to code")
	}
	if rep.Host == nil || rep.Host["cpu_model"] == nil {
		t.Error("no host information recorded; the numbers are not interpretable")
	}

	for _, sc := range rep.Scenarios {
		if len(sc.Points) != 2 {
			t.Errorf("%s: %d concurrency points, want 2", sc.Name, len(sc.Points))
		}
		if sc.PageCacheNote == "" {
			t.Errorf("%s: no page-cache note", sc.Name)
		}
		if sc.Memory == nil {
			t.Errorf("%s: no memory observation", sc.Name)
		}
		for _, p := range sc.Points {
			if len(p.Runs) != 3 {
				t.Errorf("%s c=%d: %d runs, want 3", sc.Name, p.Concurrency, len(p.Runs))
			}
			if p.Median.Operations == 0 {
				t.Errorf("%s c=%d: zero operations", sc.Name, p.Concurrency)
			}
			// A healthy in-process cluster must produce no errors at all. This
			// is where a regression that made the harness count cancelled
			// requests as failures would show up.
			if p.Median.Errors != 0 {
				t.Errorf("%s c=%d: %d errors against a healthy cluster: %v",
					sc.Name, p.Concurrency, p.Median.Errors, p.Median.ErrorSamples)
			}
			if p.Median.Latency.Count == 0 {
				t.Errorf("%s c=%d: no latency observations", sc.Name, p.Concurrency)
			}
			if p.Median.Latency.P99Ms < p.Median.Latency.P50Ms {
				t.Errorf("%s c=%d: p99 %v below p50 %v", sc.Name, p.Concurrency,
					p.Median.Latency.P99Ms, p.Median.Latency.P50Ms)
			}
		}
	}

	// A PUT scenario must actually store new objects. If it re-uploaded corpus
	// objects it would hit the already-stored fast path and report a number an
	// order of magnitude too high — which an earlier version did.
	for _, sc := range rep.Scenarios {
		if sc.Name != "put-replicated" {
			continue
		}
		for _, p := range sc.Points {
			if p.Median.Bytes == 0 {
				t.Errorf("put scenario moved no bytes at c=%d", p.Concurrency)
			}
		}
	}

	// Profiles must exist and be non-trivial.
	profs, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatal(err)
	}
	var cpu, heap int
	for _, p := range profs {
		info, _ := p.Info()
		if info != nil && info.Size() < 64 {
			t.Errorf("profile %s is %d bytes; it is empty", p.Name(), info.Size())
		}
		switch {
		case strings.HasSuffix(p.Name(), "-cpu.pprof"):
			cpu++
		case strings.HasSuffix(p.Name(), "-heap.pprof"):
			heap++
		}
	}
	if cpu == 0 || heap == 0 {
		t.Errorf("collected %d CPU and %d heap profiles; want both", cpu, heap)
	}

	// And the report must render into a document.
	if err := os.WriteFile(filepath.Join(out, "bench-latest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(out, "BENCHMARKS.md")
	if err := render(out, md); err != nil {
		t.Fatalf("render: %v", err)
	}
	doc, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "get-64k") || !strings.Contains(string(doc), "Source:") {
		t.Error("the rendered document is missing a scenario or its source link")
	}

	t.Logf("control run: %d scenarios, %d profiles, report %s",
		len(rep.Scenarios), len(profs), filepath.Base(reportPath))
}

func reserveBenchPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		l.Close()
		t.Fatal("not a tcp address")
	}
	port := addr.Port
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}
