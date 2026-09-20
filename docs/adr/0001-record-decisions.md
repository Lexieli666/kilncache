# ADR-0001: Record architecture decisions

- Status: accepted
- Date: 2026-09-20

## Context

KilnCache makes a handful of choices that determine everything downstream:
skipping consensus, hashing keys with rendezvous instead of a hash ring, fsyncing
the parent directory before acknowledging a write, evicting by a size-weighted
heap rather than plain LRU. Each of these has a defensible alternative. Six
months from now — or in an interview — the interesting question is never "what
does it do" but "why is it not the other thing", and by then the reasoning is
gone unless it was written down at the time.

The failure mode this guards against is specific: a reviewer asks "why not a
hash ring?" and the answer is a reconstruction rather than a record. A
reconstruction is always more flattering than what actually happened, and it
omits the constraint that actually drove the decision.

## Decision

Every decision that a future reader would otherwise have to reverse-engineer
from the code gets a numbered Markdown file in `docs/adr/`, in this format:

```
# ADR-NNNN: <short imperative title>
- Status: proposed | accepted | superseded by ADR-MMMM
- Date: YYYY-MM-DD
## Context      - the forces, including the ones that were about schedule
## Decision     - what was chosen, in the present tense
## Consequences - what this makes easy, what it makes hard, what it rules out
## Alternatives - what else was considered and the specific reason it lost
```

ADRs are append-only. A decision that turns out to be wrong is superseded by a
new ADR that says so; the original stays, with its status updated. Deleting the
record of a wrong decision deletes the most useful thing in the directory.

Decisions made under time pressure say so in Context. "We had four weeks" is a
real force and pretending otherwise makes the record less useful, not more
professional.

## Consequences

- A reviewer can read `docs/adr/` in ten minutes and know what this project is
  and is not trying to be.
- There is a small tax on every non-obvious change.
- Reversals are visible. This is the point.

## Alternatives

- **Design doc up front.** One large document goes stale as a unit and nobody
  notices which paragraph stopped being true. ADRs go stale one decision at a
  time and the staleness is visible in the status line.
- **Commit messages only.** Rule 2 of CONTRIBUTING already requires commit
  messages to say why, and they do carry the local reasoning. But `git log` is
  not browsable by someone evaluating the design, and a decision that spans
  fifteen commits has no single message to live in.
- **Nothing.** The default. It is why most repositories cannot answer "why not
  Raft" without the original author present.
