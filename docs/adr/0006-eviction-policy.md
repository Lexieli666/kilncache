# ADR-0006: Size-weighted access-aware eviction, with a SQLite index behind it

- Status: accepted
- Date: 2026-09-20

## Context

A node has a fixed disk and an unbounded stream of build artifacts. Three
decisions follow, and they interact:

1. **Where the metadata lives.** Eviction needs total usage without a
   directory walk, and the coldest objects without a full scan. Neither is
   answerable from the filesystem at a useful speed.
2. **What gets evicted.** Object sizes here span five orders of magnitude: a
   symbol file is a few hundred bytes, a static library tens of megabytes.
3. **When eviction runs.** On the write path, or in the background.

## Decision

### Metadata in SQLite, in WAL mode

One table keyed by `(namespace, key)` with size, creation time, last access and
an access count; one index on last access, which is what turns "give me the
coldest N" from a full scan into a range read.

- **SQLite rather than a Go map plus a snapshot file.** The index must survive a
  crash, answer totals without a scan, and answer coldest-N cheaply. A map gives
  none of those without reimplementing a B-tree and a write-ahead log, which is
  a worse version of what SQLite already is.
- **`modernc.org/sqlite`, the pure-Go driver, rather than `mattn/go-sqlite3`.**
  This is the only reason the runtime image can stay
  `distroless/static:nonroot` with no libc and no shell. That is a real security
  property, not a preference. The driver is pinned to v1.34.5 because newer
  releases require a Go toolchain past this module's `go 1.23`.
- **WAL mode**, so the repair auditor's full-index scan does not exclude the
  writes that requests are making. `TestIndexScanIsNotBlockedByWrites` measures
  a scan both idle and under a continuous writer: 20,000 rows in 27 ms idle
  against 22,453 rows in 41 ms while 3,778 writes landed, a ratio of 1.51
  (`bench/results/2026-09-20-yutongzhao/index-scan.json`). Rollback-journal mode
  would show up as a multiple rather than a few percent, and repair convergence
  would become a function of write load rather than of how much is broken.
- **`synchronous=NORMAL`.** The index is *derived* state: startup
  reconciliation rebuilds it from the disk. Paying a full fsync per metadata
  write to protect data that can be recomputed would be the wrong trade. The
  object files themselves are still fsynced individually (ADR-0003); that is
  where durability lives.

### Reads are recorded in batches, not inline

Recording an access means writing to the index. Doing that inline turns every
cache hit — the operation this whole system exists to make fast — into a
database write. Touches accumulate in memory and flush every few seconds and on
shutdown. `TestIndexTouchesAreBatched` asserts that 100 touches of one key
collapse into a single write.

The cost of a crash is that eviction picks slightly the wrong victim. That is
not a correctness property, and it is the right thing to be cheap about. The
queue is bounded; when it is full, new keys are dropped and counted rather than
blocking a cache hit on a database write.

### Eviction is size-weighted, not plain LRU

```
score = lastAccess − SizeWeight × log2(1 + size / SizeUnit)
```

Lowest score is evicted first. Defaults: `SizeUnit` 64 KiB, `SizeWeight` one
hour, so each doubling of size above 64 KiB makes an object look an hour colder.

Why not plain LRU: under LRU a single 64 MiB object occupies the space of ten
thousand small ones and costs exactly as much to keep. When the quota binds,
that is almost always the wrong way round — those ten thousand small objects
represent ten thousand actions that would otherwise be re-executed.

Why logarithmic and not linear: a linear penalty makes large objects effectively
uncacheable. The goal is to break ties among objects of *similar* age, not to
refuse to store big things. `TestEvictionScorePrefersLargeAndCold` pins both
directions, including that a 64 MiB object read now must outlive a 64 KiB object
last read a week ago.

**The weight is a judgement call, not a measurement.** It says that between two
objects an hour apart in age, the larger by 2x should go first. That is stated
plainly because it is the kind of constant that otherwise acquires false
authority. `make quota-report` measures the consequence so the number can be
revised against evidence.

### Two water marks, and eviction off the write path

Eviction starts at the high-water mark and drains to the low-water mark
(defaults 90% and 80%). With a single threshold, every write past the limit
evicts exactly one object, so a full cache performs a transaction and an unlink
on the write path forever. Draining to a low mark amortises that.

Eviction runs in its own goroutine, woken by writes and by a 30-second ticker,
never inline. Evicting synchronously would make an unlucky PUT's latency include
a scan, a batch of unlinks and a transaction — and make that latency depend on
how full the disk happens to be, which is the least predictable tail a cache can
have.

Candidates are pulled coldest-first in batches of 512 and ranked in a
**size-weighted min-heap**; the heap is popped until enough space is freed.
Ranking needs more candidates than it will use, which is why a batch is read
rather than a single victim.

### The file is unlinked before the index row is removed

If the row went first and the unlink then failed, the file would be invisible to
eviction forever while still occupying disk — a leak that grows silently and has
no symptom until the node is full. The other order's worst case is an index row
with no file, which reconciliation and `Stat` both already handle.

## Consequences

Measured (`bench/results/2026-09-20-yutongzhao/quota-eviction.json`, regenerate
with `make quota-report`): under a 4 MiB quota with 32 KiB objects, 600 cold
writes and a 10-object working set read continuously, peak usage stayed **below**
the high-water mark, all 10 hot objects survived, and the index's byte total
matched the disk exactly.

**Documented tolerance**: usage may exceed the high-water mark by up to the size
of one object, because eviction is woken by a write rather than preceding it.
The quota is therefore a bound on steady-state usage, not an instantaneous
ceiling. `TestSweepStaysWithinQuota` asserts exactly `high_water + one object`
and fails on anything larger.

What this gives up:

- **No global view.** Each node evicts locally, so one holder can evict an
  object while the other keeps it. Repair then treats that as a missing replica
  and recreates it, which is correct but means eviction and repair can chase
  each other under a tight quota. The gap between the water marks exists partly
  to give repair room to converge between sweeps.
- **`MaxBytes` counts object bytes, not disk usage.** Filesystem overhead, the
  index database and its WAL are not included, so a quota set to the size of the
  partition will still fill it. The runbook says to leave headroom.
- **Reconciliation is O(objects) at startup.** On a million-object cache that is
  seconds, before the node reports ready. The alternative — trusting the index
  and repairing lazily — would let a node answer HEAD with 200 for an object it
  lost, which is the one answer a cache must never give.

## Alternatives

- **Plain LRU.** Simpler and standard. Rejected for the size argument above; the
  code is a one-line policy change if the weighting ever proves harmful.
- **LFU or ARC.** Better hit rates on skewed workloads, more state per object,
  and much harder to explain in an incident. A build cache's access pattern is
  dominated by recency (a branch is worked on, then abandoned), which is what
  the current policy is shaped around.
- **GDSF (Greedy Dual Size Frequency).** The principled version of what this
  approximates, including a cost term and an inflation factor. The inflation
  factor is the part that makes it awkward to reason about under a moving
  workload, and the extra fidelity would be unmeasurable at this scale.
- **Evict on the write path.** No background goroutine, no wake-up channel, and
  a tail latency that depends on disk fullness.
- **No quota; let the disk fill.** What many caches do. The failure mode is the
  node dying at 3am with ENOSPC, having served correctly until the moment it
  could not write at all.
