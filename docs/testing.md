# Testing strategy

Six kinds of test run against KilnCache. Each answers a question the others
cannot, and each names the claim it would falsify.

| Layer | Command | Answers |
|---|---|---|
| Unit | `make test` | Does this function do what its doc comment says? |
| Property | `make test` (rapid-driven tests) | Does this hold for *every* input, not the three I thought of? |
| Race | `make test-race` | Is the concurrency safe, or merely lucky? |
| Integration | `make integration` | Do real processes over real sockets and real disks agree? |
| Fault injection | `make integration` + `scripts/netem.sh` | What happens when a peer is slow, lossy, or gone? |
| Chaos | `make chaos` | Does the invariant survive minutes of unscheduled failure? |
| Benchmark | `make bench` | How fast, on what hardware, reproducibly? |

## Unit tests

Standard `go test`, colocated with the code. They cover the pure logic: size
parsing, placement arithmetic, shard path construction, heap ordering, header
handling.

A unit test earns its place by failing for a reason someone would otherwise
ship. `TestParseSize` asserts that `1GB` is 1,000,000,000 and `1GiB` is
1,073,741,824 because an operator who writes `1GB` and silently gets 7% more
disk than they asked for has been lied to by the software.

## Property tests

`pgregory.net/rapid` generates inputs and shrinks failures to a minimal case.
Used where the specification is a property rather than a table of examples:

- **Placement is deterministic.** For any key, every node computes the same
  owner list. Falsifier: two independently constructed rings disagree.
- **Primary and replica are distinct.** Falsifier: a key whose two copies land
  on one node, which would make replication a no-op and the failure test a lie.
- **Placement is balanced.** Over 50,000 keys, max load / mean load stays within
  the documented bound. Falsifier: a hash that clusters.
- **Placement is stable under growth.** Adding a fourth node moves close to the
  theoretical 1/4 of keys, not most of them. Falsifier: any scheme with the
  reshuffling behaviour of naive modulo hashing.
- **Round-tripping.** Anything written and read back is byte-identical, for
  arbitrary sizes including zero.

Property tests use a fixed seed in CI so a failure is reproducible, and the
failing seed is printed so it can be replayed locally.

## Race tests

`go test -race ./...` runs in CI on every push, not nightly. A data race that
only the nightly job catches is a data race that shipped. The race detector is
the named falsifier for every "safe under concurrency" claim in the README —
concurrent PUTs of the same key, concurrent eviction and read, repair running
against live traffic, and shutdown racing in-flight requests.

## Integration tests

Build tag `integration`, so `go test ./...` stays fast for the edit loop. Two
tiers:

1. **In-process cluster.** Real `net/http` servers on loopback with real
   temporary directories, started by the test. Fast enough to run on every
   change, real enough to exercise forwarding, replication and fallback.
2. **Docker cluster.** The actual compose topology, built from the actual
   Dockerfile. This is the only tier that can prove a container can be killed
   and come back, so the node-loss and restart tests live here.

Docker tests skip with a clear message when Docker is unavailable, rather than
failing. They never skip silently — a skipped test that looks like a pass is
worse than a missing test.

## Fault injection

Two mechanisms, deliberately different:

- **In-process faults.** A peer transport that can be told to fail, hang, or
  truncate. Deterministic, fast, and able to hit paths a network cannot reach on
  demand (a peer that accepts a body and then dies before fsync).
- **`tc netem`.** Real added latency, real packet loss, on the real container
  network. Slower and noisier, but it exercises the actual socket timeouts and
  the actual retry behaviour rather than a mock of them. `scripts/netem.sh`
  applies and removes the qdisc; every benchmark run records whether netem was
  active.

## Chaos

`cmd/chaos` drives mixed PUT/GET traffic against the compose cluster for a
configured duration while stopping and restarting containers on a schedule. It
holds the expected SHA-256 for every object it wrote, independently of the
server, and verifies every byte it reads back.

The chaos run reports, and its committed report file records:

- total operations, by kind and outcome;
- **corrupted reads: the count, alongside the number of reads checked.** A
  "zero corrupted reads" claim with no sample size is meaningless; both numbers
  are always published together;
- error rate by class (connection refused, timeout, 5xx), which is expected to
  be non-zero while a node is down — a chaos run with a zero error rate means
  the faults did not land;
- time from a node returning to the cluster reaching two replicas for every
  retained object.

## Benchmarks

`cmd/bench` generates a deterministic corpus and measures each scenario at
several concurrency levels, at least five runs per scenario, emitting JSON.
`BENCHMARKS.md` is generated from those JSON files; it is never hand-edited.

Every result file embeds `scripts/hostinfo.sh` output, the git commit, the
corpus description, whether the client shared a host with the servers, whether
the cache was warm, and whether netem was active. A number without that context
is not a measurement.

Benchmarks do not run in CI. Shared runners have noisy neighbours and no storage
guarantee, so a number produced there would be unreproducible by construction.

## What is deliberately not tested

- **Byzantine peers.** A peer that lies about a digest is out of scope; the
  trust boundary is the cluster.
- **Power loss.** The durability argument (ADR-0003) rests on fsync ordering
  that this suite cannot actually verify without a device that can be cut. The
  tests verify the *calls* happen in the right order; the ADR states plainly
  that the guarantee itself is inherited from the filesystem, not proven here.
- **Bazel's own correctness.** The fixture build verifies that outputs are
  identical between a cold build and a cache-served build. It does not attempt
  to validate Bazel.
