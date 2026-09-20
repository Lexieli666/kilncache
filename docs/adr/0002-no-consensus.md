# ADR-0002: No consensus layer; rely on content-addressed immutability

- Status: accepted
- Date: 2026-09-20

## Context

KilnCache replicates every object to two of three nodes and must keep serving
correct bytes while a node is down, restarting, or partitioned. That is the
shape of a problem people solve with Raft, and the obvious question from any
reviewer is why there is no consensus here.

Two forces:

1. **What the data is.** A CAS object's key *is* the SHA-256 of its bytes. There
   is exactly one valid value for a given key, for all time. Two nodes that
   independently accept a PUT for the same key cannot diverge, because a write
   that would have produced different bytes is rejected at ingest by the digest
   check. There is no "latest" version to agree on, so there is no ordering
   problem, so there is nothing for a consensus protocol to order.

2. **What the schedule is.** This project has roughly four focused weeks. A
   correct Raft implementation — leader election, log replication, snapshots,
   membership changes, and the tests that make any of that believable — is most
   of that budget. Spending it there would leave the parts a build-cache reviewer
   actually cares about (streaming without buffering, durability on the publish
   path, placement, eviction, measured performance) as sketches.

The action cache is the one place where this reasoning needs care. `/ac/<hash>`
entries are *not* content-addressed: the key is a hash of the action, and the
value is a result that can legitimately be rewritten (a non-deterministic action,
a rebuilt output). See Consequences.

## Decision

No consensus layer, no leader election, no dynamic membership. Membership is a
static list supplied identically to all three nodes. Placement is a pure
function (rendezvous hashing, ADR-0004) evaluated independently on every node,
which needs no coordination because it needs no shared state.

Correctness rests on three properties instead:

- **Immutability.** A CAS key maps to one byte string, permanently.
- **Verification on every boundary.** Bytes are hashed on ingest and rejected on
  mismatch; the repair worker re-verifies before it treats a local copy as a
  valid source. A node never returns bytes it has not verified match the key.
- **Idempotence.** A PUT of an object that already exists is a no-op. Replaying
  a replication request, retrying after a timeout, and repairing a missing
  replica are all the same operation, so retries need no deduplication and
  partial failure needs no rollback.

## Consequences

What this buys:

- No split-brain, because there is no state to split. Two nodes that disagree
  about whether an object exists resolve it by one of them fetching the object
  and verifying it.
- A partitioned node keeps serving reads of what it has. A cache that answers
  fewer questions is still useful; a cache that blocks on a quorum is not.
- The failure model is small enough to enumerate exhaustively
  (`docs/failure-model.md`).

What it gives up, stated plainly because a reviewer will ask:

- **No durability guarantee across correlated loss.** Two copies of three nodes.
  Lose both holders of an object and it is gone. For a build cache that is a
  cache miss, not data loss — the source of truth is the build graph.
- **No membership changes at runtime.** Adding a fourth node means rewriting the
  peer list and restarting the cluster. The property test measures what that
  costs (the fraction of keys that move); it does not make it online.
- **AC entries are not protected by content addressing.** A PUT to `/ac/<hash>`
  overwrites. Two clients writing different results for the same action key
  concurrently produce a last-writer-wins outcome, and the two nodes holding the
  copies can disagree about which write won. This is the one place where the
  design is genuinely weaker than a consensus-based cache. It is acceptable
  because an AC entry is a pointer to CAS objects that are themselves verified:
  a stale AC entry causes a wasted download and a cache miss, never a corrupt
  build output. `docs/failure-model.md` documents this explicitly as "what is
  not guaranteed", and the AC handler does not claim linearizability anywhere.
- **No global quota or coordinated eviction.** Each node evicts locally, so a
  key can be evicted from one holder and retained by the other. The repair
  worker treats that as a missing replica and re-creates it, which is correct
  but means eviction and repair can chase each other under a tight quota. The
  low/high water marks exist to give repair room to converge between eviction
  passes.

## Alternatives

- **Raft over the metadata index.** Would give a consistent view of which node
  holds what, and linearizable AC writes. Costs most of the schedule, and adds a
  quorum dependency to the read path — the opposite of what a build cache wants,
  where a degraded answer beats a blocked one.
- **Gossip membership (SWIM) instead of a static list.** Solves a problem this
  project does not have: three nodes whose addresses are known at deploy time.
  It would add failure-detector tuning and false-positive handling to the test
  matrix for no gain in hiring signal.
- **Single node with a big disk.** Genuinely simpler and, for many real teams,
  the right answer. It is rejected because the entire point of the project is to
  demonstrate distributed behaviour under failure; the README says so rather
  than pretending three nodes were operationally necessary.
