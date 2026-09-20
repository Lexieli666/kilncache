# PROGRESS

Living status file for the unattended build of KilnCache. Re-read this after any
context loss and resume from "Next".

- **Repository**: `Lexieli666/kilncache`
- **Working directory**: `/mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache`
- **Spec**: `../../Project Specs/Project_1_KilnCache_Spec.md`
- **Current phase**: Phase 0 (Kickoff / scaffold)

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

**Next**

- Phase 1: the single-node cache — CAS/AC endpoints, streaming writes with
  bounded memory, digest verification, atomic publication, sharded tree, the
  Bazel C++ fixture, ADR-0003 on durability.

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
