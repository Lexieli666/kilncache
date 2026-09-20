# ADR-0004: Rendezvous hashing for placement, not a consistent-hash ring

- Status: accepted
- Date: 2026-09-20

## Context

Every node must be able to answer "which nodes should hold this key" and get the
same answer as every other node, without asking anyone. Two copies of each
object means the answer is not one node but an ordered list, and a read needs
the *rest* of that list too: when the primary is down, the reader has to know
who to try next, and it has to agree with the writer about it.

The default answer to this problem is a consistent-hash ring: hash nodes and
keys onto a circle, walk clockwise. It works, and it is what most systems use.

## Decision

Rendezvous hashing (highest random weight). For each key, compute a weight per
node and sort descending:

```
weight(key, node) = splitmix64_finalizer( keySeed(key) XOR nodeSeed(node) )
```

- `nodeSeed` is the first 8 bytes of `SHA-256("kilncache/member/" + name)`,
  computed once per node at startup. It makes placement depend on the node's
  *identity*, so reordering the `--peers` flag moves nothing.
- `keySeed` is the first 8 bytes of the key itself. Keys are already SHA-256
  digests in lowercase hex, so their bits are uniform; hashing them again would
  be pure cost on the hottest path in the system. Anything that does not look
  like a digest falls back to hashing, so placement is defined even for keys the
  store would reject — a lookup that panicked on bad input would turn a 404 into
  an outage.
- The finalizer is SplitMix64's: a bijection with full avalanche.
  `TestMixAvalanche` measures it at 32.00 of 64 bits changed per input bit flip,
  and fails if it drifts more than 2 bits from 32. Every balance property in
  this document rests on that, so it is asserted directly rather than only
  through statistics.
- Ties break by node name. A 64-bit collision is astronomically unlikely, but
  "astronomically unlikely" is not "impossible", and a tie resolved by map
  iteration order would make two nodes disagree about exactly one key — the
  hardest possible bug to find.
- Members are sorted by name at construction, so two nodes given the same peers
  in different orders build an identical ring. `TestOrderIndependence` is the
  falsifier: without it, a cluster whose operators wrote `--peers` differently
  would silently disagree about where everything lives, and half of all reads
  would become misses.

## Consequences

Measured over 50,000 keys
(`bench/results/2026-09-20-yutongzhao/placement.json`, regenerate with
`make placement-report`):

| Cluster | Primary imbalance (max/mean) | Holder imbalance |
|---|---|---|
| 3 nodes | 1.0127 | 1.0076 |
| 4 nodes | 1.0102 | 1.0086 |
| 5 nodes | 1.0162 | 1.0062 |
| 8 nodes | 1.0208 | 1.0142 |

| Membership change | Primaries moved | Theory | Holder sets changed |
|---|---|---|---|
| 3 → 4 nodes | 25.26% | 25.00% | 50.12% |
| 4 → 5 nodes | 19.62% | 20.00% | 40.07% |
| 5 → 6 nodes | 16.44% | 16.67% | 33.28% |

What this buys:

- **Balance without tuning.** A consistent-hash ring with one point per node is
  badly unbalanced; it needs 100–200 virtual nodes per physical node to get
  within a few percent, which is a parameter to choose, document, and get wrong.
  Rendezvous is within 2% of perfect with nothing to tune.
- **The preference list is free.** Sorting by weight produces the full ordered
  list of nodes, which is exactly what replica placement and read fallback both
  need. A ring gives the first node cheaply and the rest awkwardly.
- **Minimal disruption, structurally.** When a node joins, a key either stays
  where it was or moves to the newcomer — nothing shuffles between existing
  nodes. `TestPropAddingANodeOnlyMovesToTheNewNode` asserts that structurally
  rather than statistically: a hash could pass a crude "under 35% moved" check
  while churning the cluster, and this property would still catch it.
- **Removing a node moves only that node's keys.** `TestKeyMovementOnLoss`
  asserts exactly this, with no tolerance.

What it costs:

- **O(N) per lookup** instead of O(log N). For N=3 this is three integer
  multiplies and a three-element sort. It would matter at a thousand nodes; the
  README's non-goals say this is a three-node cluster, and a design that
  optimised for a scale it explicitly does not target would be the wrong call.
- **No data-locality tricks.** A ring lets you place related keys near each
  other. Content-addressed build artifacts have no meaningful relatedness, so
  there is nothing to exploit.

## Alternatives

- **Consistent hashing with virtual nodes.** The standard choice, and correct
  for a large or elastic cluster. Rejected because the virtual-node count is a
  tuning parameter that buys balance this design gets for free, and because
  extracting an ordered fallback list from a ring is fiddlier than sorting.
- **`hash(key) mod N`.** Would move about 75% of keys when the fourth node
  joins, against a measured 25.26% here. That single comparison is the reason
  neither this project nor any real one uses it.
- **A central placement service.** Reintroduces the coordination that ADR-0002
  went out of its way to avoid, and puts a network round trip in front of every
  read.
- **CRUSH-style hierarchical placement.** Solves rack and failure-domain
  awareness. There are no racks here; three containers on one host have exactly
  one failure domain, and pretending otherwise would be theatre.
