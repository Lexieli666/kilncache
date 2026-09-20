#!/usr/bin/env bash
#
# Print one digest that summarises every build output of the fixture.
#
# This is the falsifier for "the cache-served build produces identical outputs".
# It hashes the *contents* of every binary and static library Bazel produced,
# concatenated in a stable order, so a single value can be compared between a
# cold build and a cache-served one. Comparing timestamps or sizes would not
# catch a cache that returned the right length of the wrong bytes.
#
# Usage: scripts/bazel-outputs-digest.sh [workspace-dir]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ws="${1:-$(repo_root)/fixtures/bazel-cpp}"
cd "$ws" || die "no such workspace: $ws"

bin=$(bazel info bazel-bin 2>/dev/null) || die "bazel info failed in $ws"
[ -d "$bin" ] || die "bazel-bin does not exist: $bin"

# Only real build artifacts. .params and .runfiles_manifest files embed absolute
# paths and action-specific ordering, so they differ between an execution and a
# cache hit for reasons that have nothing to do with the cache being correct.
find "$bin" -maxdepth 1 -type f \( -name 'bench*' -not -name '*.params' -not -name '*runfiles*' -o -name 'lib*.a' \) \
  | sort \
  | while read -r f; do
      printf '%s  %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$(basename "$f")"
    done
