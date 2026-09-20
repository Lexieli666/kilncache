# How KilnCache is measured

This document exists so that the numbers in [BENCHMARKS.md](../BENCHMARKS.md)
can be attacked. Every choice below is one that could have been made to flatter
the result, and each says which way it was made and why.

## The rules the numbers obey

1. **Nothing is published without raw output.** Every figure in the README or
   BENCHMARKS.md comes from a JSON file committed under `bench/results/`.
   `scripts/check-numbers.sh` enforces it in CI: a numeric claim with no
   matching result file fails the build.
2. **BENCHMARKS.md is generated, never edited.** `bench -render` rewrites it
   from `bench-latest.json`. There is no path by which a remembered figure
   reaches the documentation.
3. **The median is published, not the best.** Each point is the median of five
   runs, with the full range printed beside it.
4. **Absence claims carry their sample size.** "Zero corrupted reads" is
   meaningless without the number of reads checked, so the two are always
   printed together.

## What is measured

| Scenario | Question |
|---|---|
| `get-64k` | How many small cache hits per second, and at what tail latency? This is the common case: a Bazel action fetching one output. |
| `get-8m` | How much bandwidth for large objects — a static library or a debug binary? |
| `put-replicated` | How fast can objects be written when every write places a second copy before returning? |
| `mixed-80-20` | What does a partially warm cache look like, where a fifth of lookups miss? |
| `get-64k-verified` | What does it cost to re-hash every response body — the price of proving correctness on the read path? |

Each runs at concurrency 4, 16, 64 and 256.

## How the load generator works

**Closed-loop.** Each of N workers issues its next request as soon as the
previous one returns. The concurrency column therefore means "N requests in
flight".

This is the right model for this system: the question an operator has is "how
many parallel Bazel actions can this serve?", not "what happens at a fixed
offered rate of X". It also sidesteps coordinated omission — there is no
schedule for a slow response to fall behind, because the next request does not
exist until this one finishes. An open-loop generator would answer a different
question and would need an explicit correction to report latency honestly.

The trade-off, stated plainly: closed-loop cannot show what happens when
arrivals exceed capacity, because arrivals are capacity by construction. To see
overload behaviour, read across the concurrency column — when ops/s stops rising
and latency starts, that is the knee.

**Warmup is unmeasured but still executed.** The first requests hit a cold page
cache and an empty connection pool. Those requests are real, and including them
would make every run's p99 a measurement of process startup.

**Every observation is kept.** Percentiles are nearest-rank over all of them,
not over a reservoir sample. A p99 estimated from a sample is an estimate of
exactly the part that matters, and an *interpolated* p99 is a value that was
never observed.

**Requests cancelled by the end of the window are discarded, not counted as
errors.** They are an artefact of the stopwatch: counting them puts a floor
under the error rate that rises with concurrency and falls with duration, which
looks like a property of the cache and is a property of the harness.

**The client's connection pool is sized for the concurrency.** Go's default of
two idle connections per host would make a 256-way run measure TCP handshakes
and report them as cache latency.

## Where the numbers are honest about their limits

**The client shares a host with the servers.** There is no physical network in
these figures. They bound what the server can do; they are not what a remote
client would see. Every result file records `client_colocated_with_servers`.

**Reads are warm.** Dropping the page cache requires root, which the
development host does not grant. Each scenario's result carries a
`page_cache_note` stating the corpus size against host RAM, and says plainly
when the corpus could have been served entirely from memory. The `fio` device
baseline is the honest floor for the cold case.

**The host is a WSL2 virtual machine.** Its virtual disk sits behind the Windows
host's page cache, so reads look better than a physical NVMe device would and
`fsync`-bound writes look worse. The device baseline is measured on the same
filesystem, which is what makes the comparison meaningful even though the
absolute numbers are not transferable.

**PUT benchmarks write genuinely new objects.** Re-uploading a corpus object
would measure the already-stored fast path: a CAS object whose key is present is
correct by definition, so the store skips the stream, the fsync and the
replication and answers 200. An early version of the benchmark did exactly that
and reported replicated-write throughput an order of magnitude too high — it was
timing a `stat()` call. The generator now stamps a worker ID and counter into
each object and recomputes its digest, so every write is new. That costs the
client a SHA-256 pass per object, which Bazel also pays for real.

**Corpus content is pseudo-random, not zeros.** A store that accidentally
deduplicated, compressed or truncated would look excellent on a corpus of zeros.

## Network faults

`scripts/netem.sh` applies `tc netem` delay, jitter and loss inside each
container's network namespace. The cache image is unchanged; a separate
privileged container joins the namespace to run `tc`.

Any run made under netem records the conditions in its `netem` field, and
BENCHMARKS.md prints them. `scripts/netem.sh verify` measures the round trip, so
a claim that a run happened under added latency is itself evidenced rather than
asserted.

## Profiling

CPU and heap profiles are collected from every node *while the load is running*,
at the highest concurrency point of each scenario. Profiling an idle process
produces a flamegraph of the scheduler.

Profiles land in `bench/results/<date>-<host>/profiles/` and are committed.
Findings and their before/after numbers are in
[perf-notes.md](perf-notes.md).

## Memory

Resident memory is sampled from every node twice a second for the duration of
each scenario, via `process_resident_memory_bytes` on `/metrics`. The peak is
published per scenario.

This is the evidence for the claim that memory does not scale with object size:
objects stream through a fixed 256 KiB buffer, so peak RSS should be
approximately equal for a 64 KiB scenario and an 8 MiB one. A server that
buffered whole objects would show peak RSS tracking object size × concurrency,
and this table is where that would be visible.

## Reproducing

```bash
make compose-up                      # three nodes, named volumes
make device-baseline                 # fio floor for this filesystem
make bench                           # the full matrix, 5 runs per point
make benchmarks                      # regenerate BENCHMARKS.md from the JSON
make chaos                           # 10-minute failure run
```

Each writes into `bench/results/<ISO-date>-<hostname>/`. Committing that
directory is what makes the numbers checkable by someone who was not there.
