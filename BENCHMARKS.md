# Benchmarks

**Generated from `bench/results/2026-09-20-yutongzhao/bench-latest.json` by `bench -render`. Do not edit by hand.**

Every number here comes from a committed raw result file. Regenerate with:

```bash
make bench       # runs the matrix and writes the JSON
make benchmarks  # regenerates this file from it
```

Measured 2026-09-20T10:10:06Z from git commit `e7782fc00f35`.

## How to read these numbers

- Each cell is the **median of 5 runs**, not the best. The spread column shows the
  range across those runs, so you can see whether the machine was quiet.
- Each run measures for 15 s after a 5 s unmeasured warmup.
- The load generator is **closed-loop**: each of N workers issues its next request
  as soon as the previous one returns. The concurrency column is therefore "N
  requests in flight", which is the question an operator has, and the latency
  figures are not subject to coordinated omission.
- Percentiles are nearest-rank over **every** observation, not a sample. An
  interpolated p99 is a value that was never observed.
- **The client shared a host with the servers.** There is no physical network in
  these numbers; they bound what the server can do, not what a remote client sees.

## The machine

| | |
|---|---|
| CPU | Intel(R) Core(TM) i9-14900KF |
| Cores | 32 |
| RAM | 31.2 GiB |
| Kernel | Linux 6.6.114.1-microsoft-standard-WSL2 |
| Virtualisation | wsl2 |
| Filesystem | 9p |
| Disk model | unknown |
| Go | go version go1.23.12 linux/amd64 |
| Docker | 29.1.3 |
| Nodes | 3, replica count 2 |
| KilnCache | dev (e7782fc00f35) |
| Commit | `e7782fc00f35` |

Full host record: [`bench/results/2026-09-20-yutongzhao/hostinfo.json`](bench/results/2026-09-20-yutongzhao/hostinfo.json).

This is a WSL2 virtual machine, not bare metal. Its virtual disk is backed by the
Windows host's own page cache, so read figures here are better than the same code
would see on a physical NVMe device, and write figures that go through `fsync` are
worse. The [device baseline](bench/results/2026-09-20-yutongzhao/device-baseline.json) is measured on the same
filesystem and is the floor every figure below should be read against.

## Results

### `get-64k`

64 KiB cache hits: the common case, a Bazel action fetching a small output.

Corpus: 4000 objects of 64 KiB (250 MiB total), seed `2.026092e+07`, pseudo-random content.

> WARM: the 250 MiB corpus is 0.8% of the host's 31.2 GiB of RAM, so it can be served entirely from the page cache. These are warm-cache numbers and do not measure the disk. The cold-path floor is the fio device baseline in device-baseline.json.

Source: [`bench/results/2026-09-20-yutongzhao/bench-latest.json`](bench/results/2026-09-20-yutongzhao/bench-latest.json)

| Concurrency | ops/s (median) | ops/s range | MiB/s | p50 ms | p95 ms | p99 ms | p99.9 ms | errors |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 4 | 10104 | 9676–10830 | 631.5 | 0.36 | 0.63 | 1.10 | 2.39 | 0 |
| 16 | 26986 | 26849–27213 | 1686.6 | 0.53 | 1.05 | 1.79 | 2.96 | 0 |
| 64 | 46058 | 45894–46263 | 2878.6 | 1.14 | 3.25 | 5.13 | 7.32 | 0 |
| 256 | 61041 | 58872–61644 | 3815.1 | 3.23 | 11.20 | 16.23 | 22.77 | 0 |

### `get-8m`

8 MiB cache hits: large object throughput, a static library or a debug binary.

Corpus: 120 objects of 8 MiB (960 MiB total), seed `2.026092e+07`, pseudo-random content.

> WARM: the 960 MiB corpus is 3.0% of the host's 31.2 GiB of RAM, so it can be served entirely from the page cache. These are warm-cache numbers and do not measure the disk. The cold-path floor is the fio device baseline in device-baseline.json.

Source: [`bench/results/2026-09-20-yutongzhao/bench-latest.json`](bench/results/2026-09-20-yutongzhao/bench-latest.json)

| Concurrency | ops/s (median) | ops/s range | MiB/s | p50 ms | p95 ms | p99 ms | p99.9 ms | errors |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 4 | 666 | 665–667 | 5324.3 | 5.79 | 7.60 | 8.90 | 10.91 | 0 |
| 16 | 1474 | 1463–1479 | 11792.4 | 10.29 | 16.30 | 19.93 | 24.87 | 0 |
| 64 | 1830 | 1821–1833 | 14636.7 | 32.92 | 59.23 | 75.17 | 95.55 | 0 |
| 256 | 1821 | 1796–1827 | 14565.7 | 111.24 | 348.41 | 531.37 | 908.21 | 0 |

### `put-replicated`

1 MiB writes with replica count 2: every PUT places a second copy before it returns.

Corpus: 2000 objects of 1 MiB (2.0 GiB total), seed `2.026092e+07`, pseudo-random content.

> WARM: the 2.0 GiB corpus is 6.3% of the host's 31.2 GiB of RAM, so it can be served entirely from the page cache. These are warm-cache numbers and do not measure the disk. The cold-path floor is the fio device baseline in device-baseline.json.

Source: [`bench/results/2026-09-20-yutongzhao/bench-latest.json`](bench/results/2026-09-20-yutongzhao/bench-latest.json)

| Concurrency | ops/s (median) | ops/s range | MiB/s | p50 ms | p95 ms | p99 ms | p99.9 ms | errors |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 4 | 93 | 88–172 | 93.0 | 41.59 | 54.44 | 65.15 | 76.94 | 0 |
| 16 | 509 | 242–628 | 509.2 | 3.64 | 82.15 | 102.62 | 126.40 | 0 |
| 64 | 1701 | 1273–1863 | 1701.0 | 7.86 | 157.12 | 203.37 | 255.92 | 0 |
| 256 | 1622 | 1526–1842 | 1621.5 | 21.11 | 715.90 | 1144.85 | 1740.96 | 0 |

### `mixed-80-20`

64 KiB reads, 80% hits and 20% misses: the shape of a partially warm cache.

Corpus: 4000 objects of 64 KiB (250 MiB total), seed `2.026092e+07`, pseudo-random content.

> WARM: the 250 MiB corpus is 0.8% of the host's 31.2 GiB of RAM, so it can be served entirely from the page cache. These are warm-cache numbers and do not measure the disk. The cold-path floor is the fio device baseline in device-baseline.json.

Source: [`bench/results/2026-09-20-yutongzhao/bench-latest.json`](bench/results/2026-09-20-yutongzhao/bench-latest.json)

| Concurrency | ops/s (median) | ops/s range | MiB/s | p50 ms | p95 ms | p99 ms | p99.9 ms | errors |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 4 | 8644 | 7729–8724 | 432.7 | 0.42 | 0.76 | 1.31 | 2.56 | 0 |
| 16 | 24010 | 23833–24866 | 1200.3 | 0.59 | 1.21 | 2.02 | 3.21 | 0 |
| 64 | 43151 | 41484–44409 | 2157.6 | 1.22 | 3.44 | 5.47 | 8.08 | 0 |
| 256 | 57299 | 56333–57630 | 2863.5 | 3.44 | 11.95 | 17.43 | 23.98 | 0 |

### `get-64k-verified`

64 KiB hits with every response body re-hashed by the client. The cost of proving correctness.

Corpus: 4000 objects of 64 KiB (250 MiB total), seed `2.026092e+07`, pseudo-random content.

> WARM: the 250 MiB corpus is 0.8% of the host's 31.2 GiB of RAM, so it can be served entirely from the page cache. These are warm-cache numbers and do not measure the disk. The cold-path floor is the fio device baseline in device-baseline.json.

Source: [`bench/results/2026-09-20-yutongzhao/bench-latest.json`](bench/results/2026-09-20-yutongzhao/bench-latest.json)

| Concurrency | ops/s (median) | ops/s range | MiB/s | p50 ms | p95 ms | p99 ms | p99.9 ms | errors |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 4 | 9074 | 8747–9159 | 567.1 | 0.40 | 0.67 | 1.11 | 2.44 | 0 |
| 16 | 25142 | 24687–25425 | 1571.3 | 0.57 | 1.09 | 1.86 | 3.13 | 0 |
| 64 | 41877 | 41007–42308 | 2617.3 | 1.27 | 3.44 | 5.40 | 8.26 | 0 |
| 256 | 53901 | 52840–54118 | 3368.8 | 3.71 | 12.44 | 18.14 | 24.89 | 0 |

## Memory does not scale with object size

Objects are streamed through a fixed 256 KiB buffer, so resident memory is a
function of concurrency rather than of object size. Peak RSS across all nodes,
sampled twice a second while each scenario ran:

Source: [`bench/results/2026-09-20-yutongzhao/bench-latest.json`](bench/results/2026-09-20-yutongzhao/bench-latest.json), the `memory` field of each scenario

| Scenario | Object size | Peak RSS (max across nodes) |
|---|---:|---:|
| `get-64k` | 64 KiB | 101.9 MiB |
| `mixed-80-20` | 64 KiB | 129.0 MiB |
| `get-64k-verified` | 64 KiB | 126.8 MiB |
| `put-replicated` | 1 MiB | 139.5 MiB |
| `get-8m` | 8 MiB | 138.3 MiB |

Object size grew 128x between the smallest and largest scenario; peak RSS changed
by 36%. A server that buffered whole objects would show the first ratio in the
second column.

## Device baseline

What the filesystem under the cache can do, measured with `fio` on `/home/yzhao/.cache/kilncache-baseline` (ext4).
Every figure above should be read against these.

Source: [`bench/results/2026-09-20-yutongzhao/device-baseline.json`](bench/results/2026-09-20-yutongzhao/device-baseline.json), full fio output in `device-baseline.fio.json`

| fio job | Read | Write |
|---|---:|---:|
| `randwrite_4k_qd1_fsync` | — | 4 MiB/s @ 915 IOPS |
| `randread_4k_qd32` | 3747 MiB/s @ 960643 IOPS | — |
| `seqwrite_1m_qd8` | — | 5672 MiB/s @ 5785 IOPS |

The first job is the one that matters for writes: 4 KiB random writes with
`fdatasync` after each, which is what the publish path costs per object.

## Behaviour under failure

A 10-minute chaos run with 12 concurrent clients and 10 node stops.

Source: [`bench/results/2026-09-20-yutongzhao/chaos-latest.json`](bench/results/2026-09-20-yutongzhao/chaos-latest.json)

| | |
|---|---|
| Corrupted reads | **0** out of 304325 reads verified |
| Convergence to 2 replicas | true, after 217 s, over 3000 sampled objects |
| Throughput during the run | 1020 ops/s |
| Client error rate while degraded | 22.6% |
| Verdict | PASS |

Every read was checked against a digest the chaos runner computed itself, never
one the cluster reported. The error rate is non-zero because the faults landed:
a chaos run with no client errors would mean nothing was actually broken, and
the tool reports that as INCONCLUSIVE rather than as a pass.

## Placement

Rendezvous hashing over 50000 keys.

Source: [`bench/results/2026-09-20-yutongzhao/placement.json`](bench/results/2026-09-20-yutongzhao/placement.json)

| Cluster | Primary imbalance (max/mean) | Holder imbalance |
|---:|---:|---:|
| 3 nodes | 1.0127 | 1.0076 |
| 4 nodes | 1.0102 | 1.0086 |
| 5 nodes | 1.0162 | 1.0062 |
| 8 nodes | 1.0208 | 1.0142 |

Source: [`bench/results/2026-09-20-yutongzhao/placement.json`](bench/results/2026-09-20-yutongzhao/placement.json)

| Membership change | Primaries moved | Theory |
|---|---:|---:|
| 3 → 4 nodes | 25.26% | 25.00% |
| 4 → 5 nodes | 19.62% | 20.00% |
| 5 → 6 nodes | 16.44% | 16.67% |

Modulo hashing would move about 75% of keys when the fourth node joins. That
single comparison is why this project does not use it.

## The same code over a slow, lossy network

Source: [`bench/results/2026-09-20-yutongzhao/netem/bench-latest.json`](bench/results/2026-09-20-yutongzhao/netem/bench-latest.json)
Conditions: tc netem on each node's eth0: delay 20ms +/- 5ms normal, loss 0.5%. Measured inter-node RTT 42-45 ms, against 0.02 ms without it. NOTE: the client reaches the nodes through the same interface, so client traffic is delayed and dropped too -- this measures the whole system under a slow lossy network, not the inter-node hop in isolation.

| Scenario | Concurrency | ops/s | p50 ms | p99 ms | errors |
|---|---:|---:|---:|---:|---:|
| `get-64k` | 16 | 48 | 331.49 | 517.27 | 0 |
| `get-64k` | 64 | 197 | 326.31 | 484.20 | 0 |
| `put-replicated` | 16 | 9 | 1713.88 | 4129.95 | 0 |
| `put-replicated` | 64 | 39 | 1504.51 | 3782.81 | 0 |

Throughput collapses and latency is dominated by the round trip, which is what
should happen: a cache on a slow network is a slow cache. What matters is that
the error count stays at zero — nothing was lost, corrupted, or refused; it was
only slow.

## What is not measured here

- **Cold-cache large-object reads.** Dropping the page cache needs root, which this
  host does not grant, so every read figure above is warm. The device baseline is
  the honest floor for the cold case.
- **A real network.** Client and servers share a host. `scripts/netem.sh` injects
  latency and loss between the containers, and any run made under it says so in
  its `netem` field.
- **Anything about other caches.** There is no comparison here, because a fair one
  would need the same workload, hardware and tuning effort for both.
