#!/usr/bin/env bash
#
# Generate the Bazel C++ workspace used as the end-to-end benchmark workload.
#
# The fixture has to satisfy three constraints at once, and the generator exists
# because satisfying them by hand is not reproducible:
#
#   1. Enough targets that a build is dominated by compilation rather than by
#      Bazel's own startup (the spec asks for ~300).
#   2. A real dependency DAG, not 300 independent files. A flat workspace builds
#      with perfect parallelism and hides exactly the serialization that a
#      remote cache changes.
#   3. Deterministic output. The same seed produces byte-identical sources, so
#      a cold build and a cache-served build are comparable, and so a rebuild
#      three weeks later measures the cache rather than a different workload.
#
# The C++ is intentionally dull. It is a load generator for the compiler, not a
# demonstration of C++; the spec is explicit that C++ is not a claimed skill.
#
# Usage: scripts/gen-bazel-fixture.sh [--libs N] [--bins N] [--out DIR]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

libs=280
bins=20
out="$(repo_root)/fixtures/bazel-cpp"
seed=20260920

while [ $# -gt 0 ]; do
  case "$1" in
    --libs) libs="$2"; shift 2 ;;
    --bins) bins="$2"; shift 2 ;;
    --out)  out="$2";  shift 2 ;;
    --seed) seed="$2"; shift 2 ;;
    -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

command -v python3 >/dev/null 2>&1 || die "python3 is required to generate the fixture"

note "generating $libs libraries and $bins binaries into $out (seed $seed)"
python3 "$(dirname "${BASH_SOURCE[0]}")/gen_bazel_fixture.py" \
  --libs "$libs" --bins "$bins" --out "$out" --seed "$seed"

note "done. Build it with:"
note "  cd $out && bazel build //..."
note "  cd $out && bazel build //... --remote_cache=http://localhost:8080"
