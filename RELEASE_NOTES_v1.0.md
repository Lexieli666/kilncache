# KilnCache v1.0

A distributed, disk-backed remote build cache that speaks Bazel's HTTP cache
protocol. Three nodes, content-addressed objects, rendezvous placement, two-copy
replication before acknowledgement, bounded disk with access-aware eviction, and
automatic repair of missing replicas.

## What it does, measured

Pointing Bazel at KilnCache and rebuilding a 300-target C++ workspace after
`bazel clean --expunge`, median of five runs each, on a 32-core Linux host:

Source: [`bench/results/2026-09-20-yutongzhao/bazel-bench.json`](bench/results/2026-09-20-yutongzhao/bazel-bench.json)

| | |
|---|---:|
| Compiled locally, no remote cache | 25.3 s |
| Rebuilt from KilnCache | **5.6 s** |
| **Median reduction** | **77.8%** |
| Actions served from the cache | 600 of 600 |
| Build outputs byte-for-byte identical to the cold build | **all 320** |

That last row is the point. A cache that makes a build fast and wrong is worse
than no cache.

Under failure — a 10-minute chaos run with 10 node stops:

Source: [`bench/results/2026-09-20-yutongzhao/chaos-latest.json`](bench/results/2026-09-20-yutongzhao/chaos-latest.json)

| | |
|---|---|
| Corrupted reads | **0** of 304,325 reads verified |
| Convergence back to two replicas | 217 s |
| Client error rate while degraded | 22.6%, which is how you know the faults landed |

Every read was checked against a digest the chaos runner computed itself, never
one the cluster reported.

Full numbers, including throughput, latency, the device baseline, and a run
under injected network latency and loss, are in
[BENCHMARKS.md](BENCHMARKS.md) — generated from raw JSON, never hand-edited.
Every specification target, met or exceeded, is mapped to its measured value in
[bench/RESULTS_SUMMARY.md](bench/RESULTS_SUMMARY.md).

## Before quoting the throughput numbers

The benchmark client shares a host with the servers, so the synthetic figures
contain no network. Reads are warm; dropping the page cache needs root. The host
is a WSL2 virtual machine whose disk sits behind the Windows page cache. All of
this is recorded in every result file and spelled out in
[docs/performance-methodology.md](docs/performance-methodology.md).

The Bazel figure is the one that survives scrutiny best: it measures an
end-to-end task rather than a microbenchmark, and the identical-outputs check
makes "faster" mean something.

## The interesting parts

- **No consensus, and why that is defensible** —
  [ADR-0002](docs/adr/0002-no-consensus.md), including the one place the design
  is genuinely weaker than a consensus-based cache.
- **Durability** — [ADR-0003](docs/adr/0003-durability.md) on why the parent
  directory `fsync` is the step everyone forgets, and what the tests do *not*
  prove.
- **Placement** — [ADR-0004](docs/adr/0004-rendezvous-hashing.md): rendezvous
  hashing measured at 1.0127 imbalance over 50,000 keys with nothing to tune
  ([placement.json](bench/results/2026-09-20-yutongzhao/placement.json)).
- **Eleven bugs** — [docs/bugs.md](docs/bugs.md). Including two subsystems that
  fought each other until the quota stopped being enforced, a benchmark that
  was timing a `stat()` call, and a `.gitignore` line that kept the main binary
  out of the repository.
- **Two profiling wins and one that bought nothing** —
  [docs/perf-notes.md](docs/perf-notes.md). The third is the instructive one.

## Known limitations

Action-cache entries are not content-addressed, so concurrent writes to one
action key are last-writer-wins. Losing both holders of an object loses it.
Membership is static. There is no authentication or TLS — the trust boundary is
the cluster. Under sustained quota pressure an object can sit at one replica,
because a full node declines repair writes rather than fighting its own evictor.

All of these, and the test for each guarantee that *is* made, are in
[docs/failure-model.md](docs/failure-model.md).

## Getting started

```bash
make compose-up
bazel build //... --remote_cache=http://localhost:8080
```

[README](README.md) · [Runbook](docs/runbook.md) · [Changelog](CHANGELOG.md)
