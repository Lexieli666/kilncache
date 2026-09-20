# Results against the targets

Every target from the project specification, the measured value, and the raw
file it came from. Targets that were not met are marked and explained; so is
every target that was met for a reason the number alone would not reveal.

Host: Intel Core i9-14900KF, 32 logical cores, 31.2 GiB RAM, Linux 6.6.114.1
WSL2, ext4 on a virtual disk. Three nodes as Docker containers on that one host,
replica count 2. Full record in
[`results/2026-09-20-yutongzhao/hostinfo.json`](results/2026-09-20-yutongzhao/hostinfo.json).

**Read the host line before the numbers.** This is a virtual machine whose disk
sits behind the Windows host's page cache, and the benchmark client shares the
host with the servers. Read figures are better than a physical NVMe device would
give; `fsync`-bound writes are worse; there is no network latency at all. The
[device baseline](results/2026-09-20-yutongzhao/device-baseline.json) is
measured on the same filesystem and is what makes the comparisons meaningful
even though the absolute numbers do not transfer.

---

## Performance

Source: [`bench-latest.json`](results/2026-09-20-yutongzhao/bench-latest.json), [`bazel-bench.json`](results/2026-09-20-yutongzhao/bazel-bench.json)

| Metric | Target | Measured | Verdict |
|---|---|---|---|
| 64 KiB cache-hit throughput, 64 concurrent clients | 1,500–3,000 req/s | **46,058 req/s** | Far above — see note 1 |
| 64 KiB cache-hit p99 | 10–25 ms | **5.13 ms** at 64 concurrent; 16.23 ms at 256 | Better than target |
| 8 MiB GET aggregate throughput | 600–1,200 MiB/s | **14,637 MiB/s** | Far above — see note 2 |
| Replicated PUT aggregate throughput | 300–600 MiB/s | **1,701 MiB/s** at 64 concurrent | Above target |
| Bazel clean-rebuild median time reduction, 300 C++ targets | 55–75% | **77.8%** | Slightly above the range |

**Note 1 — why 64 KiB throughput is 15x the target.** The target assumed a
physical network between client and servers. Here they share a host, so a
request is a loopback round trip with no NIC, no switch and no serialisation
delay. The number is real and reproducible, and it is a bound on what the server
can do, not a prediction of what a remote Bazel client would see. A client one
network hop away would be limited by that hop long before it reached this.

**Note 2 — why 8 MiB throughput is 12x the target.** The 960 MiB corpus fits
comfortably in 31 GiB of RAM, so every read is served from the page cache. The
result file says so in its `page_cache_note` field. Dropping the page cache
requires root, which this host does not grant, so a cold large-object number
could not be measured. The device baseline is the honest floor for that case.

**Note 3 — why the Bazel reduction is the number to trust.** Unlike the
synthetic figures, the end-to-end measurement includes everything: Bazel's own
loading and analysis, action execution, and the real transfer of 1,194 MiB of
outputs. It is 25.3 s down to 5.6 s, median of five runs each, with
`bazel clean --expunge` before every one. What remains in the cached build is
Bazel's own work plus the download, which is the floor a remote cache cannot go
below.

---

## Correctness and distribution

Source: [`placement.json`](results/2026-09-20-yutongzhao/placement.json), [`chaos-latest.json`](results/2026-09-20-yutongzhao/chaos-latest.json), [`phase2-docker-node-loss.json`](results/2026-09-20-yutongzhao/phase2-docker-node-loss.json)

| Metric | Target | Measured | Verdict |
|---|---|---|---|
| Placement imbalance max/mean over 50,000 keys | ≤ 1.10 | **1.0127** (3 nodes), 1.0208 (8 nodes) | Met |
| Keys moved when adding node 4 of 4 | 20–35% (theory 25%) | **25.26%** | Met |
| Corrupted successful reads in fault tests | 0 | **0 of 304,325 reads verified** | Met |
| Chaos run | ≥ 10 minutes, converge to 2 replicas | **601 s, 10 node stops, converged in 217 s** | Met |

The corrupted-read count is published with its sample size because the count
alone would be meaningless. Every read was checked against a SHA-256 the chaos
runner computed itself from bytes it generated — never against anything the
cluster reported. A verifier that asked the server what the digest should be
would pass for exactly the bug it exists to catch.

The chaos run's client error rate was 22.6%, almost all connection-refused
against whichever node was stopped at the time. That is the evidence the faults
landed. A chaos run with a zero error rate would mean nothing was broken, and
the tool reports that as INCONCLUSIVE rather than as a pass.

---

## Engineering

Source: [`phase4-verify.md`](results/2026-09-20-yutongzhao/phase4-verify.md), [`phase5-verify.md`](results/2026-09-20-yutongzhao/phase5-verify.md)

| Metric | Target | Measured | Verdict |
|---|---|---|---|
| Tests | 200–300 Go tests | **251 unit/property + 63 integration = 314** | Above the range |
| Line coverage | ≥ 80% | **82.5%** | Met |
| Profiling wins documented | ≥ 2 with before/after | **2, plus one honest null result** | Met |
| Repo size | 8–12k Go LOC | **9,529 non-test + 9,538 test = 19,067 lines of Go** | See the caveat |

**The LOC caveat.** Exactly half the Go in this repository is test code: 9,529
lines of implementation against 9,538 lines of tests. The implementation alone
sits inside the 8–12k target; the total is above it. Which number the target
meant is ambiguous, so both are given rather than whichever flatters.

Counted with `find . -name '*.go' | xargs wc -l`, which includes comments and
blank lines. This code is heavily commented by design — the repository's third
rule is that every claim has a falsifier, and a good deal of the commentary is
naming which test that is.

Coverage is measured with `-coverpkg` across the whole module with the
integration tag on (`make cover-all`). Plain `go test -cover ./...` scores
`internal/node` at zero, because its coverage comes from another package's
tests, and would understate what is actually exercised.

---

## The two profiling wins

Source: [`eviction-throughput.json`](results/2026-09-20-yutongzhao/eviction-throughput.json), [`bench-before-pooled-buffer-fix.json`](results/2026-09-20-yutongzhao/bench-before-pooled-buffer-fix.json), [`bench-latest.json`](results/2026-09-20-yutongzhao/bench-latest.json)

| Change | Before | After | Factor |
|---|---:|---:|---:|
| Eviction: dropped the directory `fsync` on delete, batched index removals | 190 objects/s | 28,338 objects/s | **149x** |
| Reads: stopped `io.CopyBuffer` discarding the pooled buffer | 32,532 ops/s | 46,058 ops/s | **1.42x**, p99 −31.8% |

Both are written up in [`docs/perf-notes.md`](../docs/perf-notes.md), along with
a third finding that was real, was fixed, and changed nothing measurable — which
is the more instructive of the three, because the first attempt at it produced a
table of ±8% noise that could easily have been reported as a small win.

---

## What was not measured

- **Cold-cache large-object reads.** Needs root to drop the page cache.
- **A real network between client and servers.** They share a host. One scenario
  was re-run under `tc netem` with added latency and loss
  ([`netem/bench-latest.json`](results/2026-09-20-yutongzhao/netem/bench-latest.json)):
  throughput collapses, latency follows the round trip, and the error count
  stays at **zero**. It was slow, not wrong.
- **Comparison against any other cache.** A fair one would need the same
  workload, hardware and tuning effort on both sides.
- **Durability under real power loss.** The tests verify that `fsync`, `rename`
  and the directory `fsync` happen in the right order. Verifying the guarantee
  itself would need hardware that can be cut mid-write.

---

## Resume bullets, with the measured figures substituted

The specification's section 7 bullets, rewritten with what was actually
measured. Every figure below comes from
[`bench-latest.json`](results/2026-09-20-yutongzhao/bench-latest.json),
[`bazel-bench.json`](results/2026-09-20-yutongzhao/bazel-bench.json),
[`chaos-latest.json`](results/2026-09-20-yutongzhao/chaos-latest.json),
[`placement.json`](results/2026-09-20-yutongzhao/placement.json) or
[`phase4-verify.md`](results/2026-09-20-yutongzhao/phase4-verify.md).

> Built a distributed Bazel remote build cache in Go: content-addressed storage,
> rendezvous hashing, two-copy replication, disk quotas with access-aware
> eviction, and automatic replica repair across three nodes.

> Cut median clean-rebuild time of a 300-target C++ workload **78%** (25.3 s →
> 5.6 s, median of 5 runs each) by serving 600 of 600 actions from the cache
> with byte-identical outputs; sustained **46,000 cache hits/s at 5.1 ms p99**
> and **14.6 GiB/s** large-object reads on a 32-core Linux host with client and
> servers co-located.
> Sources: [`bazel-bench.json`](results/2026-09-20-yutongzhao/bazel-bench.json),
> [`bench-latest.json`](results/2026-09-20-yutongzhao/bench-latest.json)

> Verified **zero corrupted reads across 304,325 verified reads** during a
> 10-minute chaos run with 10 node stops, converging back to two replicas in
> 217 s; property tests over 50,000 keys measured placement imbalance at
> **1.0127** and **25.3%** key movement when a fourth node joins.
> Sources: [`chaos-latest.json`](results/2026-09-20-yutongzhao/chaos-latest.json),
> [`placement.json`](results/2026-09-20-yutongzhao/placement.json)

> **314 tests** under the race detector at **82.5%** coverage on every push;
> raw benchmark data, CPU and heap profiles committed with the results, and a
> CI check that fails the build if any published number lacks one.
> Source: [`phase4-verify.md`](results/2026-09-20-yutongzhao/phase4-verify.md)

Two honesty notes for anyone using these bullets:

1. The throughput figures were measured with the client on the same host as the
   servers. Say so, or say "on loopback". Quoting the hit rate without that
   context overstates what a remote client would see.
2. The Bazel reduction is the figure that survives scrutiny best, because it
   measures an end-to-end task rather than a microbenchmark, and because the
   identical-outputs check makes "faster" mean something.
