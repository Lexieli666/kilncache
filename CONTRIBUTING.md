# Contributing to KilnCache

Three rules govern this repository. They are not style preferences; each one
exists because the alternative produces a project that looks convincing and
isn't.

## Rule 1 — No unmeasured numbers

Every figure that appears in `README.md`, `BENCHMARKS.md`, any file under
`docs/`, or a commit message must be reproducible by a command committed to this
repository, with the raw output of that command committed under
`bench/results/<ISO-date>-<hostname>/`.

Concretely:

- A throughput or latency number cites the JSON file it came from.
- A test count comes from `go test ./... -count=1` output, not from memory.
- A coverage percentage comes from `go tool cover -func`, not an estimate.
- "About", "roughly", and "~" are not exemptions. If the number is not measured,
  it does not get written down.

The falsifier for this rule is mechanical: `scripts/check-numbers.sh` greps the
published documents for numeric claims and fails if a claim has no matching raw
result file.

## Rule 2 — Commit history is an artifact

Commits are small and their messages say **why**, not what. The diff already
says what.

Format is [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(storage): fsync the parent directory before acknowledging a PUT

A rename is only durable once the directory entry is on stable media. Without
the directory fsync, a power loss between rename and the next journal commit
leaves the object invisible while the metadata index still claims it exists,
which is exactly the "stale metadata" failure the repair worker cannot
distinguish from a deleted object.
```

Scopes in use: `storage`, `cluster`, `httpapi`, `repair`, `metrics`, `bench`,
`chaos`, `fixtures`, `ci`, `docs`, `build`.

A commit that mixes a bug fix with a refactor gets split. A commit whose message
is "fix tests" gets rewritten before it is pushed.

## Rule 3 — Every claim has a falsifier

For every claim the project makes, there is an artifact that would fail if the
claim were false, and the claim names it.

| Claim type | Required falsifier |
|---|---|
| Correctness ("a partial upload is never visible") | A test that produces the partial upload and asserts the object is absent. |
| Concurrency ("safe under concurrent PUTs of the same key") | A test that runs it under `-race` in CI. |
| Distributed behaviour ("a read survives losing the primary") | An integration test that kills the primary and verifies the bytes. |
| Performance ("N MiB/s") | The benchmark command, the raw JSON, and the `hostinfo.json` line for the machine. |
| Absence ("zero corrupted reads") | The run that would have detected one: its duration, its operation count, and its report file. |

"Zero corrupted reads" with no statement of how many reads were checked is not a
claim, it is a mood. Every absence claim states its sample size.

## Working agreement

- Tests first where the behaviour is specifiable. Each phase's acceptance
  criteria in the spec are written as tests before the feature is written.
- `make lint test test-race` must pass before every commit.
- New public behaviour arrives with its documentation in the same commit.
- Decisions that a future reader would otherwise have to reverse-engineer go in
  `docs/adr/` as a numbered ADR, including the ones that were made under time
  pressure and the ones that were wrong.
- Bugs that the tests caught are recorded in `docs/bugs.md` with the test that
  caught them. A bug list is evidence the tests work; an empty one is evidence
  they don't.

## Built with an AI assistant

This repository was written with Claude Code under the protocol above: the
acceptance criteria and the falsifier come first, the implementation follows,
and no number reaches a document without a committed raw result behind it. Where
the assistant produced something wrong, the bug and the test that caught it are
in `docs/bugs.md` rather than quietly amended away.
