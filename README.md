# KilnCache

[![CI](https://github.com/Lexieli666/kilncache/actions/workflows/ci.yml/badge.svg)](https://github.com/Lexieli666/kilncache/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A distributed, disk-backed remote build cache that speaks Bazel's HTTP remote
cache protocol. Three nodes, content-addressed objects, rendezvous placement,
two-copy replication before acknowledgement, bounded disk with access-aware
eviction, and automatic repair of missing replicas.

**v1.0** — a working cache with its numbers, its failure modes and its bugs all
written down.

## The problem

A Bazel build without a remote cache rebuilds everything that any developer or
CI job has already built. With one, a clean checkout downloads the artifacts
instead of recompiling them. The open question for anyone running that cache is
what happens when a node dies mid-build, when the disk fills, or when a network
hiccup truncates a transfer — because a build cache that occasionally returns
the wrong bytes is far worse than no cache at all.

KilnCache exists to answer those questions with tests and measurements rather
than assurances.

## What it does

- **Content-addressed storage.** `PUT /cas/<sha256>` streams to a temp file
  while hashing, rejects on digest mismatch, and publishes with
  fsync + rename + directory fsync. A partial upload is never visible.
- **Rendezvous placement.** A pure function maps each key to an ordered list of
  nodes. Every node computes it identically with no coordination.
- **Two-copy replication.** A PUT is acknowledged only after the object exists
  on two nodes. If the second copy cannot be written, the client gets a 503, not
  a false success.
- **Replica fallback.** A GET tries local, then primary, then replica. Losing a
  node costs latency, not correctness.
- **Bounded disk.** A per-node quota with high and low water marks, driven by a
  SQLite index of size and last access, evicting by a size-weighted heap.
- **Self-repair.** A bounded background worker pool finds retained objects whose
  second copy is missing and recreates it.

## What it deliberately does not do

- Remote **execution** (REAPI). Cache only.
- **Consensus**, leader election, or dynamic membership — see
  [ADR-0002](docs/adr/0002-no-consensus.md) for why immutable content-addressed
  objects make this defensible, and for the one place it is genuinely weaker
  (action-cache entries, which are not content-addressed).
- **Authentication, TLS, or multi-tenancy.** The trust boundary is the cluster.
  Do not expose a node to an untrusted network.
- **Cross-region replication**, Kubernetes manifests, or a UI.

## Architecture

```
                    Bazel (--remote_cache=http://node-a:8080)
                                     |
            +------------------------+------------------------+
            |                        |                        |
         Node A                   Node B                   Node C
      HTTP front door          HTTP front door          HTTP front door
            |                        |                        |
       rendezvous(key) -> {primary, replica}   (same pure function everywhere)
            |                        |                        |
     local CAS on disk         local CAS on disk        local CAS on disk
     sharded dirs + SQLite     sharded dirs + SQLite    sharded dirs + SQLite
     quota + eviction          quota + eviction         quota + eviction
            \______________ background repair audit ______________/
```

Any node is a valid front door. It serves the object if it holds it, otherwise
it forwards one hop to a node that should. A forwarding header makes loops
impossible.

Details: [architecture](docs/architecture.md) · [failure model](docs/failure-model.md) ·
[protocol](docs/protocol.md) · [ADRs](docs/adr/)

## Quick start

```bash
# Build and run the three-node cluster
make compose-up
curl -s localhost:8080/healthz | jq .
curl -s localhost:8081/readyz  | jq .

# Point Bazel at it
bazel build //... --remote_cache=http://localhost:8080

# Tear down, including the cache volumes
make compose-down
```

Single node, no Docker:

```bash
make build
./bin/kilncache --node-name=solo --listen=:8080 --data-dir=/tmp/kiln --dev
```

`make help` lists every target. `make tools` reports which external tools are
installed and what stops working without each one.

### Try it against a real Bazel build

```bash
make build
scripts/gen-bazel-fixture.sh                       # 300 C++ targets, deterministic
scripts/localnode.sh start solo 18080 --max-bytes=40GiB

cd fixtures/bazel-cpp
bazel build //... --config=remote --remote_cache=http://127.0.0.1:18080   # populate
bazel clean --expunge
bazel build //... --config=remote --remote_cache=http://127.0.0.1:18080   # served from cache
```

The second build reports `600 remote cache hit` and runs no compiler processes.
`scripts/bazel-outputs-digest.sh` hashes every artifact so the two builds can be
compared byte for byte.

## Development

```bash
make lint        # gofmt -s, go vet, golangci-lint
make test        # unit and property tests
make test-race   # the falsifier for every concurrency claim here
make cover       # coverage profile and total
make integration # real servers, real disks; Docker tiers skip loudly if absent
```

### Building on a Windows drive under WSL2

This checkout lives on `/mnt/d`, a 9p mount. Three consequences, all handled:

- **Bazel** keeps its output base on the Linux filesystem. Use
  `bazel --bazelrc=.bazelrc.wsl ...`, or `--output_user_root=$HOME/.cache/bazel`.
- **Cache data** lives in Docker *named volumes*, never bind mounts. fsync and
  rename on 9p do not have the semantics the store depends on, and the
  throughput is a fraction of native.
- **File mode bits** are not meaningful on 9p; nothing in the code or tests
  asserts on them.

`scripts/device-baseline.sh` refuses to publish a baseline taken on 9p unless
explicitly overridden, for the same reason.

## What it does, measured

The headline experiment, on a 32-core Linux host with three nodes as containers
and the client co-located
([raw](bench/results/2026-09-20-yutongzhao/bazel-bench.json)):

| A clean build of 300 C++ targets | Median of 5 runs |
|---|---:|
| Compiled locally, no remote cache | 25.3 s |
| Rebuilt from KilnCache after `bazel clean --expunge` | **5.6 s** |
| **Median reduction** | **77.8%** |
| Actions served from the cache | 600 of 600 |
| Build outputs byte-for-byte identical to the cold build | **all 320** |

That last row is the one that matters. A cache that makes a build fast and wrong
is worse than no cache.

Behaviour under failure, from a 10-minute chaos run with 10 node stops
([raw](bench/results/2026-09-20-yutongzhao/chaos-latest.json)):

| | |
|---|---|
| Corrupted reads | **0** out of 304,325 reads verified |
| Convergence back to two replicas | 217 s |
| Client error rate while degraded | 22.6% — the faults landed |

Every read was checked against a digest the chaos runner computed itself, never
one the cluster reported.

Placement, over 50,000 keys
([raw](bench/results/2026-09-20-yutongzhao/placement.json)):

| | Measured | |
|---|---:|---|
| Load imbalance, 3 nodes (max/mean) | 1.0127 | bound: 1.10 |
| Keys moved when a 4th node joins | 25.26% | theory: 25% |

Throughput, latency, the device baseline and the network-fault run are in
[BENCHMARKS.md](BENCHMARKS.md), which is generated from raw JSON and never
hand-edited. Every target from the specification, met or not, is mapped to its
measured value in [bench/RESULTS_SUMMARY.md](bench/RESULTS_SUMMARY.md).

### Before quoting any of this

The client shares a host with the servers, so there is no network in the
synthetic throughput numbers. Reads are warm: dropping the page cache needs
root, which this host does not grant. The host is a WSL2 virtual machine, so
reads look better than a physical NVMe device would and `fsync`-bound writes
look worse. All of that is recorded in every result file and spelled out in
[docs/performance-methodology.md](docs/performance-methodology.md).

The Bazel figure is the one that survives scrutiny best: it measures an
end-to-end task rather than a microbenchmark, and the identical-outputs check
makes "faster" mean something.

## Measurement

Rule 1 of [CONTRIBUTING.md](CONTRIBUTING.md): no number appears in this
repository without a committed command and committed raw output under
`bench/results/<ISO-date>-<hostname>/`. `scripts/check-numbers.sh` enforces it in
CI — a numeric claim with no matching result file fails the build.

```bash
make device-baseline   # fio floor for this filesystem
make bench             # the full matrix, 5 runs per point
make benchmarks        # regenerate BENCHMARKS.md from the JSON
make bazel-bench       # the end-to-end experiment
make chaos             # 10-minute failure run
make check-numbers     # the rule, enforced
```

## Documentation

| | |
|---|---|
| [Architecture](docs/architecture.md) | What the pieces are and how a request moves through them. |
| [Failure model](docs/failure-model.md) | What is guaranteed, what is not, and the test for each. |
| [Protocol](docs/protocol.md) | What Bazel expects, and what each status code costs a build. |
| [Performance methodology](docs/performance-methodology.md) | How the numbers were made, and where they are honest about their limits. |
| [Performance notes](docs/perf-notes.md) | Two profiling wins, and one that bought nothing. |
| [Runbook](docs/runbook.md) | For whoever is holding this at 3 a.m. |
| [Bugs](docs/bugs.md) | Twelve bugs the tests, chaos runs and CI caught, and what each one taught. |
| [Testing](docs/testing.md) | Six kinds of test and what each answers. |
| [ADRs](docs/adr/) | Why no consensus, why rendezvous hashing, why synchronous replication, why this eviction policy. |

## License

MIT. See [LICENSE](LICENSE).
