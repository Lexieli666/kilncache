# Performance notes

Two changes that came out of profiling, with the numbers that justify them —
and one that came out of profiling, was correct, and bought nothing.

Methodology is in [performance-methodology.md](performance-methodology.md).
Raw results are under `bench/results/2026-09-20-yutongzhao/`.

---

## 1. Eviction was fsync-bound and could not keep up with ingest

**How it was found.** A 10-minute chaos run left three nodes sitting at about
16 GB each against a 6 GiB quota — 2.6x over — with eviction apparently stopped.
It was not stopped; it was losing.

**Reproducing it.** `TestEvictionThroughput` fills a store and times a sweep. It
showed nothing at first, reporting 40,767 objects/s: `t.TempDir()` puts the
store on tmpfs, where `fsync` is free. Pointed at ext4 with `TMPDIR`, it
reproduced immediately.

**The measurement.**

Source: [`eviction-throughput.json`](../bench/results/2026-09-20-yutongzhao/eviction-throughput.json)
| | Before | After |
|---|---:|---:|
| Eviction | 190 objects/s | **28,338 objects/s** |
| Ingest on the same machine, single writer | 84 objects/s | 50 objects/s |

Eviction was barely twice the single-writer ingest rate, and a node serves many
writers at once. Under load, usage climbed past the high-water mark and stayed
there.

**The cause.** `Store.Delete` did two expensive things per object: it `fsync`ed
the parent directory after every unlink, and it removed each index row in its
own transaction. The device baseline measures `fsync` at about 1.1 ms on this
host ([`device-baseline.json`](../bench/results/2026-09-20-yutongzhao/device-baseline.json)),
which alone caps eviction below a thousand objects a second.

**The fix, and why it is not an inconsistency.** No directory `fsync` on delete,
and one index transaction per batch.

Publication must be durable because the client was told the object exists
(ADR-0003). A *deletion* need not be. If the machine loses power after the
unlink but before the directory entry reaches stable storage, the file
reappears, startup reconciliation adopts it, and the evictor removes it again.
Nothing is lost; a little disk is occupied for a little while.

**Guard.** `TestEvictionThroughput` fails if eviction ever drops below half the
measured ingest rate. That is the property that actually matters — not an
absolute number, which would be a property of the machine.

**Factor: 149x.**

---

## 2. `io.CopyBuffer` silently discarded the pooled buffer on every read

**How it was found.** A CPU profile taken during a saturated 64 KiB GET
workload, at concurrency 256, with the profile window inside the measured
period.

```
$ go tool pprof -peek makeslice .../profiles-before.../get-64k-node0-cpu.pprof
      flat  flat%   sum%        cum   cum%   calls calls% + context
                                             2.04s 64.15% |   io.copyBuffer
```

A 32 KiB slice was being allocated on essentially every request — 3.4% of all
CPU — despite the read path already taking a 256 KiB buffer from a pool.

**The cause.** The read path did:

```go
buf := getBuf()                                   // 256 KiB from a sync.Pool
defer putBuf(buf)
return io.CopyBuffer(w, struct{ io.Reader }{o}, *buf)
```

`io.CopyBuffer` **ignores the buffer it is given** when the destination
implements `io.ReaderFrom`, and delegates to that instead. The chain here ends
at `net/http`'s response writer, which — because the source is not an
`*os.File`, being wrapped to hide `WriteTo` — falls back to `io.Copy` and
allocates its own 32 KiB buffer.

So the pooled buffer was taken, never written to, and returned, while every
request allocated afresh and did 8x more syscall pairs than intended.

**The fix.** Write the copy loop out explicitly, so the buffer that was
allocated is the buffer that is used. Same change on the forwarded-read path in
the peer client, where neither end is a file and sendfile is unreachable
regardless.

**The measurement.** Median of 5 runs per point, before and after, same host and
same corpus. Sources:
[`bench-before-pooled-buffer-fix.json`](../bench/results/2026-09-20-yutongzhao/bench-before-pooled-buffer-fix.json)
and
[`bench-latest.json`](../bench/results/2026-09-20-yutongzhao/bench-latest.json).
| Scenario | Concurrency | ops/s before | ops/s after | Change | p99 before | p99 after | Change |
|---|---:|---:|---:|---:|---:|---:|---:|
| `get-64k` | 4 | 9,551 | 10,104 | +5.8% | 1.27 ms | 1.10 ms | −13.4% |
| `get-64k` | 16 | 22,055 | 26,986 | +22.4% | 2.46 ms | 1.79 ms | −27.3% |
| `get-64k` | 64 | 32,532 | 46,058 | **+41.6%** | 7.53 ms | 5.13 ms | **−31.8%** |
| `get-64k` | 256 | 46,270 | 61,041 | +31.9% | 23.36 ms | 16.23 ms | −30.5% |
| `mixed-80-20` | 64 | 32,343 | 43,151 | +33.4% | 7.78 ms | 5.47 ms | −29.6% |
| `get-8m` | 4 | 565 | 666 | +17.8% | 10.04 ms | 8.90 ms | −11.4% |
| `put-replicated` | 64 | 1,694 | 1,701 | +0.4% | 219 ms | 203 ms | −7.2% |

The write path is unchanged, as expected — the fix is on the read path, and its
appearing unchanged is a useful control.

Afterwards the allocation is absent from the profile entirely:

```
$ go tool pprof -peek makeslice .../profiles/get-64k-node0-cpu.pprof
(no io.copyBuffer caller)
```

**Factor: 1.42x throughput and 0.68x p99 at the knee.**

---

## 3. A finding that was real and bought nothing

Worth recording, because the first attempt at fix 2 produced this table, from
two runs of the same code
([`bench-before-pooled-buffer-fix.json`](../bench/results/2026-09-20-yutongzhao/bench-before-pooled-buffer-fix.json)
is the later of the two):
| Scenario | Concurrency | Before | After | Change |
|---|---:|---:|---:|---:|
| `get-64k` | 16 | 20,692 | 22,055 | +6.6% |
| `get-64k` | 64 | 32,560 | 32,532 | −0.1% |
| `get-64k` | 256 | 46,317 | 46,270 | −0.1% |
| `put-replicated` | 16 | 574 | 530 | −7.8% |

Scattered around zero, some positive, some negative — exactly what a pair of
runs of *identical code* looks like, which is what it was: the edit had silently
failed to apply, and the "after" containers were running the "before" binary.

Two things came out of that. First, a habit: confirm the change is in the
deployed artefact (`strings .../kilncache | grep copyVerified`) before believing
a comparison. Second, a calibration — this table is what this machine's
run-to-run noise looks like, roughly ±8%, which is the bar a real result has to
clear. Fix 2's +41.6% clears it; the accidental null result does not, and
reporting it as a small win would have been reporting noise.

---

## What dominates now

From the CPU profile of a saturated 64 KiB GET workload after both fixes:

| | Share of CPU |
|---|---:|
| `syscall.Syscall6` | 42.2% |
| `crypto/sha256.block` | 13.1% |
| `runtime.futex` | 3.3% |
| everything else | < 1.3% each |

The system is **syscall-bound**, which is the right place for a cache that is
moving bytes between a file and a socket to be. The remaining SHA-256 is read
verification (`--verify-reads`, on by default) — a deliberate cost, paid so the
node refuses to serve bytes that do not match their key. It is a flag, and
`get-64k-verified` measures what turning the client's half of it on costs.

**What would be next**, if this were continuing:

- **The replicated PUT tail.** The `put-replicated` p99 at concurrency 256 in
  [`bench-latest.json`](../bench/results/2026-09-20-yutongzhao/bench-latest.json)
  is over a second, which is poor.
  Replication is sequential-after-local (ADR-0005): the coordinator writes its
  own copy, then sends it. Streaming to both holders at once through `io.Pipe`
  would cut latency from `local + remote` to `max(local, remote)`. It was not
  done because a mid-stream peer failure would then have no source to retry
  from, and that trade needs measuring rather than guessing.
- **Batching the directory `fsync` across concurrent publishes** into the same
  shard. Objects are spread over 65,536 shards, so the hit rate would be low
  except under a burst — which is exactly when it matters.
