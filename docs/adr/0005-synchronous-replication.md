# ADR-0005: Acknowledge a PUT only after both copies exist

- Status: accepted
- Date: 2026-09-20

## Context

A PUT can be acknowledged at three different moments, and the choice determines
what the replication factor actually means:

1. **After the local write.** Fastest. The second copy is made in the
   background, or eventually by the repair worker.
2. **After the local write and the replica write.** Slowest by one network hop.
3. **After a quorum.** Meaningless with two copies of three nodes — a quorum of
   two *is* both copies.

Option 1 is what most caches do, and it is defensible for a cache: a lost object
is a cache miss, not data loss. But it makes the stated replication factor a
claim about intent rather than about state. A client that receives 200 has been
told the object is on two nodes. If the second write then fails and the node
holding the only copy dies before repair notices, the object is gone and nothing
ever recorded that anything went wrong.

That gap is also exactly what makes the Phase 2 and Phase 3 tests meaningful or
not. "Kill the primary and read through another node" only proves something if
the second copy was guaranteed to exist before the kill.

## Decision

Option 2. A PUT returns 2xx only when `replica_count` nodes hold the object. If
fewer copies could be written, the response is **503** with the count in
`X-Kilncache-Copies` and `X-Kilncache-Copies-Wanted`.

Mechanics:

- The **coordinator** — the first holder in the preference list — writes its own
  copy first, then replicates to the remaining holders concurrently, each
  reading from local disk independently. Concurrent rather than sequential
  because with a replica count of three a sequential fan-out would make latency
  the sum of every hop.
- A **front door that is not a holder** proxies the client's body to the
  coordinator and stores nothing itself. Staging a copy locally would write,
  replicate from, and then abandon a copy nobody asked for: on a 2 GiB build,
  two gigabytes of pointless writes on whichever node the client addressed.
- Local-first, rather than streaming to every holder at once through
  `io.Pipe`. Writing locally first verifies the digest and leaves a stable
  source to replicate from, so a peer that dies mid-transfer can be retried
  without the client's body — which has already been consumed. The pipe version
  would cut latency from `local + remote` to `max(local, remote)` and is the
  obvious next optimisation; `docs/perf-notes.md` measures the cost rather than
  assuming it.
- Replication is **idempotent**, because CAS objects are immutable. A retry, a
  duplicate, and a repair are the same operation, so partial failure needs no
  rollback and retries need no deduplication.

## Consequences

- PUT latency is one network hop plus one remote fsync longer than a local
  write. On the development host a 4 KiB write with `fdatasync` measured 914
  IOPS — about 1.1 ms
  (`bench/results/2026-09-20-yutongzhao/device-baseline.json`) — so for small
  objects the replica's fsync, not the network, dominates.
- Writes fail when a node is down. This is the deliberate trade and it is worth
  stating plainly: with replica count 2 over three nodes, roughly two thirds of
  keys have a holder on any given node, so losing one node makes about two
  thirds of *writes* fail while **reads continue from the surviving copy**. For
  a build cache that is the right way round — a failed upload costs one action's
  worth of re-upload later; a silently missing replica costs a rebuild at the
  worst possible moment.
- The replication factor is a statement about state, so the Phase 2 node-kill
  test and the Phase 3 chaos run mean what they say.
- **A peer that acknowledges a write it did not persist cannot be detected at
  write time.** `TestTruncatingPeerIsAcceptedThenDetectedByRepair` exists to
  keep this honest: the coordinator counts the ack, returns success, and the
  missing copy is the repair auditor's problem. This is a genuine hole in
  synchronous replication, not something the ack protocol closes.

## Falsifiers

- `TestCoordinatePlacesBothCopies` — both copies exist when PUT returns.
- `TestReplicaWriteFailureIsA503Path` — one copy written of two required gives
  `ErrInsufficientReplicas`, and the outcome reports 1 of 2.
- `TestReplicaRejectionIsNotRetriedElsewhere` — a 400 from a peer is the
  object's fault, not the peer's, and is not offered to anyone else.
- `TestPutFromNonOwnerIsProxied` — the front door stores nothing it does not own.
- `TestCoordinatorOnlyIssuesReplicaWrites` — the forwarding chain is bounded.
- `TestSlowPeerHitsTheHopTimeout` — a hung peer does not hold a client open.
- The three-node integration suite kills a container holding a sampled object
  and verifies the bytes read through another node, byte for byte.

## Alternatives

- **Asynchronous replication with a durable intent log.** Write locally, log the
  intent, replicate in the background, retry from the log. Genuinely better for
  write latency, and a real system with this traffic should probably do it. It
  needs a durable queue, a retry policy, and a way to tell a pending replication
  from a failed one — a subsystem in its own right, and the schedule (ADR-0002)
  did not have room for one with tests that would make it believable.
- **Fire and forget, rely on repair.** Cheapest, and makes the replication
  factor an aspiration. Rejected for the reason above: it would not change what
  the cache *does* much, but it would make every claim in this repository about
  replication unfalsifiable.
- **Write to all three nodes.** More durable, one more hop, and at three nodes
  it means the placement function has nothing to decide. The project is about
  placement and partial failure; replicating everywhere removes the subject.
