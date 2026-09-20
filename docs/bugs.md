# Bugs the tests caught

A bug list is evidence that the tests work. An empty one is evidence that they
don't. Every entry names the test or tool that found it, what was actually
wrong, and what changed — including the ones that were my own mistakes in the
test rather than in the product, because confusing those two is itself a common
failure.

Entries are newest last.

---

## 1. Generated `BUILD.bazel` was syntactically invalid

**Found by**: `bazel build //...` on the generated fixture — the first time it
was run, which is the point of running it.

**Symptom**: `indentation error` at line 290, `syntax error at 'outdent'` at the
end of the file.

**Cause**: the generator built each rule with `textwrap.dedent` on an f-string.
`dedent` strips the *common* leading whitespace of a block. The interpolated
dependency list carried its own, shallower indentation, so the common prefix
shrank and every surrounding line came out under-indented. Rules with no
dependencies were fine; rules with dependencies were not, which is why the first
34 targets parsed.

**Fix**: build the rule text by explicit concatenation. Less clever, and it
cannot go wrong this way.

**Falsifier**: the fixture is built end to end in Phase 1 verification; a
malformed `BUILD.bazel` fails immediately and loudly.

---

## 2. The fixture was too small to measure anything

**Found by**: timing the first complete build — 7.4 s wall, 1.09 s critical
path, on 32 cores.

**Symptom**: not a crash. A benchmark that would have "worked" and reported a
meaningless number, which is worse.

**Cause**: 300 targets of trivial C++. Bazel's own loading and analysis
dominated; the compiler barely ran. A cache measured against that workload would
have been measuring Bazel's startup time, and the resulting percentage would
have been real, reproducible, and about the wrong thing.

**Fix**: the generator now emits template-instantiation ballast per translation
unit. The cold build is about 24 s with a 7.7 s critical path, and object files
are a few hundred KiB each, so the benchmark moves a realistic number of bytes
as well as spending realistic CPU.

**Note**: this is the failure mode the repository's first rule is aimed at. The
number would have passed every check except "is this measuring what I claim".

---

## 3. `pkill -f kilncache` killed the shell that ran it

**Found by**: an overnight automation step exiting with status 144 and no output.

**Cause**: `pkill -f` matches against full command lines, and the command line
of the shell running `pkill -f kilncache` contains the string `kilncache`. It
matched itself.

**Fix**: `scripts/localnode.sh`, which tracks PIDs in files. Boring, and cannot
do this.

---

## 4. An integration test raced the server it was testing

**Found by**: `TestClientDisconnectMidUpload`, intermittently — and it was the
*test* that was wrong.

**Symptom**: `partial uploads left in .../tmp: [incoming-2561662586]`.

**Cause**: the test severed a TCP connection mid-body and then immediately
asserted that the temp file was gone. But when a client hangs up, the server
does not find out until its next read fails, which happens after the client has
already moved on. The assertion was racing the cleanup.

**Fix**: two assertions instead of one, with the distinction written down.
`assertNoTempFiles` is used where the server answered *us* — the response cannot
be written until the write path has unwound, so cleanup has provably happened.
`assertNoTempFilesEventually` is used where the client hung up, and waits with a
deadline, because the honest claim there is "cleanup happens promptly", not
"cleanup is faster than the test".

**Why it is recorded**: the tempting fix was a `time.Sleep` before the
assertion, which would have hidden the question rather than answering it.

---

## 5. The container could not create its own data directory

**Found by**: `TestDockerClusterSurvivesNodeLoss`, on its first run —
`container kilncache-node-a is unhealthy`.

**Symptom**:
`open store: storage: create /var/lib/kilncache/tmp: mkdir ...: permission denied`,
in a crash loop.

**Cause**: the runtime image is `distroless/static:nonroot`, so the process runs
as UID 65532. `/var/lib/kilncache` existed only as a volume mount point, not as
a path in the image. When Docker initialises a fresh named volume it copies the
image's contents *and ownership* at that path — and when the path does not exist
in the image, the volume is created owned by root. The nonroot process then
cannot create anything inside it. Distroless has no shell and no `mkdir`, so
there is no startup hook that could have fixed it.

**Fix**: create an empty directory in the build stage and
`COPY --from=build --chown=65532:65532` it to `/var/lib/kilncache`, so the
volume inherits the right ownership at initialisation.

**Why Phase 0 missed it**: Phase 0's compose check started the containers and
curled `/healthz`, but Phase 0 had no storage layer, so nothing ever tried to
write to the volume. The health check passed on a node that could not have
served a single object. This is a good argument for health checks that exercise
the thing being claimed, and `/readyz` now fails until the store opens.

---

## 6. Repair skipped work it could not queue, so convergence scaled with cache size

**Found by**: a chaos run's convergence measurement — after the first version of
that measurement was itself fixed (see below).

**Symptom**: after a node returned, under-replicated objects were repaired at
roughly 17 per second, and a 90-second run did not converge within two minutes.

**Cause**: two compounding mistakes.

1. When the bounded repair queue was full, the audit dropped the task **and
   advanced its cursor past it**. The pass still looked like progress, but the
   objects behind the full queue were not examined again until the cursor
   wrapped all the way round the index. Convergence time became a function of
   total cache size rather than of how much was actually broken.
2. The auditor then slept the full repair interval (60 s) before the next pass,
   so the repair rate was "one queue's worth per interval" regardless of how
   much needed fixing.

**Fix**: a full queue now stops the pass with the cursor on the last object that
was actually accepted, so the next pass resumes exactly there. And the wait
between passes is adaptive: a pass that ends with work pending schedules the
next one after a configured 250 ms backoff, while a pass that finds nothing to
do waits the full repair interval.

**Falsifiers**: `TestFullQueueStopsThePassInsteadOfBlockingOrSkipping` (does not
block, does not run past the full queue) and `TestFullQueueDoesNotSkipObjects`
(with a queue of depth 4, all 30 objects still get repaired).

**Measured after**: the same shape of run converged in 69 s.

---

## 7. Repair and eviction fought each other, and the quota stopped being enforced

**Found by**: the first full 10-minute chaos run.

**Symptom**: three nodes sitting at about 16 GB each against a 6 GiB quota — 2.6x
over — with eviction having stopped entirely, and the repair worker logging
"recreated a missing copy" continuously.

**Cause**: two independent problems that combined into a livelock.

1. **The index had one connection for everything.** `MaxOpenConns(1)` is the
   right cap for SQLite *writes*, which serialise anyway. Applying it to reads
   as well threw away the main reason WAL was chosen. The repair auditor
   streamed the whole index as one long-lived cursor, holding that single
   connection for the duration, and every eviction sweep, access-time flush and
   metadata write queued behind it.
2. **Eviction and repair had opposite goals and no protocol between them.** Node
   A evicts an object to get under quota. Node B's auditor sees a holder missing
   its replica and sends the object straight back. A evicts it again. Neither
   subsystem is wrong on its own terms, and the quota is never enforced.

**Fix**:

- Separate read and write pools over the same file: one write connection, four
  read connections. WAL allows concurrent readers alongside the writer.
- The auditor reads the index in keyset-paginated pages of 512 instead of one
  streamed cursor, so no connection is held across the pass. Keyset rather than
  `LIMIT/OFFSET` because the table is `WITHOUT ROWID` keyed on `(ns, key)`, so
  each page is a direct index seek; `OFFSET` would make a full sweep quadratic.
- A new hop role, `HopRepair`, terminal like `HopReplica` but declined with
  **507** by a node already above its high-water mark. A client's write is new
  data someone is waiting for and is always accepted; a repair write is a copy
  of something this node has already decided it has no room for. The sender
  counts a 507 as *declined*, not failed.

**Falsifiers**: `TestRepairWriteDeclinedWhenOverQuota`,
`TestNoQuotaProbeAcceptsRepairWrites`,
`TestRepairDeclinedOverQuotaIsNotAFailure`, and
`TestIndexScanIsNotBlockedByWrites`.

**Note**: this was predicted. ADR-0002 and ADR-0006 both say eviction and repair
"can chase each other under a tight quota". Writing that down did not prevent
it; the chaos run is what turned a known risk into a reproduced failure with a
number attached.

---

## 8. Eviction was fsync-bound and could not keep up with ingest

**Found by**: the same chaos run, then reproduced in isolation by
`TestEvictionThroughput` — which only reproduced it once the test was pointed at
a real filesystem. On the tmpfs that `t.TempDir()` uses by default it measured
40,767 objects/s and showed nothing at all.

**Symptom**: a sweep evicted about 190 objects per second. Ingest on the same
machine ran at 84 objects/s per writer with several writers, so eviction lost.
Usage climbed past the high-water mark and stayed there.

**Cause**: `Store.Delete` fsynced the parent directory after every unlink, and
removed each object's index row in its own transaction. At roughly 1.1 ms per
fsync on this host
(`bench/results/2026-09-20-yutongzhao/device-baseline.json`), the fsync alone
capped eviction at under a thousand objects a second, and the per-object commit
took most of what was left.

**Fix**: no directory fsync on delete, and one index transaction per batch.

The asymmetry with `Put` is the whole point, and it is not an inconsistency.
Publication must be durable because the client was told the object exists
(ADR-0003). A deletion need not be: if the machine loses power after the unlink
but before the directory entry is stable, the file reappears, startup
reconciliation re-adopts it, and the evictor deletes it again. Nothing is lost.

**Measured**: 190 → 35,144 objects/s on the same filesystem, a factor of 185.
Raw results in `bench/results/2026-09-20-yutongzhao/eviction-throughput.json`;
`TestEvictionThroughput` fails if eviction ever falls below half the ingest rate
again.

---

## 9. The chaos runner reported eviction as data loss

**Found by**: reading the first 10-minute chaos report, which said
`FAIL: 1266 acknowledged objects are gone`.

**Cause**: the runner verified every object it had written and treated any 404
as data loss. The cache had been given a 6 GiB quota and the run wrote 27.5 GiB,
so most of those objects had been evicted — correctly. From outside the cluster
an evicted object and a lost object are the same 404.

**Fix**: the runner now reads each node's `/stats` at the end of the run and
records eviction totals alongside the verification result. Missing objects are
reported either way, but the verdict only calls them data loss when no eviction
was observed.

**Why it is recorded**: this is a measurement bug, not a product bug, and it is
the more dangerous kind. It failed *loudly* and in the pessimistic direction,
which is lucky. The same class of error in the other direction — a verifier that
asks the server what a digest should be, rather than keeping its own record —
would have passed silently for exactly the corruption it exists to catch. The
runner computes every expected digest itself and never reads one back from the
cluster.

---

## 10. Every node crash-looped on startup after metrics were added

**Found by**: `docker compose up --wait` — `container kilncache-node-a is
unhealthy`. Not by the unit tests, which passed.

**Symptom**:

```
kilncache: register metrics: a previously registered descriptor with the same
fully-qualified name as Desc{fqName: "kilncache_hits_total", ...} has different
label names or a different help string
```

**Cause**: `hits_total` is registered twice, once per `source` label value
(`local` and `peer`). Prometheus requires every series of a metric to share one
help string, and the two registrations had different sentences — "Reads served
from the local disk." and "Reads served from a peer." Help text describes the
*metric*, not the series.

**Why the unit test missed it**: `TestSourcesWithDistinctLabels` registered two
sources with the same name and different labels, which was the right shape — but
it used the placeholder help string `"h"` for both. The one property that
actually mattered was the one the test held constant.

**Fix**: one help string covering both series, and a check in `RegisterSources`
that rejects same-name-different-help up front, printing both strings. The
client library's own message names an internal descriptor and is hard to act on.

**What went right**: the failure was at startup, not at first scrape, because
registration happens during node assembly and a failure there aborts the boot.
The health check then correctly reported the node as unhealthy rather than
letting it serve. That was a deliberate choice made when the registry was
designed, and it is the reason this was a two-minute fix rather than a mystery
in production.

**Falsifier**: `TestSameNameDifferentHelpIsRejectedClearly`, which now uses the
real help strings and asserts the error names both.
