# ADR-0003: Publish objects with fsync(file) → rename → fsync(dir)

- Status: accepted
- Date: 2026-09-20

## Context

A PUT must satisfy two things that pull in different directions:

1. **An incomplete upload is never visible.** A client that disconnects halfway
   through, or that sends bytes not matching its key, must leave nothing a
   subsequent GET can find. A build that silently consumes half an object
   produces a broken binary, and the failure surfaces somewhere else entirely.
2. **An acknowledged write survives.** Once a 2xx is returned, the object should
   still be there after the machine loses power — otherwise the replication
   count is a fiction and the repair worker cannot tell a lost object from one
   that was never written.

Writing directly to the final path fails the first requirement outright: the
file exists, at its final name, the moment it is created, and a concurrent GET
can open it while it is still half-written.

The subtler problem is the second. Almost every implementation gets as far as
"write to a temp file, fsync it, rename it" and stops. That is not sufficient.
`rename(2)` is atomic with respect to concurrent readers — the entry either
resolves to the old inode or the new one, never to something in between — but
atomicity is not durability. The directory entry created by the rename lives in
the parent directory's own metadata, and until *that* reaches stable storage the
rename can be lost by a power failure even though the file's data is safely on
disk. The result is the worst of both worlds: an object whose data is present
and whose name is gone, while anything that recorded the successful write (the
metadata index, the replication counter, the client) believes it exists.

## Decision

Publication is exactly this sequence, and the order is not negotiable:

```
1. create a uniquely named temp file under <root>/tmp/
2. stream the body into it, hashing as we go, through a fixed-size buffer
3. verify the digest (CAS only); reject and delete before anything is published
4. fsync(file)          - the data is on stable media
5. close(file)
6. rename(tmp, final)   - atomic for concurrent readers
7. fsync(parent dir)    - the *name* is on stable media
```

Supporting decisions:

- **Temp files never look like keys.** They carry an `incoming-` prefix, which
  is not valid hex, so no GET can ever resolve to one even by accident. This is
  a structural guarantee rather than a check that could be forgotten.
- **Temp files live in `<root>/tmp/`, on the same filesystem as the object
  tree.** `rename(2)` fails with `EXDEV` across filesystems, so the temp
  directory must be a sibling of the shard tree, not `/tmp`.
- **Startup sweeps `<root>/tmp/`.** A crash leaves partial uploads; without a
  sweep they accumulate until the disk fills. The sweep runs before the node
  reports ready, and removes only files carrying the `incoming-` prefix —
  anything else in that directory is left alone, because deleting unrecognised
  files on a misconfigured path is worse than leaking them.
- **The client's Content-Length is enforced.** A body that ends early would be
  caught for CAS by the digest check, but an AC entry has no digest; without
  this check a truncated `ActionResult` would be published as valid.
- **A failed directory fsync downgrades the claim, it does not fail the write.**
  Some filesystems and container storage drivers reject `fsync` on a directory
  outright. The object is present and readable, so failing the request would
  make the cache unusable there for no benefit. Instead the store records that
  directory fsync is unsupported, warns once, and reports `durable: false` on
  the result. Benchmark and chaos reports carry that flag, so a durability claim
  made on a filesystem that cannot support it is visibly not a durability claim.

## Consequences

- Every new object costs two fsyncs. On the machine this was developed on, a
  4 KiB write with `fdatasync` measured 914 IOPS — roughly 1.1 ms per sync — so
  the publish path is latency-bound on small objects, not bandwidth-bound. That
  number and its raw fio output are in
  `bench/results/2026-09-20-yutongzhao/device-baseline.json`, and it is the
  floor the PUT benchmark is read against.
- A CAS object that is already present skips the write entirely, since its key
  determines its content. That removes both fsyncs from the common path in a
  warm cache. AC entries are never skipped, because their value can change.
- Directory fsync is per shard directory, not per object, so a burst of writes
  into the same shard does not multiply the cost proportionally — but it is not
  batched either, which is a deliberate simplification. Batching would mean
  holding acknowledgements open across writes, which trades the guarantee this
  ADR exists to provide for throughput.

## Falsifiers

- `TestInterruptedUploadIsNeverVisible` severs the body mid-stream and asserts
  the object is absent and no temp file remains.
- `TestShortBodyIsRejected` and `TestLongBodyIsRejected` cover the AC path,
  where there is no digest to catch truncation.
- `TestPutRejectsDigestMismatch` asserts rejection before publication.
- `TestSweepTempsOnOpen` asserts the crash sweep removes partials and leaves
  unrecognised files alone.
- `TestPutResultReportsDurability` asserts the store and the per-write result
  never disagree about whether the write was durable.
- `TestConcurrentPutsOfSameKey` and `TestConcurrentPutAndGet` run the race under
  `-race`, including a reader holding an inode across a rename.

What these do **not** prove: that the guarantee holds under real power loss. That
would need hardware that can be cut mid-write. The tests prove the calls are made
in the right order; the guarantee itself is inherited from the filesystem, and
this document says so rather than implying more than was verified.

## Alternatives

- **Write in place.** Simplest, and violates requirement 1 immediately.
- **`O_TMPFILE` plus `linkat`.** Genuinely elegant: the file has no name at all
  until it is published, so a partial upload is unreachable by construction
  rather than by naming convention. Rejected because it is Linux-specific and
  still requires the directory fsync, so it removes the temp-file sweep but not
  the expensive part. Worth revisiting if the sweep ever becomes a problem.
- **Skip `fsync(file)`, keep the rename.** Gives atomicity without durability —
  a file with the right name and arbitrary contents after a crash. For a cache
  whose whole premise is that the bytes match the key, that is the one outcome
  that must not happen.
- **Skip the directory fsync.** The common shortcut. It is invisible in testing
  and wrong only when it matters, which is exactly the class of bug this project
  exists to take seriously.
