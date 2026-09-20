# Phase 2 verification — 2026-09-20T07:07:07Z

Three-node static membership, rendezvous placement, synchronous two-copy
replication before ACK, replica fallback on read, bounded forwarding.

Measured placement numbers: `placement.json`. Docker node-loss run:
`phase2-docker-node-loss.json`.

## go test ./...
```
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/node	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	0.307s
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	0.003s
ok  	github.com/Lexieli666/kilncache/internal/cluster	0.693s
ok  	github.com/Lexieli666/kilncache/internal/config	0.004s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.033s
ok  	github.com/Lexieli666/kilncache/internal/logging	0.003s
ok  	github.com/Lexieli666/kilncache/internal/protocol	0.003s
ok  	github.com/Lexieli666/kilncache/internal/storage	0.185s
```

## go test -race ./...
```
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/node	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	1.319s
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	1.010s
ok  	github.com/Lexieli666/kilncache/internal/cluster	4.441s
ok  	github.com/Lexieli666/kilncache/internal/config	1.010s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	1.157s
ok  	github.com/Lexieli666/kilncache/internal/logging	1.007s
ok  	github.com/Lexieli666/kilncache/internal/protocol	1.007s
ok  	github.com/Lexieli666/kilncache/internal/storage	1.929s
```

## go vet ./... && golangci-lint run ./...
```
(both clean)
```

## placement property tests, with the measured values
```
--- PASS: TestOrderIndependence (0.00s)
--- PASS: TestPrimaryAndReplicaAreDistinct (0.02s)
    ring_test.go:173: cluster of 3 over 50000 keys: primary imbalance max/mean = 1.0127, holder imbalance = 1.0076
    ring_test.go:173: cluster of 4 over 50000 keys: primary imbalance max/mean = 1.0102, holder imbalance = 1.0086
    ring_test.go:173: cluster of 5 over 50000 keys: primary imbalance max/mean = 1.0162, holder imbalance = 1.0062
    ring_test.go:173: cluster of 8 over 50000 keys: primary imbalance max/mean = 1.0208, holder imbalance = 1.0142
--- PASS: TestPlacementBalance (0.11s)
    ring_test.go:212: adding node 4 of 4 over 50000 keys: 25.26% of primaries moved (12628 keys), 50.12% of two-node holder sets changed (25062 keys)
--- PASS: TestKeyMovementOnGrowth (0.04s)
    ring_test.go:264: removing node-d over 20000 keys: 5013 primaries moved, all of them node-d's (5013)
--- PASS: TestKeyMovementOnLoss (0.01s)
    ring_test.go:284: node-b holds 67.7% of 3000 keys with RF=2 over 3 nodes (expected ~66.7%)
--- PASS: TestSelfHoldsAndPrimary (0.00s)
    ring_test.go:338: [rapid] OK, passed 100 tests (734.91µs)
--- PASS: TestPropOwnersDeterministic (0.00s)
    ring_test.go:366: [rapid] OK, passed 100 tests (545.716µs)
--- PASS: TestPropHoldersAreDistinct (0.00s)
    ring_test.go:398: [rapid] OK, passed 100 tests (875.443µs)
--- PASS: TestPropAddingANodeOnlyMovesToTheNewNode (0.00s)
    ring_test.go:430: [rapid] OK, passed 100 tests (79.149843ms)
--- PASS: TestPropWeightsAreWellSpread (0.08s)
    ring_test.go:471: mix avalanche: 32.00 of 64 bits change per input bit flip
--- PASS: TestMixAvalanche (0.00s)
ok  	github.com/Lexieli666/kilncache/internal/cluster	0.282s
```

## in-process three-node integration suite
```
--- PASS: TestSingleNodeRoundTrip (0.01s)
--- PASS: TestLargeObjectOverRealSocket (0.18s)
--- PASS: TestClientDisconnectMidUpload (0.03s)
--- PASS: TestChunkedUploadWithoutContentLength (0.00s)
--- PASS: TestRestartPersistsObjects (0.04s)
--- PASS: TestConcurrentClientsSameKey (0.01s)
--- PASS: TestMalformedRequestsOverHTTP (0.01s)
--- PASS: TestOversizeUploadRejected (0.00s)
--- PASS: TestACNamespaceIsSeparate (0.00s)
--- PASS: TestGracefulShutdownDrainsInFlight (0.15s)
--- PASS: TestClusterWriteThroughAnyFrontDoor (0.13s)
--- PASS: TestReplicasLandOnTheRightNodes (0.04s)
    cluster_test.go:147: verified 200 objects held by the killed node, 106 of them served via peer fallback
--- PASS: TestReadSurvivesLosingTheHolder (0.23s)
    cluster_test.go:191: 79 of 120 writes involved the dead node; 79 correctly returned 503, 0 did not
--- PASS: TestWriteFailsLoudlyWhenAReplicaIsDown (0.05s)
    cluster_test.go:228: verified 1200 reads of 400 objects across 3 nodes: 0 corrupted
--- PASS: TestNoCopyIsEverWrong (0.52s)
--- PASS: TestForwardedRequestIsNotForwardedAgain (0.00s)
--- PASS: TestClientCannotForgeAHopRole (0.00s)
--- PASS: TestConcurrentClusterTraffic (0.09s)
--- PASS: TestRestartRejoinsCluster (0.05s)
--- PASS: TestSingleNodeClusterStillWorks (0.00s)
--- PASS: TestHeadAgreesWithGetAcrossTheCluster (0.06s)
ok  	github.com/Lexieli666/kilncache/tests/integration	1.608s
```

## Docker: three containers, 1000 objects, stop a container, read through another
```
    docker_test.go:260: wrote 1000 objects across three containers, every one acknowledged with 2 copies
    docker_test.go:266: stopped container node-b
    docker_test.go:295: with container node-b down: read 671 objects it held through node-c, 327 served by peer fallback, 0 corrupted
    docker_test.go:303: restarted container node-b
    docker_test.go:330: after restart, container node-b served 671 of its own copies from its named volume, all byte-correct
    docker_test.go:343: wrote /mnt/d/2026 LYC Job Hunting/SDE-xiaozhao/A. General SWE and Product Engineering/Projects Impl/kilncache/bench/results/2026-09-20-yutongzhao/phase2-docker-node-loss.json
--- PASS: TestDockerClusterSurvivesNodeLoss (21.45s)
ok  	github.com/Lexieli666/kilncache/tests/integration	21.460s
```

## the whole integration suite under the race detector
```
ok  	github.com/Lexieli666/kilncache/tests/integration	9.976s
```

## test count
```
$ go test -count=1 -v ./... | grep -c "^=== RUN"
139
$ go test -tags=integration -count=1 -v ./tests/integration/ | grep -c "^=== RUN"
23
```

## coverage (make cover-all)
```
$ make cover-all
ok  	github.com/Lexieli666/kilncache/internal/buildinfo	0.005s	coverage: 1.6% of statements in ./internal/..., ./cmd/...
	github.com/Lexieli666/kilncache/internal/node		coverage: 0.0% of statements
ok  	github.com/Lexieli666/kilncache/internal/cluster	1.298s	coverage: 27.3% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/internal/config	0.004s	coverage: 15.2% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.036s	coverage: 30.7% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/internal/logging	0.005s	coverage: 1.0% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/internal/protocol	0.005s	coverage: 0.6% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/internal/storage	0.188s	coverage: 17.2% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/cmd/kilncache	0.309s	coverage: 27.5% of statements in ./internal/..., ./cmd/...
ok  	github.com/Lexieli666/kilncache/tests/integration	24.642s	coverage: 56.2% of statements in ./internal/..., ./cmd/...
total:									(statements)		82.9%
```

## no unmeasured numbers
```
check-numbers: OK - 2 numeric claim(s) across 9 file(s), each backed by an existing raw result
```
