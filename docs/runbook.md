# Runbook

For whoever is holding this at 3 a.m.

## First: is it actually KilnCache?

```bash
for p in 8080 8081 8082; do
  echo "== :$p"
  curl -s --max-time 3 localhost:$p/readyz || echo "  unreachable"
done
```

A node that is unreachable, or reports `not ready`, is the problem. A cluster
where all three say `ready` and Bazel is still slow is probably not the cache —
check whether Bazel is actually configured to use it
(`--remote_cache=http://...`, and `--remote_upload_local_results` not set to
false).

## The three numbers that explain most incidents

```bash
curl -s localhost:8080/stats | jq '{usage, coordinator, repair, eviction}'
```

| Symptom | Look at | Meaning |
|---|---|---|
| Disk filling | `usage.bytes` vs `usage.high_water_bytes`, `eviction.evicted` | If usage is above the high-water mark and `evicted` is not rising, eviction is stuck or too slow. |
| Writes failing | `coordinator.insufficient_acks`, `coordinator.replica_failures` | A holder is unreachable. Find which node is down. |
| Reads missing | `coordinator.misses` vs `coordinator.local_hits` | A genuine cold cache looks like this. So does a cluster whose nodes were given different peer lists. |
| Repair busy forever | `repair.declined_over_quota` rising | The cluster is over quota and repair is deferring to eviction, by design. Objects can sit at one replica until there is room. |
| Corruption | `storage.verify_failures` | **Should always be zero.** Anything else means a local object no longer matches its key. |

## Symptoms

### A node is unhealthy or crash-looping

```bash
docker compose -p kilncache -f deploy/compose/docker-compose.yml logs --tail=50 node-a
```

The node prints one JSON line per event. Startup failures are the line before it
exits, and they are deliberately explicit:

- `open store: ... permission denied` — the data volume is not writable by the
  container's non-root user. This is what happens if the image is built without
  the `COPY --chown` that gives `/var/lib/kilncache` to UID 65532.
- `config: ...` — a bad flag or environment variable. The message names the
  setting.
- `register metrics: ...` — two metrics registered with the same name and
  different help strings. A code bug, and it fails at startup rather than at
  the first scrape, which is why you are seeing it now.

### The disk is full

```bash
curl -s localhost:8080/stats | jq '.usage'
```

If `bytes` is well above `high_water_bytes`, eviction is not keeping up.

1. **Check it is running at all**: `eviction.sweeps` should be rising.
2. **Check it is not failing**: `eviction.failures`.
3. **Lower the quota temporarily** and restart the node —
   `KILNCACHE_MAX_BYTES=8GiB`. Eviction sweeps once at startup, so this drains
   immediately.

Note that **`MaxBytes` counts object bytes only.** Filesystem overhead, the
SQLite index and its WAL are not included. Set the quota to about 85% of the
real free space, not 100%.

### Reads are missing objects that should be there

Ask each node directly, as a peer would, so the answer comes from its own disk
rather than a fallback:

```bash
KEY=<sha256>
for p in 8080 8081 8082; do
  printf ":%s " "$p"
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "X-Kilncache-Forwarded-By: runbook" \
    -H "X-Kilncache-Hop: read" \
    "localhost:$p/cas/$KEY"
done
```

- **Two 200s**: the object is fine; the problem is elsewhere.
- **One 200**: under-replicated. Repair will fix it unless the cluster is over
  quota — check `repair.declined_over_quota`.
- **No 200s**: it was evicted, or never stored. Check `eviction.evicted`.

### Every read returns 503

That is "no holder could serve this object", which is deliberately *not* a 404 —
a partition must not look like a cold cache, or Bazel rebuilds everything
instead of reporting a problem.

Check which nodes can see each other:

```bash
docker compose -p kilncache -f deploy/compose/docker-compose.yml ps
scripts/netem.sh show    # is there a leftover qdisc from a benchmark?
```

`scripts/netem.sh clear` removes injected network conditions. A forgotten
`netem apply` from a benchmark run looks exactly like a failing network.

### A node lost its data

Bring it back empty. Reconciliation finds nothing, the node reports ready, and
repair refills it from the other holders:

```bash
docker compose -p kilncache -f deploy/compose/docker-compose.yml stop node-b
docker volume rm kilncache_data-b
docker compose -p kilncache -f deploy/compose/docker-compose.yml up -d node-b
```

Watch `repair.repaired` on the *other* nodes rise: they are the ones that notice
node-b is missing copies. Refill rate is bounded by the repair queue and worker
count (`--repair-workers`, `--repair-queue`).

### The index is corrupt

It is derived state. Delete it and restart:

```bash
docker compose -p kilncache -f deploy/compose/docker-compose.yml stop node-a
docker run --rm -v kilncache_data-a:/d alpine sh -c 'rm -f /d/index.db*'
docker compose -p kilncache -f deploy/compose/docker-compose.yml up -d node-a
```

Startup reconciliation rebuilds it by walking the object tree. Access history is
lost, so the first eviction after this picks worse victims than it otherwise
would. Nothing else is affected.

### `verify_failures` is non-zero

A local object no longer hashes to its key. The node refused to serve it, which
is correct. Find it in the logs:

```bash
docker compose -p kilncache -f deploy/compose/docker-compose.yml logs node-a \
  | grep 'failed local verification'
```

Delete the file and let repair fetch a good copy from the other holder. If this
is happening repeatedly on one node, suspect its disk.

## Things that look like incidents and are not

- **503s on writes while a node is restarting.** Correct behaviour: the PUT
  could not place both copies, so it said so instead of lying.
- **A non-zero error rate during a chaos run.** Also correct. A chaos run with
  *zero* client errors means the faults never landed, which the tool reports as
  INCONCLUSIVE rather than as a pass.
- **`repair_declined_over_quota` rising.** The cluster is full and repair is
  deferring to eviction. Without this the two would fight and the quota would
  not be enforced at all.
- **Cache misses after a deploy.** Placement depends on node *names*. If a name
  changed, every key that node owned moved.

## Turning things off

| Situation | Switch |
|---|---|
| Repair is saturating the network | `--repair-workers=0` |
| Read verification is too expensive | `--verify-reads=false` — measure first; the cost is in [perf-notes](perf-notes.md) |
| Eviction is too aggressive | Raise `--max-bytes`, or widen the gap between `--high-water` and `--low-water` |
| pprof should not be exposed | `--dev=false` (the default) |

## Getting evidence before you change anything

```bash
curl -s localhost:8080/stats  > /tmp/stats-a.json
curl -s localhost:8080/metrics > /tmp/metrics-a.txt
docker compose -p kilncache -f deploy/compose/docker-compose.yml logs --tail=500 > /tmp/logs.txt
```

With `--dev` on, a CPU profile of the problem as it happens:

```bash
go tool pprof -top "http://localhost:8080/debug/pprof/profile?seconds=30"
```

Take it *while* the problem is happening. A profile of an idle process is a
flamegraph of the scheduler.
