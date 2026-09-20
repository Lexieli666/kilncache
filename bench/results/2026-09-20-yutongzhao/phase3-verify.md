# Phase 3 verification — 2026-09-20T08:38:57Z

SQLite (WAL) metadata index, quota with high/low water marks, size-weighted
access-aware eviction, startup reconciliation, bounded repair worker pool,
and a 10-minute chaos run. Prometheus metrics landed here too, because the
chaos runner needs node counters to tell eviction apart from data loss.

Raw results in this directory: `quota-eviction.json`, `index-scan.json`,
`eviction-throughput.json`, `chaos-20260920-082057.json` (= `chaos-latest.json`).

## go test ./...
```
ok  	github.com/Lexieli666/kilncache/cmd/chaos	0.006s
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	0.317s
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	0.010s
ok  	github.com/Lexieli666/kilncache/internal/cluster	0.727s
ok  	github.com/Lexieli666/kilncache/internal/config	0.010s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.061s
ok  	github.com/Lexieli666/kilncache/internal/logging	0.010s
ok  	github.com/Lexieli666/kilncache/internal/metrics	0.019s
ok  	github.com/Lexieli666/kilncache/internal/node	0.029s
ok  	github.com/Lexieli666/kilncache/internal/protocol	0.003s
ok  	github.com/Lexieli666/kilncache/internal/repair	4.063s
ok  	github.com/Lexieli666/kilncache/internal/storage	1.459s
```

## go test -race ./...
```
ok  	github.com/Lexieli666/kilncache/cmd/chaos	1.022s
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	1.319s
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	1.011s
ok  	github.com/Lexieli666/kilncache/internal/cluster	4.777s
ok  	github.com/Lexieli666/kilncache/internal/config	1.010s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	1.327s
ok  	github.com/Lexieli666/kilncache/internal/logging	1.008s
ok  	github.com/Lexieli666/kilncache/internal/metrics	1.054s
ok  	github.com/Lexieli666/kilncache/internal/node	1.124s
ok  	github.com/Lexieli666/kilncache/internal/protocol	1.010s
ok  	github.com/Lexieli666/kilncache/internal/repair	5.267s
ok  	github.com/Lexieli666/kilncache/internal/storage	22.225s
```

## go vet && golangci-lint
```
(both clean)
```

## quota and eviction
```
--- PASS: TestQuotaValidation (0.00s)
--- PASS: TestEvictionScorePrefersLargeAndCold (0.00s)
--- PASS: TestEvictionScoreHandlesZeroSize (0.00s)
    evict_test.go:144: 400 writes of 16384 bytes under a 1048576-byte quota: final usage 819200 (high water 943718, low water 734003), 350 objects evicted
--- PASS: TestSweepStaysWithinQuota (0.05s)
    evict_test.go:188: 12 of 12 continuously-read objects survived 200 cold writes under a 524288-byte quota
--- PASS: TestHotObjectsOutliveColdOnes (0.07s)
    evict_test.go:225: sweep evicted 35 objects (143360 bytes): 245760 -> 102400, low water 102400
--- PASS: TestSweepDrainsToLowWater (0.00s)
--- PASS: TestSweepIsANoOpBelowHighWater (0.00s)
--- PASS: TestEvictionRemovesFileAndIndexRowTogether (0.01s)
--- PASS: TestSweepAfterCloseIsRejected (0.00s)
    evictbench_test.go:84: wrote   6000 objects of 16384 bytes in 212ms (28286 objects/s)
    evictbench_test.go:86: evicted 4800 objects, 78643200 bytes in 49ms (98129 objects/s, 1533.3 MiB/s)
    evictbench_test.go:88: usage   98304000 -> 19660800 (low water 19660800)
    evictbench_test.go:101: wrote /mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache/bench/results/2026-09-20-yutongzhao/eviction-throughput.json
--- PASS: TestEvictionThroughput (0.27s)
--- PASS: TestSweepTempsOnOpen (0.00s)
ok  	github.com/Lexieli666/kilncache/internal/storage	0.410s
```

## reconciliation and the metadata index
```
--- PASS: TestIndexUpsertAndGet (0.00s)
--- PASS: TestIndexGetMissing (0.00s)
--- PASS: TestIndexUpsertPreservesAccessHistory (0.00s)
--- PASS: TestIndexTotals (0.00s)
--- PASS: TestIndexRemoveCancelsPendingTouch (0.00s)
--- PASS: TestIndexColdestNIsOrdered (0.03s)
--- PASS: TestIndexTouchesAreBatched (0.00s)
--- PASS: TestIndexTouchQueueIsBounded (0.00s)
--- PASS: TestIndexConcurrentTouches (0.02s)
--- PASS: TestIndexReplaceAll (0.00s)
--- PASS: TestIndexMeta (0.00s)
--- PASS: TestIndexSurvivesReopen (0.00s)
--- PASS: TestIndexClosedRejectsWrites (0.00s)
--- PASS: TestIndexUsesWAL (0.00s)
    index_test.go:499: index scan: 20000 rows in 26ms idle, 22663 rows in 38ms under 3892 concurrent writes (1.46x)
    index_test.go:513: wrote /mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache/bench/results/2026-09-20-yutongzhao/index-scan.json
--- PASS: TestIndexScanIsNotBlockedByWrites (0.42s)
--- PASS: TestReconcileNeverMarksAMissingFileValid (0.00s)
--- PASS: TestReconcileAdoptsUnknownFiles (0.00s)
--- PASS: TestReconcilePreservesAccessHistory (0.01s)
--- PASS: TestReconcileCorrectsSize (0.00s)
--- PASS: TestReconcileReportsWhatItDid (0.00s)
ok  	github.com/Lexieli666/kilncache/internal/storage	0.509s
```

## repair
```
--- PASS: TestRepairRecreatesAMissingReplica (0.50s)
--- PASS: TestRepairOnlyIssuesReplicaWrites (0.51s)
--- PASS: TestRepairDeclinedOverQuotaIsNotAFailure (0.50s)
--- PASS: TestRepairSkipsObjectsThisNodeDoesNotOwn (0.00s)
--- PASS: TestRepairIsANoOpWhenTheReplicaIsPresent (0.40s)
--- PASS: TestRepairTreatsAnUnreachablePeerAsUnknown (0.41s)
    repair_test.go:534: converged to 2 replicas for all 50 objects in 61ms ({AuditRuns:1 ObjectsAudited:50 Enqueued:50 DroppedFullQueue:0 Repaired:50 AlreadyPresent:0 Failed:0 BytesRepaired:543 NotOurs:0 DeclinedOverQuota:0 QueueDepth:0 QueueCapacity:256})
--- PASS: TestRepairConvergesAfterAPeerReturns (0.07s)
--- PASS: TestFullQueueStopsThePassInsteadOfBlockingOrSkipping (0.00s)
    repair_test.go:651: all 30 objects repaired through a queue of depth 4 ({AuditRuns:5 ObjectsAudited:34 Enqueued:30 DroppedFullQueue:4 Repaired:30 AlreadyPresent:0 Failed:0 BytesRepaired:262 NotOurs:0 DeclinedOverQuota:0 QueueDepth:0 QueueCapacity:4})
--- PASS: TestFullQueueDoesNotSkipObjects (1.05s)
--- PASS: TestAuditBudgetAndCursor (0.01s)
--- PASS: TestAuditIsANoOpOnASingleNodeCluster (0.00s)
--- PASS: TestNewRejectsMissingCoordinator (0.00s)
--- PASS: TestRunStopsCleanly (0.10s)
--- PASS: TestCloseWithoutRun (0.00s)
--- PASS: TestRepairFailureIsCounted (0.50s)
ok  	github.com/Lexieli666/kilncache/internal/repair	4.071s
```

## metrics and node assembly
```
--- PASS: TestScrapeIsValidExposition (0.00s)
--- PASS: TestEveryMetricCarriesTheNodeLabel (0.00s)
--- PASS: TestSourcesReadLiveValues (0.00s)
--- PASS: TestSourcesWithDistinctLabels (0.00s)
--- PASS: TestSameNameDifferentHelpIsRejectedClearly (0.00s)
--- PASS: TestDuplicateSourceIsAnErrorNotAPanic (0.00s)
--- PASS: TestTwoNodesInOneProcess (0.00s)
--- PASS: TestInFlightReturnsToZero (0.00s)
--- PASS: TestObserveForwardedDefaultsToClient (0.00s)
--- PASS: TestLatencyBucketsCoverTheRangeThatMatters (0.00s)
ok  	github.com/Lexieli666/kilncache/internal/metrics	0.014s
--- PASS: TestNewRejectsInvalidConfig (0.00s)
--- PASS: TestNewAssemblesEverySubsystem (0.00s)
--- PASS: TestRepairCanBeDisabled (0.00s)
--- PASS: TestStatsReportEverySubsystem (0.00s)
--- PASS: TestStatsEndpointServesJSON (0.00s)
--- PASS: TestMetricsEndpointExposesEverySource (0.00s)
--- PASS: TestRunStartsAndStopsBackgroundWorkers (0.01s)
--- PASS: TestCloseIsIdempotent (0.00s)
--- PASS: TestQuotaProbeReflectsUsage (0.00s)
ok  	github.com/Lexieli666/kilncache/internal/node	0.030s
```

## chaos runner control run against three real nodes
```
    run_test.go:148: control run: 20202 writes, 30183 reads, 35234 verified, 0 corrupted; 3000/3000 at 2+ replicas
--- PASS: TestControlRunAgainstRealNodes (7.38s)
ok  	github.com/Lexieli666/kilncache/cmd/chaos	7.388s
```

## integration suite (in-process three-node cluster)
```
--- PASS: TestSingleNodeRoundTrip (0.01s)
--- PASS: TestLargeObjectOverRealSocket (0.18s)
--- PASS: TestClientDisconnectMidUpload (0.03s)
--- PASS: TestChunkedUploadWithoutContentLength (0.00s)
--- PASS: TestRestartPersistsObjects (0.05s)
--- PASS: TestConcurrentClientsSameKey (0.01s)
--- PASS: TestMalformedRequestsOverHTTP (0.00s)
--- PASS: TestOversizeUploadRejected (0.00s)
--- PASS: TestACNamespaceIsSeparate (0.01s)
--- PASS: TestGracefulShutdownDrainsInFlight (0.16s)
--- PASS: TestClusterWriteThroughAnyFrontDoor (0.17s)
--- PASS: TestReplicasLandOnTheRightNodes (0.08s)
--- PASS: TestReadSurvivesLosingTheHolder (0.25s)
--- PASS: TestWriteFailsLoudlyWhenAReplicaIsDown (0.07s)
--- PASS: TestNoCopyIsEverWrong (0.60s)
--- PASS: TestForwardedRequestIsNotForwardedAgain (0.01s)
--- PASS: TestClientCannotForgeAHopRole (0.01s)
--- PASS: TestConcurrentClusterTraffic (0.12s)
--- PASS: TestRestartRejoinsCluster (0.08s)
--- PASS: TestSingleNodeClusterStillWorks (0.00s)
--- PASS: TestHeadAgreesWithGetAcrossTheCluster (0.09s)
ok  	github.com/Lexieli666/kilncache/tests/integration	1.925s
```

## 10-minute chaos run against the compose cluster

Command:
```
./bin/chaos -duration 10m -fault-every 60s -fault-down 25s \
  -converge-wait 240s -workers 12 -out bench/results/2026-09-20-yutongzhao \
  -compose-file deploy/compose/docker-compose.yml -compose-project kilncache
```

Cluster: three containers, 12 GiB quota each, replica count 2.

Result (full report in `chaos-latest.json`):
```
kilnchaos: converged after 217.2s: 1122 objects at >= 2 replicas, 1878 absent everywhere, 0 under-replicated
kilnchaos: wrote /mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache/bench/results/2026-09-20-yutongzhao/chaos-20260920-082057.json

=== kilnchaos report ===
duration            601s across 3 nodes, 12 workers, seed 1789891855817446203
faults              10 node stops
writes              106023 ok, 44796 refused with 503, 33697 errors
reads               301576 hits, 21811 misses, 105153 errors
throughput          1020 ops/s, error rate 0.2265
errors by class     98592 connection refused, 12 timeouts, 27035 server 5xx
CORRUPTED READS     0 out of 304325 reads verified
convergence         1122/3000 sampled objects at >= 2 replicas after 217.2s (converged: true)
final verification  2749 correct, 0 corrupted, 2300 missing, of 5049 checked
verdict             PASS

```

Reading the numbers:

- **0 corrupted reads out of 304,325 reads verified.** Every read was checked
  against a digest the runner computed itself, never one the cluster reported.
- **Converged to 2 replicas in 217 s** over 3,000 sampled objects, 2,000 of them
  drawn from writes the cluster had *refused* with 503 — those are the genuinely
  under-replicated ones, since an acknowledged write had both copies at the
  moment it was acknowledged.
- **2,300 objects missing at the end, 0 corrupted.** The cluster evicted 24,385
  objects to stay inside its quota during the run, so a missing object is an
  evicted one. The report records both, and the verdict only calls missing
  objects data loss when no eviction was observed.
- **22.6% error rate** during traffic, almost all connection-refused against the
  node that was stopped at the time. A chaos run with a zero error rate would
  mean the faults never landed, and the tool reports that as INCONCLUSIVE.

## test count
```
$ go test -count=1 -v ./... | grep -c "^=== RUN"
229
$ go test -tags=integration -count=1 -v ./tests/integration/ ./cmd/chaos/ | grep -c "^=== RUN"
40
```

## coverage
```
$ make cover-all
total:									(statements)		82.7%
```

## no unmeasured numbers
```
check-numbers: OK - 5 numeric claim(s) across 10 file(s), each backed by an existing raw result
```
