#!/usr/bin/env bash
#
# Falsifier for CONTRIBUTING rule 1: no unmeasured numbers.
#
# Scans the published documents for numeric performance claims and fails if a
# claim is not accompanied, on the same line or in the same table block, by a
# reference to a file that actually exists under bench/results/.
#
# This is deliberately mechanical and deliberately narrow. It cannot tell that a
# number is *correct*; it can tell that a number arrived without provenance,
# which is the failure this repository is actually prone to. Prose that mentions
# a target range is exempt only when the line says "target" — targets are
# allowed to be aspirational, results are not.
#
# Usage: scripts/check-numbers.sh [file ...]     (default: README.md BENCHMARKS.md docs/*.md)

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
set +e

root="$(repo_root)"
cd "$root" || exit 1

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
  mapfile -t files < <(ls README.md BENCHMARKS.md docs/*.md 2>/dev/null)
fi

# A "measured claim" is a number attached to one of these units.
unit_re='[0-9][0-9,.]*[[:space:]]*(req/s|reqs/s|requests/s|ops/s|MiB/s|GiB/s|KiB/s|MB/s|GB/s|IOPS|ms\b|µs\b|us\b|seconds\b|s p(50|95|99)|% (faster|lower|reduction|coverage)|x faster)'

# Lines that are allowed to carry a bare number.
exempt_re='target|Target|TARGET|aspiration|goal range|not yet measured|e\.g\.|for example|placeholder|^\|[[:space:]]*Metric|^\|[[:space:]]*-|badge|shields\.io|^#|http[s]?://'

fail=0
checked=0

for f in "${files[@]}"; do
  [ -f "$f" ] || continue
  lineno=0
  while IFS= read -r line; do
    lineno=$((lineno + 1))
    echo "$line" | grep -Eq "$unit_re" || continue
    echo "$line" | grep -Eq "$exempt_re" && continue
    checked=$((checked + 1))

    # The claim must name a raw result path, and that path must exist.
    refs=$(echo "$line" | grep -oE 'bench/results/[A-Za-z0-9._/-]+' | tr -d ')`,')
    if [ -z "$refs" ]; then
      echo "$f:$lineno: numeric claim with no bench/results/ reference:" >&2
      echo "    $line" >&2
      fail=1
      continue
    fi
    for ref in $refs; do
      if [ ! -e "$ref" ]; then
        echo "$f:$lineno: references a result file that does not exist: $ref" >&2
        fail=1
      fi
    done
  done < "$f"
done

if [ "$fail" -eq 0 ]; then
  echo "check-numbers: OK — $checked numeric claim(s) across ${#files[@]} file(s), each backed by an existing raw result"
else
  echo "check-numbers: FAILED — see above. Rule 1: every published number comes from a committed raw result." >&2
fi
exit "$fail"
