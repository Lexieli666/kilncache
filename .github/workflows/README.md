# Workflows

`ci.yml` runs on every push to `main`, every pull request, and on demand.

| Job | What it proves |
|---|---|
| `lint` | Sources are gofmt-clean and pass `go vet` and golangci-lint (errcheck, bodyclose, staticcheck, gocritic, govet). |
| `test` | The unit and property suites pass on a clean checkout. |
| `test-race` | No data races under `-race`. This is the falsifier for every concurrency claim in the README. |
| `coverage` | Publishes `coverage.out` as an artifact and prints the total. The README quotes this number and nothing else. |
| `integration` | Builds the container image and runs the three-node Docker suite. |

Benchmarks are deliberately **not** run in CI: shared GitHub runners have noisy
neighbours and no NVMe guarantee, so a number produced there would violate the
repository's first rule (no unmeasured numbers — every figure comes with the
host line it was measured on). Benchmarks run on a named host and their raw
output is committed under `bench/results/`.
