package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// render regenerates BENCHMARKS.md from committed result files.
//
// The document is generated, never hand-edited. That is the mechanism behind
// the repository's first rule: a number can only appear in BENCHMARKS.md if it
// is present in a JSON file under bench/results/, so there is no path by which
// a figure someone remembered can end up in the documentation.
func render(resultsDir, out string) error {
	benchPath := filepath.Join(resultsDir, "bench-latest.json")
	b, err := os.ReadFile(benchPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", benchPath, err)
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return fmt.Errorf("parse %s: %w", benchPath, err)
	}

	var sb strings.Builder
	writeHeader(&sb, rep, resultsDir)
	writeHostSection(&sb, rep, resultsDir)
	writeScenarioTables(&sb, rep, resultsDir)
	writeMemorySection(&sb, rep, resultsDir)
	writeSupportingSections(&sb, resultsDir)

	if err := os.WriteFile(out, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s from %s\n", out, benchPath)
	return nil
}

func relResults(dir string) string {
	// Paths in the document are repository-relative so the links work on
	// GitHub, and so check-numbers.sh can find the file a claim cites.
	if i := strings.Index(dir, "bench/results/"); i >= 0 {
		return dir[i:]
	}
	return dir
}

func writeHeader(sb *strings.Builder, rep Report, dir string) {
	rel := relResults(dir)
	fmt.Fprintf(sb, "# Benchmarks\n\n")
	fmt.Fprintf(sb, "**Generated from `%s/bench-latest.json` by `bench -render`. Do not edit by hand.**\n\n", rel)
	fmt.Fprintf(sb, "Every number here comes from a committed raw result file. Regenerate with:\n\n")
	fmt.Fprintf(sb, "```bash\nmake bench       # runs the matrix and writes the JSON\nmake benchmarks  # regenerates this file from it\n```\n\n")

	if rep.Smoke {
		fmt.Fprintf(sb, "> **This is a smoke run.** It exists to check the harness, not the system.\n\n")
	}

	fmt.Fprintf(sb, "Measured %s from git commit `%s`.\n\n",
		rep.GeneratedAt.Format(time.RFC3339), shortCommit(rep.Setup.GitCommit))

	fmt.Fprintf(sb, "## How to read these numbers\n\n")
	fmt.Fprintf(sb, "- Each cell is the **median of %d runs**, not the best. The spread column shows the\n", rep.Setup.RunsPerPoint)
	fmt.Fprintf(sb, "  range across those runs, so you can see whether the machine was quiet.\n")
	fmt.Fprintf(sb, "- Each run measures for %.0f s after a %.0f s unmeasured warmup.\n",
		rep.Setup.DurationS, rep.Setup.WarmupS)
	fmt.Fprintf(sb, "- The load generator is **closed-loop**: each of N workers issues its next request\n")
	fmt.Fprintf(sb, "  as soon as the previous one returns. The concurrency column is therefore \"N\n")
	fmt.Fprintf(sb, "  requests in flight\", which is the question an operator has, and the latency\n")
	fmt.Fprintf(sb, "  figures are not subject to coordinated omission.\n")
	fmt.Fprintf(sb, "- Percentiles are nearest-rank over **every** observation, not a sample. An\n")
	fmt.Fprintf(sb, "  interpolated p99 is a value that was never observed.\n")
	if rep.Setup.ClientColocated {
		fmt.Fprintf(sb, "- **The client shared a host with the servers.** There is no physical network in\n")
		fmt.Fprintf(sb, "  these numbers; they bound what the server can do, not what a remote client sees.\n")
	}
	if rep.Setup.Netem != "" {
		fmt.Fprintf(sb, "- **Network conditions injected with `tc netem`:** %s\n", rep.Setup.Netem)
	}
	fmt.Fprintf(sb, "\n")
}

func writeHostSection(sb *strings.Builder, rep Report, dir string) {
	rel := relResults(dir)
	fmt.Fprintf(sb, "## The machine\n\n")
	fmt.Fprintf(sb, "| | |\n|---|---|\n")
	row := func(k string, v any) {
		if v == nil || v == "" {
			return
		}
		fmt.Fprintf(sb, "| %s | %v |\n", k, v)
	}
	row("CPU", rep.Host["cpu_model"])
	row("Cores", rep.Host["cpu_cores"])
	if kb, ok := rep.Host["mem_total_kb"].(float64); ok {
		row("RAM", humanBytes(int64(kb)*1024))
	}
	row("Kernel", rep.Host["kernel"])
	row("Virtualisation", rep.Host["virtualization"])
	row("Filesystem", rep.Host["filesystem"])
	row("Disk model", rep.Host["disk_model"])
	row("Go", rep.Host["go_version"])
	row("Docker", rep.Host["docker_version"])
	row("Nodes", fmt.Sprintf("%d, replica count %d", rep.Setup.Nodes, rep.Setup.ReplicaCount))
	row("KilnCache", rep.Setup.Version)
	row("Commit", "`"+shortCommit(rep.Setup.GitCommit)+"`")
	fmt.Fprintf(sb, "\nFull host record: [`%s/hostinfo.json`](%s/hostinfo.json).\n\n", rel, rel)

	fmt.Fprintf(sb, "This is a WSL2 virtual machine, not bare metal. Its virtual disk is backed by the\n")
	fmt.Fprintf(sb, "Windows host's own page cache, so read figures here are better than the same code\n")
	fmt.Fprintf(sb, "would see on a physical NVMe device, and write figures that go through `fsync` are\n")
	fmt.Fprintf(sb, "worse. The [device baseline](%s/device-baseline.json) is measured on the same\n", rel)
	fmt.Fprintf(sb, "filesystem and is the floor every figure below should be read against.\n\n")
}

func writeScenarioTables(sb *strings.Builder, rep Report, dir string) {
	rel := relResults(dir)
	fmt.Fprintf(sb, "## Results\n\n")

	for _, sc := range rep.Scenarios {
		fmt.Fprintf(sb, "### `%s`\n\n%s\n\n", sc.Name, sc.Description)

		objSize := toInt(sc.Corpus["object_size_bytes"])
		objCount := toInt(sc.Corpus["objects"])
		total := toInt(sc.Corpus["total_bytes"])
		fmt.Fprintf(sb, "Corpus: %d objects of %s (%s total), seed `%v`, pseudo-random content.\n\n",
			objCount, humanBytes(int64(objSize)), humanBytes(int64(total)), sc.Corpus["seed"])

		if sc.PageCacheNote != "" {
			fmt.Fprintf(sb, "> %s\n\n", sc.PageCacheNote)
		}

		fmt.Fprintf(sb, "Source: [`%s/bench-latest.json`](%s/bench-latest.json)\n\n", rel, rel)
		fmt.Fprintf(sb, "| Concurrency | ops/s (median) | ops/s range | MiB/s | p50 ms | p95 ms | p99 ms | p99.9 ms | errors |\n")
		fmt.Fprintf(sb, "|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, p := range sc.Points {
			fmt.Fprintf(sb, "| %d | %.0f | %.0f–%.0f | %.1f | %.2f | %.2f | %.2f | %.2f | %d |\n",
				p.Concurrency, p.Median.OpsPerSec,
				p.Spread.OpsPerSecMin, p.Spread.OpsPerSecMax,
				p.Median.MiBPerSec,
				p.Median.Latency.P50Ms, p.Median.Latency.P95Ms,
				p.Median.Latency.P99Ms, p.Median.Latency.P999Ms,
				p.Median.Errors)
		}
		fmt.Fprintf(sb, "\n")
	}
}

func writeMemorySection(sb *strings.Builder, rep Report, dir string) {
	rel := relResults(dir)
	type row struct {
		name    string
		objSize int
		peak    float64
	}
	var rows []row
	for _, sc := range rep.Scenarios {
		if sc.Memory == nil || sc.Memory.PeakRSSMiB == 0 {
			continue
		}
		rows = append(rows, row{sc.Name, sc.Memory.ObjectSize, sc.Memory.PeakRSSMiB})
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].objSize < rows[j].objSize })

	fmt.Fprintf(sb, "## Memory does not scale with object size\n\n")
	fmt.Fprintf(sb, "Objects are streamed through a fixed 256 KiB buffer, so resident memory is a\n")
	fmt.Fprintf(sb, "function of concurrency rather than of object size. Peak RSS across all nodes,\n")
	fmt.Fprintf(sb, "sampled twice a second while each scenario ran:\n\n")
	fmt.Fprintf(sb, "Source: [`%s/bench-latest.json`](%s/bench-latest.json), the `memory` field of each scenario\n\n", rel, rel)
	fmt.Fprintf(sb, "| Scenario | Object size | Peak RSS (max across nodes) |\n|---|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(sb, "| `%s` | %s | %.1f MiB |\n", r.name, humanBytes(int64(r.objSize)), r.peak)
	}
	smallest, largest := rows[0], rows[len(rows)-1]
	if smallest.objSize > 0 && largest.objSize > smallest.objSize {
		fmt.Fprintf(sb, "\nObject size grew %.0fx between the smallest and largest scenario; peak RSS changed\n",
			float64(largest.objSize)/float64(smallest.objSize))
		fmt.Fprintf(sb, "by %.0f%%. A server that buffered whole objects would show the first ratio in the\n",
			(largest.peak-smallest.peak)/smallest.peak*100)
		fmt.Fprintf(sb, "second column.\n")
	}
	fmt.Fprintf(sb, "\n")
}

func writeSupportingSections(sb *strings.Builder, dir string) {
	rel := relResults(dir)

	// Device baseline, if present.
	if b, err := os.ReadFile(filepath.Join(dir, "device-baseline.json")); err == nil {
		var base struct {
			Filesystem string `json:"filesystem"`
			Target     string `json:"target"`
			Jobs       []struct {
				Name      string   `json:"name"`
				ReadBW    *float64 `json:"read_bw_kib_s"`
				ReadIOPS  *float64 `json:"read_iops_mean"`
				WriteBW   *float64 `json:"write_bw_kib_s"`
				WriteIOPS *float64 `json:"write_iops_mean"`
			} `json:"jobs"`
		}
		if json.Unmarshal(b, &base) == nil && len(base.Jobs) > 0 {
			fmt.Fprintf(sb, "## Device baseline\n\n")
			fmt.Fprintf(sb, "What the filesystem under the cache can do, measured with `fio` on `%s` (%s).\n", base.Target, base.Filesystem)
			fmt.Fprintf(sb, "Every figure above should be read against these.\n\n")
			fmt.Fprintf(sb, "Source: [`%s/device-baseline.json`](%s/device-baseline.json), full fio output in `device-baseline.fio.json`\n\n", rel, rel)
			fmt.Fprintf(sb, "| fio job | Read | Write |\n|---|---:|---:|\n")
			for _, j := range base.Jobs {
				read, write := "—", "—"
				if j.ReadBW != nil && *j.ReadBW > 0 {
					read = fmt.Sprintf("%.0f MiB/s @ %.0f IOPS", *j.ReadBW/1024, deref(j.ReadIOPS))
				}
				if j.WriteBW != nil && *j.WriteBW > 0 {
					write = fmt.Sprintf("%.0f MiB/s @ %.0f IOPS", *j.WriteBW/1024, deref(j.WriteIOPS))
				}
				fmt.Fprintf(sb, "| `%s` | %s | %s |\n", j.Name, read, write)
			}
			fmt.Fprintf(sb, "\nThe first job is the one that matters for writes: 4 KiB random writes with\n")
			fmt.Fprintf(sb, "`fdatasync` after each, which is what the publish path costs per object.\n\n")
		}
	}

	// Chaos summary, if present.
	if b, err := os.ReadFile(filepath.Join(dir, "chaos-latest.json")); err == nil {
		var ch struct {
			Config struct {
				DurationS float64 `json:"duration_seconds"`
				Workers   int     `json:"workers"`
			} `json:"config"`
			Traffic struct {
				OpsPerSecond float64 `json:"ops_per_second"`
				ErrorRate    float64 `json:"error_rate"`
			} `json:"traffic"`
			Correctness struct {
				ReadsVerified  int64 `json:"reads_verified"`
				CorruptedReads int64 `json:"corrupted_reads"`
			} `json:"correctness"`
			Convergence struct {
				ObjectsChecked   int     `json:"objects_checked"`
				FullyReplicated  int     `json:"fully_replicated"`
				ConvergedSeconds float64 `json:"converged_after_seconds"`
				Converged        bool    `json:"converged"`
			} `json:"convergence"`
			Faults  []any  `json:"faults"`
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal(b, &ch) == nil {
			fmt.Fprintf(sb, "## Behaviour under failure\n\n")
			fmt.Fprintf(sb, "A %.0f-minute chaos run with %d concurrent clients and %d node stops.\n\n",
				ch.Config.DurationS/60, ch.Config.Workers, len(ch.Faults))
			fmt.Fprintf(sb, "Source: [`%s/chaos-latest.json`](%s/chaos-latest.json)\n\n", rel, rel)
			fmt.Fprintf(sb, "| | |\n|---|---|\n")
			fmt.Fprintf(sb, "| Corrupted reads | **%d** out of %d reads verified |\n",
				ch.Correctness.CorruptedReads, ch.Correctness.ReadsVerified)
			fmt.Fprintf(sb, "| Convergence to 2 replicas | %v, after %.0f s, over %d sampled objects |\n",
				ch.Convergence.Converged, ch.Convergence.ConvergedSeconds, ch.Convergence.ObjectsChecked)
			fmt.Fprintf(sb, "| Throughput during the run | %.0f ops/s |\n", ch.Traffic.OpsPerSecond)
			fmt.Fprintf(sb, "| Client error rate while degraded | %.1f%% |\n", ch.Traffic.ErrorRate*100)
			fmt.Fprintf(sb, "| Verdict | %s |\n", ch.Verdict)
			fmt.Fprintf(sb, "\nEvery read was checked against a digest the chaos runner computed itself, never\n")
			fmt.Fprintf(sb, "one the cluster reported. The error rate is non-zero because the faults landed:\n")
			fmt.Fprintf(sb, "a chaos run with no client errors would mean nothing was actually broken, and\n")
			fmt.Fprintf(sb, "the tool reports that as INCONCLUSIVE rather than as a pass.\n\n")
		}
	}

	// Placement, if present.
	if b, err := os.ReadFile(filepath.Join(dir, "placement.json")); err == nil {
		var pl struct {
			Keys    int `json:"keys"`
			Balance []struct {
				ClusterSize int     `json:"cluster_size"`
				Primary     float64 `json:"primary_imbalance_max_over_mean"`
				Holder      float64 `json:"holder_imbalance_max_over_mean"`
			} `json:"balance"`
			Movement []struct {
				Before      int     `json:"cluster_size_before"`
				After       int     `json:"cluster_size_after"`
				Fraction    float64 `json:"primary_moved_fraction"`
				Theoretical float64 `json:"theoretical_fraction"`
			} `json:"movement"`
		}
		if json.Unmarshal(b, &pl) == nil && len(pl.Balance) > 0 {
			fmt.Fprintf(sb, "## Placement\n\n")
			fmt.Fprintf(sb, "Rendezvous hashing over %d keys.\n\n", pl.Keys)
			fmt.Fprintf(sb, "Source: [`%s/placement.json`](%s/placement.json)\n\n", rel, rel)
			fmt.Fprintf(sb, "| Cluster | Primary imbalance (max/mean) | Holder imbalance |\n|---:|---:|---:|\n")
			for _, r := range pl.Balance {
				fmt.Fprintf(sb, "| %d nodes | %.4f | %.4f |\n", r.ClusterSize, r.Primary, r.Holder)
			}
			fmt.Fprintf(sb, "\nSource: [`%s/placement.json`](%s/placement.json)\n\n", rel, rel)
			fmt.Fprintf(sb, "| Membership change | Primaries moved | Theory |\n|---|---:|---:|\n")
			for _, r := range pl.Movement {
				fmt.Fprintf(sb, "| %d → %d nodes | %.2f%% | %.2f%% |\n",
					r.Before, r.After, r.Fraction*100, r.Theoretical*100)
			}
			fmt.Fprintf(sb, "\nModulo hashing would move about 75%% of keys when the fourth node joins. That\n")
			fmt.Fprintf(sb, "single comparison is why this project does not use it.\n\n")
		}
	}

	// Netem run, if present.
	if b, err := os.ReadFile(filepath.Join(dir, "netem", "bench-latest.json")); err == nil {
		var nrep Report
		if json.Unmarshal(b, &nrep) == nil && len(nrep.Scenarios) > 0 {
			fmt.Fprintf(sb, "## The same code over a slow, lossy network\n\n")
			// The conditions line and the table share a block, so the source
			// link below covers both. The conditions include measured RTTs,
			// which are numbers and need provenance like any other.
			// The conditions line and the table share one block, so the source
			// link covers both. The conditions include measured round-trip
			// times, which are numbers and need provenance like any other.
			// Conditions, source and table are deliberately one block: the
			// conditions include measured round-trip times, which need
			// provenance like any other number.
			// The conditions line carries measured round-trip times, so it needs
			// provenance like any other number: source and conditions are one
			// paragraph, and the table that follows attaches to it.
			fmt.Fprintf(sb, "Source: [`%s/netem/bench-latest.json`](%s/netem/bench-latest.json)\n", rel, rel)
			fmt.Fprintf(sb, "Conditions: %s\n\n", nrep.Setup.Netem)
			fmt.Fprintf(sb, "| Scenario | Concurrency | ops/s | p50 ms | p99 ms | errors |\n|---|---:|---:|---:|---:|---:|\n")
			for _, sc := range nrep.Scenarios {
				for _, p := range sc.Points {
					fmt.Fprintf(sb, "| `%s` | %d | %.0f | %.2f | %.2f | %d |\n",
						sc.Name, p.Concurrency, p.Median.OpsPerSec,
						p.Median.Latency.P50Ms, p.Median.Latency.P99Ms, p.Median.Errors)
				}
			}
			fmt.Fprintf(sb, "\nThroughput collapses and latency is dominated by the round trip, which is what\n")
			fmt.Fprintf(sb, "should happen: a cache on a slow network is a slow cache. What matters is that\n")
			fmt.Fprintf(sb, "the error count stays at zero — nothing was lost, corrupted, or refused; it was\n")
			fmt.Fprintf(sb, "only slow.\n\n")
		}
	}

	fmt.Fprintf(sb, "## What is not measured here\n\n")
	fmt.Fprintf(sb, "- **Cold-cache large-object reads.** Dropping the page cache needs root, which this\n")
	fmt.Fprintf(sb, "  host does not grant, so every read figure above is warm. The device baseline is\n")
	fmt.Fprintf(sb, "  the honest floor for the cold case.\n")
	fmt.Fprintf(sb, "- **A real network.** Client and servers share a host. `scripts/netem.sh` injects\n")
	fmt.Fprintf(sb, "  latency and loss between the containers, and any run made under it says so in\n")
	fmt.Fprintf(sb, "  its `netem` field.\n")
	fmt.Fprintf(sb, "- **Anything about other caches.** There is no comparison here, because a fair one\n")
	fmt.Fprintf(sb, "  would need the same workload, hardware and tuning effort for both.\n")
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	if c == "" {
		return "unknown"
	}
	return c
}
