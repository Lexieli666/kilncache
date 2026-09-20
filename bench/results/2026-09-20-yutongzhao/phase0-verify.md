# Phase 0 verification — 2026-09-20T06:17:10Z

## go build ./...
```
(clean)
```

## go test ./...
```
?   	github.com/Lexieli666/kilncache/cmd/kilncache	[no test files]
?   	github.com/Lexieli666/kilncache/internal/buildinfo	[no test files]
?   	github.com/Lexieli666/kilncache/internal/cluster	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/config	0.003s
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
?   	github.com/Lexieli666/kilncache/internal/storage	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.004s
ok  	github.com/Lexieli666/kilncache/internal/logging	0.003s
```

## go test -race ./...
```
?   	github.com/Lexieli666/kilncache/cmd/kilncache	[no test files]
?   	github.com/Lexieli666/kilncache/internal/buildinfo	[no test files]
?   	github.com/Lexieli666/kilncache/internal/cluster	[no test files]
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
?   	github.com/Lexieli666/kilncache/internal/storage	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/config	1.010s
ok  	github.com/Lexieli666/kilncache/internal/httpapi	1.011s
ok  	github.com/Lexieli666/kilncache/internal/logging	1.008s
```

## go vet ./...
```
(clean)
```

## golangci-lint run ./...
```
(clean)
```

## docker compose config
```
(valid)
```

## docker compose ps
```
NAME               STATUS
kilncache-node-a   Up 29 seconds (healthy)
kilncache-node-b   Up 29 seconds (healthy)
kilncache-node-c   Up 29 seconds (healthy)
```

## curl /healthz on all three nodes
```
$ curl -s localhost:8080/healthz
{"status":"ok","node":"node-a","version":"dev (unknown)","uptime_seconds":29.313133263}

$ curl -s localhost:8081/healthz
{"status":"ok","node":"node-b","version":"dev (unknown)","uptime_seconds":29.19301416}

$ curl -s localhost:8082/healthz
{"status":"ok","node":"node-c","version":"dev (unknown)","uptime_seconds":29.170677436}

```

## test count
```
$ go test -count=1 -v ./... | grep -c '^=== RUN'
32
```

## coverage
```
ok  	github.com/Lexieli666/kilncache/internal/config	0.003s	coverage: 80.4% of statements
?   	github.com/Lexieli666/kilncache/internal/metrics	[no test files]
?   	github.com/Lexieli666/kilncache/internal/repair	[no test files]
?   	github.com/Lexieli666/kilncache/internal/storage	[no test files]
ok  	github.com/Lexieli666/kilncache/internal/httpapi	0.005s	coverage: 81.9% of statements
ok  	github.com/Lexieli666/kilncache/internal/logging	0.004s	coverage: 92.9% of statements
total:									(statements)	66.5%
```
