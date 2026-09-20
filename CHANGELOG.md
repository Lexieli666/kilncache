# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] — 2026-09-20

First release. A three-node, disk-backed remote build cache speaking Bazel's
HTTP cache protocol.

### Added

**The cache protocol**

- `GET`/`HEAD`/`PUT` on `/cas/{sha256}` and `/ac/{sha256}`, plus the `/cache/`
  prefix some Bazel deployments use.
- Streaming through a fixed 256 KiB buffer, so resident memory is a function of
  concurrency rather than object size.
- SHA-256 verification on ingest for CAS objects, and optionally on every read.
- Durable publication: `fsync(file)` → `rename` → `fsync(directory)`. An
  interrupted upload is never visible, and a crash's leftovers are swept before
  the node reports ready.
- Status codes chosen around Bazel's behaviour of disabling the remote cache for
  a whole invocation on any non-404 failure: a malformed key is a miss, not an
  error.

**The cluster**

- Rendezvous (highest-random-weight) placement, computed identically on every
  node from a static peer list, with no coordination.
- Synchronous two-copy replication: a PUT returns 2xx only when the required
  number of nodes hold the object, and 503 otherwise.
- Read fallback: local, then each holder in preference order, then the rest.
  Every holder unreachable is a 503, never a 404 — a partition must not look
  like a cold cache.
- Forwarding bounded by a hop state machine rather than a counter, so two nodes
  with stale peer lists cannot loop.

**Storage management**

- SQLite metadata index in WAL mode, pure-Go driver, with separate read and
  write connection pools.
- Per-node quota with high and low water marks, and size-weighted access-aware
  eviction driven by a min-heap over the coldest candidates.
- Startup reconciliation that treats the disk as authoritative, so the index
  cannot claim an object the node has lost.
- A bounded repair worker pool that recreates missing replicas, and defers to
  eviction when a holder is already over its high-water mark.

**Operability**

- Prometheus metrics and a human-readable `/stats`, both reading the same
  counters so they cannot disagree.
- Separate `/healthz` and `/readyz`, so a draining node is not restarted
  mid-drain.
- pprof behind an explicit `--dev` flag.
- Structured JSON logs.
- `distroless/static:nonroot` image with a self-probing health check.

**Tooling and evidence**

- `cmd/bench`: benchmark driver producing JSON that records the host, the git
  commit, the corpus, and whether the cache was warm.
- `cmd/chaos`: fault-injecting traffic driver with independent SHA-256
  bookkeeping.
- `scripts/netem.sh`: `tc netem` injection inside each container's namespace.
- `scripts/bazel-bench.sh`: the end-to-end experiment.
- `scripts/check-numbers.sh`: fails CI if any published number lacks a committed
  raw result.
- `BENCHMARKS.md` is generated from JSON and never hand-edited.

### Measured

Full mapping to the project's targets is in
[`bench/RESULTS_SUMMARY.md`](bench/RESULTS_SUMMARY.md). Headlines, on a 32-core
WSL2 host with client and servers co-located:

- Bazel clean-rebuild of a 300-target C++ workspace: **25.3 s → 5.6 s, a 77.8%
  median reduction**, with all 320 build outputs byte-for-byte identical to the
  locally compiled ones
  ([bazel-bench.json](bench/results/2026-09-20-yutongzhao/bazel-bench.json)).
- 64 KiB cache hits: **46,058 req/s at 5.13 ms p99** at 64 concurrent
  ([bench-latest.json](bench/results/2026-09-20-yutongzhao/bench-latest.json)).
- Zero corrupted reads across **304,325 verified reads** during a 10-minute
  chaos run with 10 node stops; converged back to two replicas in 217 s
  ([chaos-latest.json](bench/results/2026-09-20-yutongzhao/chaos-latest.json)).
- Placement imbalance **1.0127** over 50,000 keys; **25.26%** of keys move when
  a fourth node joins
  ([placement.json](bench/results/2026-09-20-yutongzhao/placement.json)).

### Known limitations

Documented in full in [`docs/failure-model.md`](docs/failure-model.md):

- Action-cache entries are not content-addressed, so concurrent writes to one
  action key are last-writer-wins and two holders can disagree about the winner.
- Losing both holders of an object loses it. Two copies of three nodes is not
  durability.
- Membership is static; adding a node means editing the peer list and
  restarting.
- No authentication, authorisation or TLS. The trust boundary is the cluster.
- A peer that acknowledges a write it did not persist is invisible at write
  time; the repair auditor finds it on a later pass.
- Under sustained quota pressure an object can sit at one replica, because a
  full node declines repair writes rather than fighting its own evictor.

### Not implemented (and not planned for 1.x)

Remote execution, gRPC REAPI, consensus, dynamic membership, cross-region
replication, Kubernetes manifests, a UI.
