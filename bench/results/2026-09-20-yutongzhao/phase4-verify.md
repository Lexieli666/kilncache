# Phase 4 verification — 2026-09-20T11:20:14Z

Observability and benchmarks. Metrics and `/stats` landed in Phase 3, because
the chaos runner needed node counters to tell eviction from data loss; this
phase adds the benchmark driver, the generated BENCHMARKS.md, netem fault
injection, in-load profiling, and the two profiling wins in docs/perf-notes.md.

## go test ./...
```
ok  	github.com/Lexieli666/kilncache/cmd/bench	0.010s
ok  	github.com/Lexieli666/kilncache/cmd/chaos	0.009s
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	0.311s
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	0.004s
ok  	github.com/Lexieli666/kilncache/internal/cluster	0.506s
ok  	github.com/Lexieli666/kilncache/internal/config	0.004s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.061s
ok  	github.com/Lexieli666/kilncache/internal/logging	0.004s
ok  	github.com/Lexieli666/kilncache/internal/metrics	0.017s
ok  	github.com/Lexieli666/kilncache/internal/node	0.035s
ok  	github.com/Lexieli666/kilncache/internal/protocol	0.005s
ok  	github.com/Lexieli666/kilncache/internal/repair	4.052s
ok  	github.com/Lexieli666/kilncache/internal/storage	1.273s
```

## go test -race ./...
```
ok  	github.com/Lexieli666/kilncache/cmd/bench	1.024s
ok  	github.com/Lexieli666/kilncache/cmd/chaos	1.031s
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	1.327s
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	1.012s
ok  	github.com/Lexieli666/kilncache/internal/cluster	3.187s
ok  	github.com/Lexieli666/kilncache/internal/config	1.013s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	1.357s
ok  	github.com/Lexieli666/kilncache/internal/logging	1.014s
ok  	github.com/Lexieli666/kilncache/internal/metrics	1.080s
ok  	github.com/Lexieli666/kilncache/internal/node	1.149s
ok  	github.com/Lexieli666/kilncache/internal/protocol	1.014s
ok  	github.com/Lexieli666/kilncache/internal/repair	5.280s
ok  	github.com/Lexieli666/kilncache/internal/storage	19.803s
```

## go vet && golangci-lint
```
(both clean)
```

## metrics exposed by a live node
```
$ curl -s localhost:8080/metrics | grep -oP "^kilncache_\w+" | sort -u
kilncache_bytes_in_total
kilncache_bytes_out_total
kilncache_evicted_bytes_total
kilncache_eviction_failures_total
kilncache_eviction_sweeps_total
kilncache_evictions_total
kilncache_forwarded_requests_total
kilncache_high_water_bytes
kilncache_hits_total
kilncache_index_touches_dropped_total
kilncache_index_touches_written_total
kilncache_insufficient_replicas_total
kilncache_low_water_bytes
kilncache_misses_total
kilncache_proxied_puts_total
kilncache_puts_already_stored_total
kilncache_puts_rejected_total
kilncache_puts_total
kilncache_quota_bytes
kilncache_read_fallbacks_total
kilncache_repair_audited_total
kilncache_repair_bytes_total
kilncache_repair_completed_total
kilncache_repair_declined_over_quota_total
kilncache_repair_dropped_full_queue_total
kilncache_repair_failed_total
kilncache_repair_queue_capacity
kilncache_repair_queue_depth
kilncache_replication_attempts_total
kilncache_replication_failures_total
kilncache_request_duration_seconds_bucket
kilncache_request_duration_seconds_count
kilncache_request_duration_seconds_sum
kilncache_requests_in_flight
kilncache_requests_total
kilncache_stored_bytes
kilncache_stored_objects
kilncache_verify_failures_total
```

## pprof is reachable only in dev mode
```
--- PASS: TestPprofOffByDefault (0.00s)
--- PASS: TestPprofOnInDevMode (0.00s)
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.003s
```

## benchmark harness control run against three real nodes
```
    run_test.go:213: control run: 3 scenarios, 12 profiles, report bench-20260920-112053.json
--- PASS: TestBenchmarkAgainstRealNodes (58.83s)
ok  	github.com/Lexieli666/kilncache/cmd/bench	58.857s
```

## benchmark matrix: 5 scenarios x 4 concurrency levels x 5 runs each
```
  16          26986     1686.6      0.53      1.05      1.79    0.0000
  64          46058     2878.6      1.14      3.25      5.13    0.0000
  256         61041     3815.1      3.23     11.20     16.23    0.0000

get-8m  (8 MiB objects)
  conc        ops/s      MiB/s    p50 ms    p95 ms    p99 ms       err
  4             666     5324.3      5.79      7.60      8.90    0.0000
  16           1474    11792.4     10.29     16.30     19.93    0.0000
  64           1830    14636.7     32.92     59.23     75.17    0.0000
  256          1821    14565.7    111.24    348.41    531.37    0.0000

put-replicated  (1 MiB objects)
  conc        ops/s      MiB/s    p50 ms    p95 ms    p99 ms       err
  4              93       93.0     41.59     54.44     65.15    0.0000
  16            509      509.2      3.64     82.15    102.62    0.0000
  64           1701     1701.0      7.86    157.12    203.37    0.0000
  256          1622     1621.5     21.11    715.90   1144.85    0.0000

mixed-80-20  (64 KiB objects)
  conc        ops/s      MiB/s    p50 ms    p95 ms    p99 ms       err
  4            8644      432.7      0.42      0.76      1.31    0.0000
  16          24010     1200.3      0.59      1.21      2.02    0.0000
  64          43151     2157.6      1.22      3.44      5.47    0.0000
  256         57299     2863.5      3.44     11.95     17.43    0.0000

get-64k-verified  (64 KiB objects)
  conc        ops/s      MiB/s    p50 ms    p95 ms    p99 ms       err
  4            9074      567.1      0.40      0.67      1.11    0.0000
  16          25142     1571.3      0.57      1.09      1.86    0.0000
  64          41877     2617.3      1.27      3.44      5.40    0.0000
  256         53901     3368.8      3.71     12.44     18.14    0.0000

```

## memory does not scale with object size

Peak RSS across all three nodes, sampled twice a second for the duration of each
scenario, at concurrency up to 256:
```
  get-64k: objects 65536 B, peak RSS 101.9 MiB
  get-8m: objects 8388608 B, peak RSS 138.3 MiB
  put-replicated: objects 1048576 B, peak RSS 139.5 MiB
  mixed-80-20: objects 65536 B, peak RSS 129 MiB
  get-64k-verified: objects 65536 B, peak RSS 126.8 MiB
```

Object size varies 128x between `get-64k` and `get-8m`; peak RSS varies by about
a third. What remains scales with *concurrency* -- 256 in-flight transfers, each
holding a 256 KiB copy buffer, is about 64 MiB by itself -- which is the
behaviour a fixed-buffer streaming design should have. A server that buffered
whole objects would show 8 MiB x 256 here.

## CPU and heap profiles, taken during load
```
get-64k-node0-cpu.pprof
get-64k-node0-heap.pprof
get-64k-node1-cpu.pprof
get-64k-node1-heap.pprof
get-64k-node2-cpu.pprof
get-64k-node2-heap.pprof
get-64k-verified-node0-cpu.pprof
get-64k-verified-node0-heap.pprof
get-64k-verified-node1-cpu.pprof
get-64k-verified-node1-heap.pprof
get-64k-verified-node2-cpu.pprof
get-64k-verified-node2-heap.pprof
get-8m-node0-cpu.pprof
get-8m-node0-heap.pprof
get-8m-node1-cpu.pprof
get-8m-node1-heap.pprof
get-8m-node2-cpu.pprof
get-8m-node2-heap.pprof
mixed-80-20-node0-cpu.pprof
mixed-80-20-node0-heap.pprof
mixed-80-20-node1-cpu.pprof
mixed-80-20-node1-heap.pprof
mixed-80-20-node2-cpu.pprof
mixed-80-20-node2-heap.pprof
put-replicated-node0-cpu.pprof
put-replicated-node0-heap.pprof
put-replicated-node1-cpu.pprof
put-replicated-node1-heap.pprof
put-replicated-node2-cpu.pprof
put-replicated-node2-heap.pprof
```

## one scenario re-run under tc netem
```
$ scripts/netem.sh apply 20ms 5ms 0.5%
$ scripts/netem.sh verify
  node-b: rtt min/avg/max/mdev = 38.905/44.761/51.501/4.473 ms
  (without netem: 0.028/0.050/0.081/0.020 ms)

  get-64k c=16: 48 ops/s, p50 331.492 ms, p99 517.272 ms, errors 0
  get-64k c=64: 196 ops/s, p50 326.309 ms, p99 484.202 ms, errors 0
  put-replicated c=16: 8 ops/s, p50 1713.882 ms, p99 4129.954 ms, errors 0
  put-replicated c=64: 39 ops/s, p50 1504.511 ms, p99 3782.807 ms, errors 0
```

## BENCHMARKS.md is regenerated from JSON with no manual edits
```
$ make benchmarks && diff BENCHMARKS.md <(regenerate)
identical: the committed file is exactly what the generator produces
```

## no unmeasured numbers
```
check-numbers: OK - 24 numeric claim(s) across 16 file(s), each backed by an existing raw result
```

## test count and coverage
```
$ go test -count=1 -v ./... | grep -c "^=== RUN"
251
$ go test -tags=integration -count=1 -v ./tests/integration/ ./cmd/chaos/ ./cmd/bench/ | grep -c "^=== RUN"
63
$ make cover-all
total:									(statements)		82.5%
```
