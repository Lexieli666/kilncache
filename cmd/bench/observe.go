package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NodeSample is one node's resident memory and goroutine count at a moment.
type NodeSample struct {
	Node       string  `json:"node"`
	RSSBytes   float64 `json:"rss_bytes"`
	HeapBytes  float64 `json:"heap_alloc_bytes"`
	Goroutines float64 `json:"goroutines"`
	OpenFDs    float64 `json:"open_fds"`
}

// MemoryObservation records resident memory across a scenario.
//
// This is the evidence for the claim that memory does not scale with object
// size. The store streams through a fixed 256 KiB buffer, so serving 8 MiB
// objects should cost the same resident memory as serving 64 KiB ones; if it
// ever stops being true, this is the number that says so.
type MemoryObservation struct {
	Scenario    string       `json:"scenario"`
	ObjectSize  int          `json:"object_size_bytes"`
	Concurrency int          `json:"concurrency"`
	Before      []NodeSample `json:"before"`
	Peak        []NodeSample `json:"peak"`
	After       []NodeSample `json:"after"`
	PeakRSSMiB  float64      `json:"peak_rss_mib_max_across_nodes"`
}

// sampleNodes reads the runtime gauges from each node's /metrics.
func sampleNodes(ctx context.Context, client *http.Client, targets []string) []NodeSample {
	out := make([]NodeSample, 0, len(targets))
	for _, t := range targets {
		s := NodeSample{Node: t}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t+"/metrics", http.NoBody)
		if err != nil {
			out = append(out, s)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			out = append(out, s)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "#") || line == "" {
				continue
			}
			name, value, ok := splitMetric(line)
			if !ok {
				continue
			}
			switch name {
			case "process_resident_memory_bytes":
				s.RSSBytes = value
			case "go_memstats_heap_alloc_bytes":
				s.HeapBytes = value
			case "go_goroutines":
				s.Goroutines = value
			case "process_open_fds":
				s.OpenFDs = value
			}
		}
		out = append(out, s)
	}
	return out
}

func splitMetric(line string) (string, float64, bool) {
	sp := strings.LastIndex(line, " ")
	if sp < 0 {
		return "", 0, false
	}
	name := line[:sp]
	if i := strings.IndexByte(name, '{'); i >= 0 {
		name = name[:i]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
	if err != nil {
		return "", 0, false
	}
	return name, v, true
}

// watchMemory samples every node while a run is in progress and keeps the peak.
func watchMemory(ctx context.Context, client *http.Client, targets []string) (stop func() []NodeSample) {
	var mu sync.Mutex
	peak := make([]NodeSample, len(targets))
	for i, t := range targets {
		peak[i].Node = t
	}
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			now := sampleNodes(ctx, client, targets)
			mu.Lock()
			for i := range now {
				if i < len(peak) && now[i].RSSBytes > peak[i].RSSBytes {
					peak[i] = now[i]
				}
			}
			mu.Unlock()
		}
	}()

	return func() []NodeSample {
		<-done
		mu.Lock()
		defer mu.Unlock()
		out := make([]NodeSample, len(peak))
		copy(out, peak)
		return out
	}
}

// collectProfiles pulls a CPU profile and a heap profile from every node.
//
// The CPU profile is taken *during* load, which is the only time it says
// anything: profiling an idle process produces a flamegraph of the scheduler.
// The caller must therefore start the load first and call this while it runs.
func collectProfiles(ctx context.Context, client *http.Client, targets []string, dir, label string, seconds int) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(targets)*2)

	for i, t := range targets {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			node := fmt.Sprintf("node%d", i)

			cpuPath := filepath.Join(dir, fmt.Sprintf("%s-%s-cpu.pprof", label, node))
			if err := fetchTo(ctx, client, fmt.Sprintf("%s/debug/pprof/profile?seconds=%d", t, seconds), cpuPath); err != nil {
				errCh <- fmt.Errorf("cpu profile from %s: %w", t, err)
			}
			heapPath := filepath.Join(dir, fmt.Sprintf("%s-%s-heap.pprof", label, node))
			if err := fetchTo(ctx, client, t+"/debug/pprof/heap", heapPath); err != nil {
				errCh <- fmt.Errorf("heap profile from %s: %w", t, err)
			}
		}(i, t)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	fmt.Fprintf(os.Stderr, "   collected CPU and heap profiles into %s (label %s)\n", dir, label)
	return nil
}

func fetchTo(ctx context.Context, client *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}
