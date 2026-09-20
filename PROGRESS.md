# PROGRESS

Living status file for the unattended build of KilnCache. Re-read this after any
context loss and resume from "Next".

- **Repository**: `Lexieli666/kilncache`
- **Working directory**: `/mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache`
- **Spec**: `../../Project Specs/Project_1_KilnCache_Spec.md`
- **Current phase**: **Complete.** v1.0 tagged and released.

## Environment

Host: `yutongzhao`, Intel Core i9-14900KF, 32 logical cores, 31 GiB RAM,
Linux 6.6.114.1-microsoft-standard-WSL2, WSL2.

| Tool | Version | Notes |
|---|---|---|
| go | go1.23.12 linux/amd64 | Installed user-local to `~/.local/go` (no sudo on this host). |
| golangci-lint | 1.64.8 | Binary release into `~/.local/bin`. |
| docker | server 29.1.3 | Docker Desktop via WSL integration. |
| docker compose | 2.32.4 | Plugin was **missing**; installed to `~/.docker/cli-plugins/docker-compose`. |
| bazel | bazelisk 1.25.0 → Bazel 7.4.1 | Pinned by `.bazelversion`. Bazelisk reports the latest release when run outside a workspace. |
| fio | 3.38 | **Built from source** into `~/.local` — no sudo, so no apt package. Built without libaio; the baseline script probes for `io_uring` instead. |
| jq | 1.7.1 | Binary release into `~/.local/bin`. |
| tc | iproute2 6.19.0 | Present. netem inside containers needs `NET_ADMIN`. |
| gh | 2.46.0 | Authenticated as `Lexieli666` (active account). |

### Constraints recorded up front

1. **No sudo on this host.** Everything above was installed into `$HOME`.
   Anything that genuinely requires root is noted where it appears.
2. **Checkout is on `/mnt/d`, a 9p mount.** Bazel output base is forced to
   `$HOME/.cache/bazel` (`.bazelversion` + `.bazelrc.wsl`); cache data uses
   Docker named volumes, never bind mounts; nothing asserts on file mode bits;
   `git config core.filemode` is already `false`.
3. **Benchmarks must not measure 9p.** `scripts/device-baseline.sh` refuses to
   publish a baseline from a 9p path unless explicitly overridden. Node data
   directories for local benchmarking go under `$HOME`, not the checkout.
4. **Git identity** for this repository is set locally to
   `Lexie Li <lexieli@engineering.upenn.edu>` to match the spec's owner and the
   target GitHub account, leaving the machine's global identity untouched.

## Phase 0 — Kickoff / scaffold

**Done**

- Repository layout, `go.mod` (`github.com/Lexieli666/kilncache`, Go 1.23).
- `internal/config`: flags + `KILNCACHE_*` env with documented precedence,
  binary/decimal size parsing, full validation. Tests cover every rejection.
- `internal/logging`: JSON `log/slog` with the node ID attached at the root.
- `internal/httpapi`: `/healthz` and `/readyz` as genuinely separate signals,
  access log, panic recovery, node-stamping header, route table, graceful
  shutdown that marks the node not-ready before draining.
- `cmd/kilncache`: config load, signal handling, ordered startup/shutdown, and a
  `-healthcheck` self-probe so the distroless image needs no shell or curl.
- Makefile (build, fmt, lint, test, test-race, cover, integration, bench-smoke,
  chaos, compose-*, device-baseline, hostinfo, tools), `.golangci.yml`
  (errcheck, govet, staticcheck, gocritic, bodyclose, unused, ineffassign,
  misspell, unconvert, nolintlint).
- Distroless `Dockerfile`, three-node `docker-compose.yml` on named volumes.
- GitHub Actions: lint, test, test-race, coverage, integration on ubuntu-24.04
  with module caching.
- `scripts/`: `hostinfo.sh`, `device-baseline.sh`, `toolcheck.sh`, `lib.sh`.
- Docs: `README.md`, `CONTRIBUTING.md` (the three rules), `docs/testing.md`,
  ADR-0001 (record decisions), ADR-0002 (no consensus, and what it gives up).
- Device baseline measured and committed under
  `bench/results/2026-09-20-yutongzhao/`.

**Failed / worked around**

- `docker compose` was not installed; installed the plugin user-local.
- `go`, `fio`, `jq`, `golangci-lint`, `bazel` were all absent and apt needs
  sudo; all installed into `$HOME` instead.
- fio has no libaio (no `libaio-dev`, no sudo). The baseline script now probes
  `fio --enghelp` and uses `io_uring`; the engine used is recorded in the result
  file so the number is interpretable.

**Next**: Phase 1 (done, below).

## Phase 1 — correct single-node cache

**Done**

- `internal/storage`: sharded object tree (`<ns>/<xx>/<yy>/<key>`, two hex-byte
  levels), streaming writes through a fixed 256 KiB pooled buffer, SHA-256
  verification on ingest, publication by fsync → rename → directory fsync,
  crash sweep of partial uploads at startup, idempotent CAS writes, `Walk` for
  later reconciliation, and always-on counters.
- `internal/httpapi`: `GET`/`HEAD`/`PUT`/`POST` on `/cas/{sha256}` and
  `/ac/{sha256}`, plus the `/cache/` prefix. Cache routes are matched ahead of
  `ServeMux` because its path-cleaning redirects would turn a garbled key into a
  301, and Bazel disables the remote cache for a whole invocation on any
  non-404 failure.
- `internal/node`: one definition of what a node is, shared by `cmd/kilncache`
  and the integration tests, so the tests exercise the arrangement that ships.
- `fixtures/bazel-cpp`: generator producing 300 targets (280 `cc_library` in 8
  layers + 20 `cc_binary`) with deterministic sources and enough compile ballast
  that the build measures compilation rather than Bazel's startup.
- Docs: ADR-0003 (durability, including what the tests do *not* prove),
  `docs/protocol.md` (what Bazel actually expects, and what each status code
  costs a build).
- `scripts/localnode.sh` (PID-file node manager), `scripts/bazel-outputs-digest.sh`,
  `scripts/check-numbers.sh` (now enforced in CI).

**Verified** — raw output in `bench/results/2026-09-20-yutongzhao/`:

- `phase1-verify.md`: `go test`, `go test -race`, `go vet`, `golangci-lint`, the
  11-test integration suite (also under `-race`), 84 unit/property tests,
  70.4% total line coverage, and `check-numbers.sh`.
- `phase1-bazel-e2e.md`: a real Bazel build of the fixture against KilnCache,
  `bazel clean --expunge`, and a rebuild served entirely from the cache —
  600/600 remote cache hits, zero compiler processes, and all 320 build outputs
  byte-for-byte identical to the locally-compiled ones.

**Failed / worked around**

- The Bazel fixture as first generated built in ~7 s, most of it Bazel's own
  loading and analysis. Measuring a cache against that would have measured
  Bazel's startup, so the generator now emits template-instantiation ballast per
  translation unit; the cold build is ~24 s with object files of a realistic
  size.
- `textwrap.dedent` mangled the generated `BUILD.bazel` (an interpolated
  dependency list brings its own shallower indent, so the common prefix shrinks
  and everything else comes out under-indented). Replaced with explicit string
  construction.
- `pkill -f kilncache` kills the shell that runs it, because `-f` matches the
  caller's own command line. Replaced by `scripts/localnode.sh` with PID files.
- The first integration test asserted synchronously that a severed upload had
  been cleaned up. That was a test bug, not a product bug: when the client hangs
  up, the server does not learn until its next read fails, so the assertion was
  racing the server. It now waits with a deadline, and the comment says why.

**Next**: Phase 2 (done, below).

## Phase 2 — three-node placement and replication

**Done**

- `internal/cluster/ring.go`: rendezvous (highest-random-weight) placement.
  Member seeds from SHA-256 of the name, key seed taken straight from the
  already-uniform digest, SplitMix64 finalizer, ties broken by name, members
  sorted so peer-list order cannot change placement.
- `internal/protocol`: the node-to-node contract — headers, the `Hop` state
  machine that bounds forwarding, `Backend`, `ObjectReader`, and the
  insufficient-replicas sentinel. It exists so the dependency between
  `internal/httpapi` and `internal/cluster` points one way.
- `internal/cluster/client.go`: peer HTTP client with a real connection pool
  (Go's default of 2 idle conns/host would make every replicated PUT pay a
  handshake), per-hop deadlines that cover establishing a response rather than
  draining a body, and errors that distinguish a dead peer from a rejected
  object.
- `internal/cluster/coordinator.go`: the `Backend`. Local write first, then
  concurrent fan-out to the other holders; a non-owner front door proxies and
  stores nothing; reads try local, then holders, then the rest; a partition is
  reported as 503, never as a cache miss.
- Tests: a fault-injecting fake peer (down / rejecting / slow / *acknowledges
  but does not persist*), 5 rapid property tests, and the three-node integration
  tier plus a Docker tier that stops and restarts real containers.

**Measured** (raw files in `bench/results/2026-09-20-yutongzhao/`):

| Claim | Measured | Target | Source |
|---|---|---|---|
| Placement imbalance, 3 nodes, 50,000 keys | 1.0127 | ≤ 1.10 | `placement.json` |
| Placement imbalance, 8 nodes, 50,000 keys | 1.0208 | ≤ 1.10 | `placement.json` |
| Keys moved adding node 4 of 4 | 25.26% | 20–35% (theory 25%) | `placement.json` |
| Corrupted reads, Docker node-loss run | 0 of 671 checked | 0 | `phase2-docker-node-loss.json` |
| Writes involving a dead holder that returned 503 | 79 of 79 | all | `phase2-verify.md` |

**Failed / worked around**

- The Docker tier immediately found a real bug: the distroless `nonroot` user
  could not create the store directory, because `/var/lib/kilncache` existed
  only as a mount point and Docker initialises a fresh named volume owned by
  root when the path is absent from the image. Fixed by creating the directory
  in the build stage and `COPY --chown=65532:65532`. Phase 0's compose check had
  passed on a node that could not have served a single object, because Phase 0
  had no storage layer. Recorded as bug 5 in `docs/bugs.md`.
- Two coordinator fallback tests were written against keys the test node
  happened to hold, so the fallback path never ran. Fixed to select keys the
  front door provably does not hold.
- `go test -cover ./...` scored `internal/node` at 0% because its coverage comes
  from another package's tests. Added `make cover-all` (`-coverpkg` across the
  module with the integration tag on), which is now the published figure; the
  narrow `cover` target stays for the edit loop.

**Next**: Phase 3 (done, below).

## Phase 3 — quota, eviction, restart, repair, chaos

**Done**

- `internal/storage/index.go`: SQLite metadata index in WAL mode, pure-Go driver
  so the runtime image stays distroless/static. **Separate read and write
  pools** (one writer, four readers) and **keyset-paginated scans**, both forced
  by a real starvation bug found in chaos.
- Access times are batched, not written inline: a cache hit must not cost a
  database write. 100 touches of one key collapse to one update.
- `internal/storage/evict.go`: quota with high/low water marks, size-weighted
  access-aware scoring, a min-heap over a batch of the coldest candidates, and a
  background sweeper woken by writes — never inline on the write path.
- `Store.Reconcile`: the disk is authoritative and the index is derived, so an
  index row with no file cannot survive a restart. This makes "never marks a
  missing file valid" structural rather than a behaviour to maintain.
- `internal/repair`: bounded worker pool, audit with a budget and a resumable
  cursor, backpressure that stops a pass rather than skipping work, and a
  `HopRepair` role a full node can decline.
- `internal/metrics` + `/stats` + `/metrics`: Prometheus collectors that read
  the counters the subsystems already keep, so `/stats` and `/metrics` cannot
  disagree. (Phase 4 work, pulled forward: the chaos runner needs node counters
  to tell eviction apart from data loss.)
- `cmd/chaos`: mixed traffic, scheduled container stops, independent SHA-256
  bookkeeping, convergence measurement weighted towards genuinely
  under-replicated objects, and a report that always publishes a sample size
  next to every absence claim.

**Measured** (raw files in `bench/results/2026-09-20-yutongzhao/`):

| Claim | Measured | Source |
|---|---|---|
| Corrupted reads, 10-minute chaos run | 0 of 304,325 reads verified | `chaos-latest.json` |
| Convergence to 2 replicas after faults | 217 s over 3,000 sampled objects | `chaos-latest.json` |
| Node stops during the run | 10 | `chaos-latest.json` |
| Client error rate while degraded | 22.6% (the faults landed) | `chaos-latest.json` |
| Hot objects surviving 600 cold writes under quota | 10 of 10 | `quota-eviction.json` |
| Peak usage against the high-water mark | under it, not over | `quota-eviction.json` |
| Eviction throughput after the fix | 28,338 objects/s (was 190) | `eviction-throughput.json` |
| Index scan under concurrent writes | 1.51x the idle scan | `index-scan.json` |
| Tests | 229 unit/property + 40 integration | `phase3-verify.md` |
| Coverage | 82.7% | `phase3-verify.md` |

**Failed / worked around** — five bugs, all in `docs/bugs.md` (entries 6–10):

- Repair skipped work it could not queue, so convergence scaled with cache size
  rather than with how much was broken.
- Repair and eviction fought each other and the quota stopped being enforced —
  nodes sat at 2.6x their configured quota. Two causes: one shared SQLite
  connection starving eviction behind the auditor's scan, and no protocol
  between the two subsystems. Fixed with split pools, paginated scans, and a
  repair hop a full node declines with 507.
- Eviction was fsync-bound at 190 objects/s, below ingest. Removing the
  directory fsync on delete (durability is required for creating an object, not
  for removing one) and batching index removals made it 185x faster.
- The chaos runner reported evicted objects as data loss. It now reads each
  node's `/stats` and attributes them.
- Every node crash-looped once metrics were added: two series of one metric
  registered with different help strings. The unit test had used a placeholder
  help string for both and so held constant the one thing that mattered.

**Next**: Phase 4 (done, below).

## Phase 4 — observability and benchmarks

**Done**

- `cmd/bench`: deterministic corpora, a closed-loop load generator, every
  latency observation kept (nearest-rank percentiles, not a sample), median of
  5 runs with the spread published beside it, and a JSON result that records the
  host, the git commit, the corpus, whether the cache was warm, whether the
  client shared a host with the servers, and whether netem was active.
- `bench -render` regenerates `BENCHMARKS.md` from the committed JSON. The file
  is never hand-edited, which is the mechanism behind rule 1.
- `scripts/netem.sh` applies `tc netem` inside each container's network
  namespace without changing the cache image, and `verify` measures the
  round trip so "this ran under added latency" is itself evidenced.
- CPU and heap profiles collected from every node *during* load.
- Memory sampled twice a second per scenario, which is the evidence that RSS
  does not scale with object size.
- Docs: `docs/performance-methodology.md`, `docs/perf-notes.md`,
  `docs/architecture.md`, `docs/failure-model.md`, `docs/runbook.md`.

**Measured** (raw files in `bench/results/2026-09-20-yutongzhao/`):

| Claim | Measured | Source |
|---|---|---|
| 64 KiB cache hits, 256 concurrent | 61,041 req/s at 16.2 ms p99 | `bench-latest.json` |
| 64 KiB cache hits, 64 concurrent | 46,058 req/s at 5.1 ms p99 | `bench-latest.json` |
| 8 MiB GET aggregate (warm) | 14.5 GiB/s at 256 concurrent | `bench-latest.json` |
| Replicated PUT aggregate | 1,622 MiB/s at 256 concurrent | `bench-latest.json` |
| Peak RSS, 64 KiB objects vs 8 MiB objects | 101.9 MiB vs 138.3 MiB (128x object size, 1.36x memory) | `bench-latest.json` |
| Profiling win 1: eviction throughput | 190 → 28,338 objects/s (149x) | `eviction-throughput.json` |
| Profiling win 2: pooled read buffer | +41.6% ops/s, −31.8% p99 at c=64 | `bench-before-pooled-buffer-fix.json` vs `bench-latest.json` |
| Under netem (42 ms RTT, 0.5% loss) | throughput collapses, **0 errors** | `netem/bench-latest.json` |
| Tests | 251 unit/property + 63 integration | `phase4-verify.md` |
| Coverage | 82.5% | `phase4-verify.md` |

**Failed / worked around**

- The first attempt at profiling win 2 produced a table scattered around zero —
  because the edit had silently failed to apply and both runs used the same
  binary. That accident calibrated this machine's run-to-run noise at roughly
  ±8%, which is now the bar a result has to clear, and added a habit of
  confirming the change is in the deployed artefact before believing a
  comparison. Written up as section 3 of `docs/perf-notes.md`.
- The PUT scenario originally re-uploaded corpus objects, so after the first
  pass every write hit the already-stored fast path and the benchmark reported
  replicated-write throughput roughly ten times too high. It was timing a
  `stat()` call. Fixed by generating a fresh object per write.
- The benchmark counted requests cancelled at the end of the measurement window
  as errors, putting a floor under the error rate that rose with concurrency and
  fell with duration — a property of the stopwatch, not the cache.
- `scripts/hostinfo.sh` was invoked by a relative path, which silently failed
  under `go test` (package directory as cwd) and produced result files with no
  host record. Same class of bug as the results directory earlier; both now
  resolve against the repository root.
- `tc netem` applies to the interface the *client* also reaches the nodes
  through, so the netem run measures the whole system on a slow network rather
  than the inter-node hop in isolation. Recorded in the result's `netem` field
  rather than glossed over.

**Next**: Phase 5 (done, below).

## Phase 5 — Bazel end-to-end benchmark and v1.0

**Done**

- `scripts/bazel-bench.sh`: four phases — cold build with no cache, populating
  build, cache-served rebuild, and a byte-for-byte comparison of the outputs.
  Measured phases repeat 5 times with `bazel clean --expunge` before each. The
  report records whether the cache was actually empty before the populating
  phase, so that number cannot silently become a cache-hit measurement.
- `BENCHMARKS.md` extended with the end-to-end results, still generated.
- `bench/RESULTS_SUMMARY.md`: every target from the specification mapped to its
  measured value, with the ones that were exceeded explained rather than
  celebrated, and the resume bullets rewritten with real figures plus two
  honesty notes about how to quote them.
- `CHANGELOG.md`, complete docs (architecture, failure model, protocol,
  performance methodology, perf notes, runbook, bugs, testing), six ADRs.

**Measured** — `bench/results/2026-09-20-yutongzhao/bazel-bench.json`:

| | Median of 5 runs |
|---|---|
| Cold build, no remote cache | 25.3 s |
| Populating build (from an empty cache) | 28.3 s, 1,387 MiB uploaded |
| Rebuild served from KilnCache | **5.6 s** |
| **Median reduction** | **77.8%** (target 55–75%, goal ≥60%) |
| Actions served from cache | 600 of 600 |
| Outputs identical to the cold build | all 320 artifacts |

**Failed / worked around**

- The clean-checkout verification found the most serious bug in the project:
  an unanchored `.gitignore` pattern (`kilncache`, no leading slash) had
  excluded the entire `cmd/kilncache/` directory, so the main binary's source
  was never committed. Every working-tree check — build, test, lint, the Docker
  image, CI on a push — stayed green, because they all ran against the working
  directory rather than against what git tracks. Recorded as bug 11; the lesson
  is that verifying the artefact and verifying the working directory are
  different things, and only one of them is what other people get.
- `scripts/check-numbers.sh` did not recognise citations written relative to the
  citing file (`results/...` from inside `bench/`), so `RESULTS_SUMMARY.md`
  failed its own rule. The checker now resolves both spellings.

## Final state

| | Measured |
|---|---|
| Tests | 251 unit/property + 63 integration = 314 |
| Coverage (`make cover-all`) | 81–83% across runs |
| Go source | 9,529 non-test + 9,538 test lines |
| Bugs found and documented | 11, in `docs/bugs.md` |
| ADRs | 6 |
| Commits | one per phase, each explaining why |

## Targets (from the spec, section 6 — not yet measured)

These are **targets**, not results. Each is replaced by a measured value with a
raw result file, or reported as not met, in `bench/RESULTS_SUMMARY.md`.

| Metric | Target |
|---|---|
| 64 KiB cache-hit throughput, 64 clients | 1,500–3,000 req/s |
| 64 KiB cache-hit p99 | 10–25 ms |
| 8 MiB GET aggregate throughput | 600–1,200 MiB/s |
| Replicated PUT aggregate throughput | 300–600 MiB/s |
| Bazel clean-rebuild median time reduction (300 C++ targets) | 55–75% |
| Placement imbalance max/mean over 50,000 keys | ≤ 1.10 |
| Keys moved when adding node 4 of 4 | 20–35% (theory 25%) |
| Corrupted successful reads in fault tests | 0, with sample size stated |
| Chaos run | ≥ 10 min, convergence to 2 replicas |
| Tests | 200–300 Go tests, ≥ 80% line coverage |
| Profiling wins documented | ≥ 2 with before/after |
| Repo size | 8–12k Go LOC |
