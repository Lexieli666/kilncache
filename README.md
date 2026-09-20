# KilnCache

[![CI](https://github.com/Lexieli666/kilncache/actions/workflows/ci.yml/badge.svg)](https://github.com/Lexieli666/kilncache/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A distributed, disk-backed remote build cache that speaks Bazel's HTTP remote
cache protocol. Three nodes, content-addressed objects, rendezvous placement,
two-copy replication before acknowledgement, bounded disk with access-aware
eviction, and automatic repair of missing replicas.

> **Status: Phase 2 complete.** Bazel builds the 300-target C++ fixture against
> a node, `bazel clean --expunge` wipes the local cache, and the rebuild is
> served entirely from KilnCache with byte-identical outputs
> ([raw evidence](bench/results/2026-09-20-yutongzhao/phase1-bazel-e2e.md)).
> Three nodes place objects by rendezvous hashing and replicate synchronously;
> stopping a container and reading its objects through another node returns the
> right bytes
> ([raw evidence](bench/results/2026-09-20-yutongzhao/phase2-docker-node-loss.json)).
> Quota, eviction and repair land in Phase 3; every section below that describes
> unimplemented behaviour says so. No performance number appears in this file
> until it has a raw result file under `bench/results/`.

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

Details: [docs/protocol.md](docs/protocol.md) ·
[docs/architecture.md](docs/architecture.md) *(Phase 5)* ·
[docs/failure-model.md](docs/failure-model.md) *(Phase 5)* ·
[docs/testing.md](docs/testing.md) · [docs/adr/](docs/adr/)

## Quick start

```bash
# Build and run the three-node cluster
make compose-up
curl -s localhost:8080/healthz | jq .
curl -s localhost:8081/readyz  | jq .

# Point Bazel at it (Phase 1 onward)
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

## What has been measured so far

Placement, over 50,000 keys
([`placement.json`](bench/results/2026-09-20-yutongzhao/placement.json),
regenerate with `make placement-report`):

| Property | Measured | Bound |
|---|---|---|
| Load imbalance, 3 nodes (max/mean) | 1.0127 | ≤ 1.10 |
| Load imbalance, 8 nodes (max/mean) | 1.0208 | ≤ 1.10 |
| Keys moved when a 4th node joins | 25.26% | theory 25% |
| Keys moved when a node is removed | exactly that node's keys | — |

Failure behaviour, three real containers
([`phase2-docker-node-loss.json`](bench/results/2026-09-20-yutongzhao/phase2-docker-node-loss.json)):
1,000 objects written with two copies each, one container stopped, then **671
objects that the stopped container held were read through another node and all
671 matched their SHA-256 — 0 corrupted**. After restart the container served
all 671 of its own copies from its named volume.

Throughput and latency are Phase 4; there are no numbers for them here yet
because there are no raw results for them yet.

## Measurement

Rule 1 of [CONTRIBUTING.md](CONTRIBUTING.md): no number appears in this
repository without a committed command and committed raw output under
`bench/results/<ISO-date>-<hostname>/`.

The device baseline is the floor every later number is read against — a cache
serving 90% of what the disk can do is a good cache; one serving 9% has a bug.
The current baseline is in
[`bench/results/2026-09-20-yutongzhao/device-baseline.json`](bench/results/2026-09-20-yutongzhao/device-baseline.json),
with the host it was taken on recorded beside it.

Performance targets for later phases are stated in
[PROGRESS.md](PROGRESS.md) as targets, and will be replaced by measured values
in `BENCHMARKS.md` — generated from raw JSON, never hand-edited.

## License

MIT. See [LICENSE](LICENSE).
