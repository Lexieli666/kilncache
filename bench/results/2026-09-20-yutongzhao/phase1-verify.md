# Phase 1 verification — 2026-09-20T06:42:28Z

Single-node disk-backed Bazel HTTP remote cache. Bazel end-to-end evidence is in
phase1-bazel-e2e.md in this directory.

## go test ./...
```
?   	github.com/Lexieli666/kilncache/cmd/kilncache	[no test files]
?   	github.com/Lexieli666/kilncache/internal/buildinfo	[no test files]
?   	github.com/Lexieli666/kilncache/internal/cluster	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/config	0.005s
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/node	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.033s
ok  	github.com/Lexieli666/kilncache/internal/logging	0.002s
ok  	github.com/Lexieli666/kilncache/internal/storage	0.184s
```

## go test -race ./...
```
?   	github.com/Lexieli666/kilncache/cmd/kilncache	[no test files]
?   	github.com/Lexieli666/kilncache/internal/buildinfo	[no test files]
?   	github.com/Lexieli666/kilncache/internal/cluster	[no test files]
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/node	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/config	1.009s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	1.162s
ok  	github.com/Lexieli666/kilncache/internal/logging	1.007s
ok  	github.com/Lexieli666/kilncache/internal/storage	1.948s
```

## go vet ./... && golangci-lint run ./...
```
(both clean)
```

## integration suite (real listeners, real sockets, real files)
```
--- PASS: TestSingleNodeRoundTrip (0.01s)
--- PASS: TestLargeObjectOverRealSocket (0.18s)
--- PASS: TestClientDisconnectMidUpload (0.03s)
--- PASS: TestChunkedUploadWithoutContentLength (0.00s)
--- PASS: TestDigestMismatchOverHTTP (0.00s)
--- PASS: TestRestartPersistsObjects (0.04s)
--- PASS: TestConcurrentClientsSameKey (0.01s)
--- PASS: TestMalformedRequestsOverHTTP (0.00s)
--- PASS: TestOversizeUploadRejected (0.00s)
--- PASS: TestACNamespaceIsSeparate (0.00s)
--- PASS: TestGracefulShutdownDrainsInFlight (0.15s)
PASS
ok  	github.com/Lexieli666/kilncache/tests/integration	0.432s
```

## race detector over the integration suite
```
ok  	github.com/Lexieli666/kilncache/tests/integration	2.568s
```

## test count
```
$ go test -count=1 -v ./... | grep -c "^=== RUN"
84
$ go test -tags=integration -count=1 -v ./tests/integration/ | grep -c "^=== RUN"
11
```

## coverage
```
$ go test -coverprofile=coverage.out -covermode=atomic ./...
	github.com/Lexieli666/kilncache/internal/buildinfo		coverage: 0.0% of statements
	github.com/Lexieli666/kilncache/cmd/kilncache		coverage: 0.0% of statements
ok  	github.com/Lexieli666/kilncache/internal/config	0.004s	coverage: 79.3% of statements
	github.com/Lexieli666/kilncache/internal/node		coverage: 0.0% of statements
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.034s	coverage: 83.9% of statements
ok  	github.com/Lexieli666/kilncache/internal/logging	0.003s	coverage: 92.9% of statements
ok  	github.com/Lexieli666/kilncache/internal/storage	0.185s	coverage: 78.4% of statements
$ go tool cover -func=coverage.out | tail -1
total:									(statements)		70.4%
```

## no unmeasured numbers
```
check-numbers: OK - 1 numeric claim(s) across 6 file(s), each backed by an existing raw result
```
