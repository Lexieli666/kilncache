#!/usr/bin/env bash
#
# Falsifier for CONTRIBUTING rule 1: no unmeasured numbers.
#
# Scans the published documents for numeric performance claims and fails if a
# claim is not accompanied, within its own paragraph or table, by a reference to
# a file that actually exists under bench/results/.
#
# It works on paragraphs rather than lines because prose wraps: "measured 914
# IOPS" and the citation that backs it routinely land on different lines of the
# same sentence, and a line-based check would either reject that or force an
# unreadable reflow.
#
# This is deliberately mechanical and deliberately narrow. It cannot tell that a
# number is *correct*; it can tell that a number arrived without provenance,
# which is the failure this repository is actually prone to. A line that calls
# its number a target is exempt -- targets are allowed to be aspirational,
# results are not.
#
# Usage: scripts/check-numbers.sh [file ...]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
set +e

root="$(repo_root)"
cd "$root" || exit 1

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
  mapfile -t files < <(ls README.md BENCHMARKS.md docs/*.md docs/adr/*.md bench/RESULTS_SUMMARY.md 2>/dev/null)
fi

export UNIT_RE='[0-9][0-9,.]*\s*(req/s|reqs/s|requests/s|ops/s|MiB/s|GiB/s|KiB/s|MB/s|GB/s|IOPS|ms\b|µs\b|seconds\b|% (faster|lower|reduction|coverage|of statements)|x faster)'
export EXEMPT_RE='(?i)target|aspiration|goal range|not yet measured|e\.g\.|for example|placeholder|budget|\bwant\b|shields\.io'

python3 - "${files[@]}" <<'PY'
import os, re, sys

unit_re = re.compile(os.environ['UNIT_RE'])
exempt_re = re.compile(os.environ['EXEMPT_RE'])
ref_re = re.compile(r'bench/results/[A-Za-z0-9._/-]+')

fail = 0
checked = 0
files = sys.argv[1:]

for path in files:
    if not os.path.isfile(path):
        continue
    text = open(path, encoding='utf-8').read()
    lines = text.splitlines()

    # Split into blocks separated by blank lines; a Markdown table counts as one
    # block, so a "Source:" line under the table backs every row in it.
    blocks, start, cur = [], 0, []
    for i, line in enumerate(lines):
        if line.strip() == '':
            if cur:
                blocks.append((start, cur))
                cur = []
        else:
            if not cur:
                start = i + 1
            cur.append(line)
    if cur:
        blocks.append((start, cur))

    in_code = False
    for lineno, block in blocks:
        body = '\n'.join(block)
        # Skip fenced code blocks: a command line or a log excerpt is evidence,
        # not a claim.
        if body.lstrip().startswith('```'):
            continue
        claim_lines = [l for l in block if unit_re.search(l) and not exempt_re.search(l)]
        if not claim_lines:
            continue
        checked += len(claim_lines)
        refs = ref_re.findall(body)
        if not refs:
            for l in claim_lines:
                print(f'{path}:{lineno}: numeric claim with no bench/results/ reference:', file=sys.stderr)
                print(f'    {l.strip()}', file=sys.stderr)
            fail = 1
            continue
        for ref in refs:
            ref = ref.rstrip('.,)`')
            if not os.path.exists(ref):
                print(f'{path}:{lineno}: references a result file that does not exist: {ref}', file=sys.stderr)
                fail = 1

if fail == 0:
    print(f'check-numbers: OK - {checked} numeric claim(s) across {len(files)} file(s), each backed by an existing raw result')
else:
    print('check-numbers: FAILED - see above. Rule 1: every published number comes from a committed raw result.', file=sys.stderr)
sys.exit(fail)
PY
