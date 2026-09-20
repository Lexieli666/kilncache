# Failure model

What KilnCache does when things go wrong, what it guarantees, and — the section
that matters most — what it does not.

Every row names the test or the run that would catch it if the behaviour
changed. A failure mode with no falsifier is a hope.

## Summary

| What fails | What happens | Falsifier |
|---|---|---|
| One node is down | Reads continue from a surviving copy. Writes whose holders include the dead node fail with 503. | `TestReadSurvivesLosingTheHolder`, `TestWriteFailsLoudlyWhenAReplicaIsDown`, `TestDockerClusterSurvivesNodeLoss` |
| A replica write fails | The PUT returns **503**, never 200. | `TestReplicaWriteFailureIsA503Path` |
| A client disconnects mid-upload | Nothing is published; the temp file is removed. | `TestClientDisconnectMidUpload`, `TestInterruptedUploadIsNeverVisible` |
| A node crashes mid-upload | The partial file is swept at startup, before the node reports ready. | `TestSweepTempsOnOpen` |
| A local object is corrupted | Verified reads refuse it and the failure is counted; repair can replace it. | `TestVerifyReadsDetectsLocalCorruption`, `TestReconcileCorrectsSize` |
| The index disagrees with the disk | Startup reconciliation rebuilds it from the disk. An index row with no file cannot survive. | `TestReconcileNeverMarksAMissingFileValid`, `TestReconcileAdoptsUnknownFiles` |
| A peer is slow or hung | The per-hop deadline fires; the request does not hang. | `TestSlowPeerHitsTheHopTimeout` |
| Every holder is unreachable | **503**, never 404. A partition must not look like a cold cache. | `TestAllHoldersUnreachableIsNotAMiss` |
| The disk fills | Eviction drains from the high-water mark to the low-water mark. | `TestSweepStaysWithinQuota`, `TestHotObjectsOutliveColdOnes` |
| A node restarts | It serves its own copies again from its volume; repair fills what it missed. | `TestRestartRejoinsCluster`, `TestDockerClusterSurvivesNodeLoss` |
| Two nodes disagree about placement | The hop state machine bounds forwarding at three nodes. | `TestCoordinatorOnlyIssuesReplicaWrites`, `TestForwardedRequestIsNotForwardedAgain` |
| Sustained mixed failure | 10-minute chaos run: 0 corrupted reads of 304,325 verified, converged to 2 replicas in 217 s. | [`chaos-latest.json`](../bench/results/2026-09-20-yutongzhao/chaos-latest.json) |

## The guarantees

**A GET never returns bytes that do not match their key.** CAS objects are
verified on ingest and, when `--verify-reads` is on (the default), again while
being served. Bazel verifies independently too, and the chaos runner keeps its
own digests and checks every read against them. Three independent checks, and
the "zero corrupted reads" claim is measured by the one that does not trust the
server.

**An acknowledged PUT means `replica_count` nodes hold the object.** If fewer
copies could be written, the client gets 503 with the counts in
`X-Kilncache-Copies` and `X-Kilncache-Copies-Wanted`.

**A partial upload is never visible.** Temp files carry a prefix that is not
valid hex, so no key can resolve to one; publication is by atomic rename; and a
crash's leftovers are swept before the node reports ready.

**A node that has lost an object says so.** The disk is authoritative and the
index is derived, so a HEAD cannot return 200 for a file that is gone.

## What is not guaranteed — read this part

**Action cache entries are not content-addressed.** A `PUT /ac/<hash>`
overwrites. Two clients writing different results for the same action key
concurrently produce last-writer-wins, and the two nodes holding the copies can
disagree about which write won. This is the one place the design is genuinely
weaker than a consensus-based cache.

It is acceptable because an AC entry is a pointer to CAS objects that are
themselves verified: a stale AC entry causes a wasted download and a cache miss,
never a corrupt build output. It is stated here, in
[ADR-0002](adr/0002-no-consensus.md), and in
[docs/protocol.md](protocol.md) rather than left to be discovered.

**Losing both holders loses the object.** Two copies across three nodes. For a
build cache that is a cache miss, not data loss — the build graph is the source
of truth — but it is not durability and is not claimed as such.

**A peer that acknowledges a write it did not persist is invisible at write
time.** The coordinator counts the ack and returns success. The missing copy is
the repair auditor's problem, and it is found on the next pass rather than
immediately. `TestTruncatingPeerIsAcceptedThenDetectedByRepair` exists to keep
this documented rather than discovered.

**Durability is inherited from the filesystem, not proven here.** The tests
verify that `fsync`, `rename` and the directory `fsync` happen in the right
order. They cannot verify that the guarantee holds under real power loss, which
would need hardware that can be cut mid-write. If the filesystem cannot `fsync`
a directory at all, the store records that and reports `durable: false` on every
write rather than claiming a guarantee it is not providing.

**Eviction is local, so the two holders of an object can disagree about
keeping it.** One evicts, the other does not; repair sees a missing replica and
recreates it. Under sustained quota pressure, a node above its high-water mark
declines repair writes with 507 so the two subsystems stop fighting — which
means **an object can sit at one replica while the cluster is full**. That is a
deliberate choice: keeping more distinct objects at one copy beats keeping fewer
at two, for a cache whose source of truth is elsewhere. It is visible in
`kilncache_repair_declined_over_quota_total`.

**Membership is static.** Adding or removing a node means editing the peer list
and restarting. If two nodes are given different peer lists, they will disagree
about placement: reads will miss more often and writes will place copies
somewhere the other node does not look. The forwarding bound keeps that from
becoming a loop, and nothing else protects against it.

**There is no authentication, authorisation, or transport security.** The trust
boundary is the cluster. A client that can reach a node can read and write
anything. Do not expose a node to an untrusted network.

**Byzantine peers are out of scope.** A peer that lies about a digest is not
defended against.

## Operational failure modes

**A full disk that eviction cannot fix.** If the quota exceeds the real free
space, writes fail with the filesystem's error. `MaxBytes` counts object bytes
only — filesystem overhead, the index database and its WAL are not included. The
[runbook](runbook.md) says to leave headroom.

**A corrupt index.** Delete `index.db*` and restart. Reconciliation rebuilds it
from the disk; only access history is lost, which shifts eviction order.

**A node with the wrong clock.** Access times are recorded from the local clock,
so a badly skewed node evicts in the wrong order. Nothing else depends on time.

**A slow disk.** Eviction and the publish path both go through `fsync`. If
eviction falls below the ingest rate the quota stops being enforced — this
happened, and `TestEvictionThroughput` now fails if eviction drops below half
the measured ingest rate.
