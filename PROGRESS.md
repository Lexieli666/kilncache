# PROGRESS

Living status file for the unattended build of KilnCache. Re-read this after any
context loss and resume from "Next".

- **Repository**: `Lexieli666/kilncache`
- **Working directory**: `/mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache`
- **Spec**: `../../Project Specs/Project_1_KilnCache_Spec.md`
- **Current phase**: Phase 2 complete; Phase 3 next

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

**Next**

- Phase 3: SQLite (WAL) metadata index, quota with high/low water marks,
  size-weighted access-aware eviction, startup reconciliation that never marks a
  missing file valid, a bounded repair worker pool, and `cmd/chaos`.

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
