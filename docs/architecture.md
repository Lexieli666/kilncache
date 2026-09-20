# Architecture

KilnCache is three processes that each hold part of a content-addressed object
store, agree on who holds what without talking to each other, and keep serving
while one of them is gone.

```
                    Bazel  --remote_cache=http://node-a:8080
                                      |
        +-----------------------------+-----------------------------+
        |                             |                             |
     node-a                        node-b                        node-c
   ┌──────────────┐             ┌──────────────┐             ┌──────────────┐
   │ HTTP front   │             │ HTTP front   │             │ HTTP front   │
   │   door       │             │   door       │             │   door       │
   ├──────────────┤             ├──────────────┤             ├──────────────┤
   │ coordinator  │◄───────────►│ coordinator  │◄───────────►│ coordinator  │
   │ rendezvous(key) → ordered list of holders, same on every node            │
   ├──────────────┤             ├──────────────┤             ├──────────────┤
   │ object store │             │ object store │             │ object store │
   │  cas/xx/yy/  │             │  cas/xx/yy/  │             │  cas/xx/yy/  │
   │  ac/xx/yy/   │             │  ac/xx/yy/   │             │  ac/xx/yy/   │
   ├──────────────┤             ├──────────────┤             ├──────────────┤
   │ SQLite index │             │ SQLite index │             │ SQLite index │
   │ evictor      │             │ evictor      │             │ evictor      │
   │ repair       │             │ repair       │             │ repair       │
   └──────────────┘             └──────────────┘             └──────────────┘
```

Any node is a valid front door. There is no leader, no coordinator election and
no shared metadata store — see [ADR-0002](adr/0002-no-consensus.md) for why that
is defensible here and exactly where it is weaker than a consensus design.

## The packages, and why each exists

| Package | Responsibility |
|---|---|
| `internal/config` | Flags and `KILNCACHE_*` environment, with documented precedence and validation that refuses a nonsense configuration at startup rather than at eviction time. |
| `internal/storage` | The object tree, the durable publish path, the SQLite metadata index, quota and eviction. |
| `internal/cluster` | Rendezvous placement, the peer client, and the coordinator that implements replication and read fallback. |
| `internal/protocol` | The contract between the HTTP layer and the cluster layer: headers, hop roles, the `Backend` interface. It exists so the dependency between `httpapi` and `cluster` points one way. |
| `internal/httpapi` | Bazel's HTTP cache protocol, health, metrics, `/stats`, pprof, and the middleware stack. |
| `internal/repair` | The bounded background worker pool that recreates missing replicas. |
| `internal/metrics` | Prometheus collectors, which read the counters the subsystems already keep rather than duplicating them. |
| `internal/node` | One definition of what a node *is*, shared by `cmd/kilncache` and the integration tests, so the tests exercise the arrangement that ships. |

## The write path

A `PUT /cas/<sha256>` takes at most two network hops, and the bound is
structural rather than a retry limit.

```
client ──► front door
             │
             ├─ is this node one of the holders?
             │    yes → act as coordinator
             │    no  → proxy the body to holders[0] as hop=coordinator, store nothing
             │
             └─ coordinator:
                  1. stream body to <root>/tmp/incoming-XXXX, hashing as it goes
                  2. CAS: reject if the digest does not match the key
                  3. fsync(file)
                  4. rename into cas/xx/yy/<key>      ← atomic for readers
                  5. fsync(parent directory)          ← the step usually missing
                  6. replicate to the other holders concurrently, as hop=replica
                  7. ACK only when replica_count copies exist; otherwise 503
```

Step 5 is what [ADR-0003](adr/0003-durability.md) is about: a rename is atomic
the moment it returns, but the directory entry it creates is not *durable* until
the directory's own metadata reaches stable storage. Without it, a power loss
can leave an object whose data is on disk and whose name is gone, while the
client, the counters and the index all believe it exists.

Step 7 is what [ADR-0005](adr/0005-synchronous-replication.md) is about: a PUT
that returns 200 with one of two copies written has not replicated the object,
it has replicated the *belief* that it did.

A non-holder front door stores nothing. Staging a local copy would write,
replicate from, and then abandon a copy nobody asked for — on a 2 GiB build,
two gigabytes of pointless writes on whichever node the client happened to
address.

## The read path

```
GET /cas/<sha256>
  │
  ├─ local copy?           → serve it (verified on read, if enabled)
  ├─ forwarded request?    → stop here: 404. A forwarded read never forwards again.
  └─ otherwise             → try each holder in preference order, then the rest
                              │
                              ├─ a holder has it   → stream it through
                              ├─ all reachable, none has it → 404 (a genuine miss)
                              └─ none reachable    → 503, never 404
```

Local first even when this node is not a holder: placement is advisory, not
authoritative. An object can be here because the membership changed, or because
repair put it here, and serving it costs nothing and skips a hop.

The last line matters more than it looks. A partition must not look like a cold
cache — if it did, Bazel would rebuild everything rather than report a problem.

## Placement

`weight(key, node) = splitmix64_finalizer(keySeed(key) XOR nodeSeed(node))`,
sorted descending. Element 0 is the primary, element 1 the replica, and the rest
are the fallbacks a read tries. Every node computes it identically from the
static peer list, so there is nothing to agree about.

Measured over 50,000 keys: load imbalance 1.0127 max/mean on three nodes, and
25.26% of primaries move when a fourth node joins against a theoretical 25%.
Modulo hashing would move about 75%. See
[ADR-0004](adr/0004-rendezvous-hashing.md) and
[`bench/results/2026-09-20-yutongzhao/placement.json`](../bench/results/2026-09-20-yutongzhao/placement.json).

## Forwarding is bounded by a state machine, not a counter

```
HopClient ──► HopCoordinator ──► HopReplica   (terminal)
    │                        └──► HopRepair   (terminal)
    └──────────────────────────► HopRead      (terminal)
```

The only outgoing edge from `HopCoordinator` leads to a terminal state, so a
client request produces a chain of at most three nodes no matter how badly two
nodes disagree about placement — and they can disagree, because a node started
with a stale peer list computes different owners.

A hop is only trusted when the request also carries `X-Kilncache-Forwarded-By`,
so a client cannot set a header and talk a node out of replicating. An
unrecognised hop from a newer peer is treated as terminal, so a rolling upgrade
cannot open a loop.

`HopRepair` is terminal like `HopReplica` and differs in one way: a node above
its high-water mark declines it with 507. That exists because eviction and
repair are otherwise capable of fighting each other indefinitely — see
[docs/bugs.md](bugs.md) entry 7, where they did.

## Storage layout

```
<data-dir>/
  cas/ab/cd/abcd…          objects, content-addressed
  ac/ab/cd/abcd…           action results, not content-addressed
  tmp/incoming-XXXXXXX     uploads in progress
  index.db                 SQLite metadata (WAL)
```

Two levels of one hex byte gives 65,536 leaf directories, created lazily. A flat
directory with a million entries makes lookups linear on some filesystems and
`ls` unusable during an incident on all of them.

Temp files carry an `incoming-` prefix, which is not valid hex, so no GET can
ever resolve to one even by accident. That is structural rather than a check
someone can forget to make.

## Background work

Three goroutines per node, all stopped in order on shutdown:

- **Access-time flusher** (every 5 s). Recording a read is a database write;
  doing it inline would make a cache hit slower than a miss. Reads accumulate in
  memory and are flushed in one transaction. A crash loses a little recency
  information, which shifts eviction order slightly and is not a correctness
  property.
- **Evictor** (woken by writes, plus a 30 s ticker). Drains from the high-water
  mark to the low-water mark using a size-weighted min-heap over the coldest
  candidates. Never runs inline on the write path, because that would make an
  unlucky PUT's latency depend on how full the disk happens to be.
- **Repair auditor and workers.** Pages through the index, asks the other
  holders whether they have each object this node should hold, and re-sends what
  is missing. The queue is bounded; when it is full the pass *stops* rather than
  skipping, so the next pass resumes exactly there.

Shutdown order is: stop accepting and drain in-flight requests, then stop the
background workers. The reverse would abort a transfer a request is waiting on.

## What runs where

`deploy/compose/docker-compose.yml` starts three nodes on Docker **named
volumes**, never bind mounts. On a Windows host with the repository on `/mnt/d`,
a bind mount goes through a translation layer where `fsync` and `rename` do not
have the semantics this store depends on. Neither the containers nor the network
are given fixed names, so a test cluster and a developer's cluster can run side
by side.

The runtime image is `distroless/static:nonroot`: no libc, no shell, no package
manager. That is why the SQLite driver is the pure-Go one, and why the health
check is the binary probing itself rather than `curl`.
