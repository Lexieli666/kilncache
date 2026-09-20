#!/usr/bin/env bash
#
# The headline experiment: does pointing Bazel at KilnCache actually make a
# clean rebuild faster, and by how much?
#
# Four phases, in this order and for this reason:
#
#   1. COLD    - bazel clean --expunge, then build with no remote cache at all.
#                This is the baseline: what a developer with an empty local
#                cache and no shared cache pays.
#   2. POPULATE - clean --expunge, then build through KilnCache with an empty
#                cache. Every action misses, executes locally, and uploads. This
#                is what the first person to build after a change pays.
#   3. CACHED  - clean --expunge, then build through the now-warm KilnCache.
#                Every action should be a remote hit. This is what everyone
#                afterwards pays, and it is the number the project is about.
#   4. VERIFY  - the outputs of 1 and 3 are compared byte for byte. A cache that
#                makes a build fast and wrong is worse than no cache.
#
# Phases 1 and 3 are repeated and the median is reported, because a single
# sample of a build time on a shared machine is an anecdote.
#
# Usage: scripts/bazel-bench.sh [-n runs] [-o out-dir] [-t cache-url]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RUNS=5
OUT=""
CACHE_URL="http://localhost:8080"
WORKSPACE="$(repo_root)/fixtures/bazel-cpp"

while getopts "n:o:t:w:h" opt; do
  case "$opt" in
    n) RUNS="$OPTARG" ;;
    o) OUT="$OPTARG" ;;
    t) CACHE_URL="$OPTARG" ;;
    w) WORKSPACE="$OPTARG" ;;
    h) sed -n '2,26p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "bad option" ;;
  esac
done

[ -n "$OUT" ] || OUT="$(results_dir)"
mkdir -p "$OUT"
have bazel || die "bazel is required"
have jq    || die "jq is required"
[ -d "$WORKSPACE" ] || die "no workspace at $WORKSPACE; run scripts/gen-bazel-fixture.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

note "workspace: $WORKSPACE"
note "cache:     $CACHE_URL"
note "runs:      $RUNS per measured phase"

# --- helpers ---------------------------------------------------------------

now_s() { date +%s.%N; }

# node_stats fetches one node's counters, or an empty object if unreachable.
node_stats() {
  curl -fsS --max-time 5 "$CACHE_URL/stats" 2>/dev/null || echo '{}'
}

# bazel_metrics extracts what happened from Bazel's build event stream.
#
# The BEP is the authoritative source for hit counts: parsing the console output
# would be parsing a human-readable string that changes between releases.
bazel_metrics() {
  local bep="$1"
  # Runner counts are the authoritative record of what was served from the cache
  # and what was executed. Parsing the console output instead would be parsing a
  # human-readable string that changes between Bazel releases.
  jq -s '
    [ .[] | select(.id.buildMetrics) | .buildMetrics.actionSummary.runnerCount // [] ] | last // [] | . as $rc |
    {
      remote_cache_hits: ( [ $rc[] | select(.name == "remote cache hit") | .count ] | add // 0 ),
      locally_executed:  ( [ $rc[] | select(.name | test("linux-sandbox|local|processwrapper|worker")) | .count ] | add // 0 ),
      internal_actions:  ( [ $rc[] | select(.name == "internal") | .count ] | add // 0 ),
      total_actions:     ( [ $rc[] | select(.name == "total") | .count ] | add // 0 ),
      runner_counts:     $rc
    }' "$bep" 2>/dev/null || echo '{}'
}

# bazel_extra pulls the remaining build metrics from a second pass, keeping the
# jq expressions small enough to read.
bazel_extra() {
  local bep="$1"
  jq -s '
    {
      peak_heap_bytes: (
        [ .[] | select(.id.buildMetrics) | .buildMetrics.memoryMetrics.peakPostGcHeapSize // 0 ] | last // 0
      ),
      exit_code: (
        [ .[] | select(.id.buildFinished) | .finished.exitCode.code // 0 ] | last // 0
      )
    }' "$bep" 2>/dev/null || echo '{}'
}

# run_build times one build and records what Bazel and the cache both saw.
#
# --nokeep_state_after_build and a preceding clean --expunge together mean each
# run starts from nothing: no analysis cache, no action cache, no output tree.
# Without that, run 2 of a phase would measure an incremental build.
run_build() {
  local phase="$1" idx="$2"; shift 2
  local bep="$TMP/bep-$phase-$idx.json"
  local log="$TMP/log-$phase-$idx.txt"

  ( cd "$WORKSPACE" && bazel clean --expunge >/dev/null 2>&1 )

  local before after start end
  before="$(node_stats)"
  start="$(now_s)"
  ( cd "$WORKSPACE" && bazel build //... \
      --build_event_json_file="$bep" \
      "$@" > "$log" 2>&1 )
  local rc=$?
  end="$(now_s)"
  after="$(node_stats)"

  if [ $rc -ne 0 ]; then
    note "  build failed; last lines:"
    tail -15 "$log" >&2
    die "phase $phase run $idx failed"
  fi

  local elapsed
  elapsed="$(echo "$end - $start" | bc -l)"

  local metrics extra
  metrics="$(bazel_metrics "$bep")"
  extra="$(bazel_extra "$bep")"
  metrics="$(jq -n --argjson a "$metrics" --argjson b "$extra" '$a + $b')"

  # Bytes moved, from the cache's own counters rather than from Bazel's.
  local bytes_in bytes_out
  bytes_in="$(jq -n --argjson b "$before" --argjson a "$after" \
    '($a.storage.bytes_in // 0) - ($b.storage.bytes_in // 0)')"
  bytes_out="$(jq -n --argjson b "$before" --argjson a "$after" \
    '($a.storage.bytes_out // 0) - ($b.storage.bytes_out // 0)')"

  jq -n \
    --arg phase "$phase" \
    --argjson run "$idx" \
    --argjson seconds "$elapsed" \
    --argjson metrics "$metrics" \
    --argjson bytes_in "$bytes_in" \
    --argjson bytes_out "$bytes_out" \
    --arg summary "$(grep -E '^INFO: (Elapsed|[0-9]+ processes)' "$log" | tr '\n' ' ')" \
    '{phase: $phase, run: $run, seconds: $seconds, bazel: $metrics,
      cache_bytes_in: $bytes_in, cache_bytes_out: $bytes_out, summary: $summary}'
}

# outputs_digest hashes every build artifact, for the identical-outputs check.
outputs_digest() {
  bash "$(dirname "${BASH_SOURCE[0]}")/bazel-outputs-digest.sh" "$WORKSPACE"
}

# --- phase 1: cold, no remote cache ---------------------------------------

note ""
note "phase 1/4: cold builds with no remote cache ($RUNS runs)"
COLD="$TMP/cold.jsonl"
: > "$COLD"
for i in $(seq 1 "$RUNS"); do
  r="$(run_build cold "$i")"
  echo "$r" >> "$COLD"
  note "  run $i: $(echo "$r" | jq -r '.seconds | .*100 | round / 100')s"
done
outputs_digest > "$TMP/digest-cold.txt"
note "  outputs: $(wc -l < "$TMP/digest-cold.txt") artifacts"

# --- phase 2: populate -----------------------------------------------------
#
# This phase only means something against an empty cache. Run against a warm
# one it measures a cache hit and calls it a population, which would make the
# "first build after a change" number meaningless. The state is recorded either
# way rather than assumed.

note ""
note "phase 2/4: populating build through KilnCache"
OBJECTS_BEFORE="$(node_stats | jq -r '.usage.objects // 0')"
if [ "${OBJECTS_BEFORE:-0}" != "0" ]; then
  note "  WARNING: the cache already holds $OBJECTS_BEFORE objects."
  note "  The populating-build number below is therefore NOT a cold-population"
  note "  measurement, and the report records that. Start from an empty cache:"
  note "    docker compose -f deploy/compose/docker-compose.yml down -v && ... up -d --wait"
fi
POPULATE="$(run_build populate 1 --config=remote --remote_cache="$CACHE_URL")"
note "  $(echo "$POPULATE" | jq -r '.seconds | .*100 | round / 100')s, uploaded $(echo "$POPULATE" | jq -r '.cache_bytes_in') bytes"

# --- phase 3: cached -------------------------------------------------------

note ""
note "phase 3/4: rebuilds served from KilnCache ($RUNS runs)"
CACHED="$TMP/cached.jsonl"
: > "$CACHED"
for i in $(seq 1 "$RUNS"); do
  r="$(run_build cached "$i" --config=remote --remote_cache="$CACHE_URL")"
  echo "$r" >> "$CACHED"
  note "  run $i: $(echo "$r" | jq -r '.seconds | .*100 | round / 100')s, downloaded $(echo "$r" | jq -r '.cache_bytes_out') bytes"
done
outputs_digest > "$TMP/digest-cached.txt"

# --- phase 4: identical outputs -------------------------------------------

note ""
note "phase 4/4: comparing build outputs"
if diff -q "$TMP/digest-cold.txt" "$TMP/digest-cached.txt" >/dev/null; then
  IDENTICAL=true
  note "  IDENTICAL: all $(wc -l < "$TMP/digest-cold.txt") artifacts match byte for byte"
else
  IDENTICAL=false
  note "  DIFFERENCES FOUND:"
  diff "$TMP/digest-cold.txt" "$TMP/digest-cached.txt" | head -20 >&2
fi

# --- report ----------------------------------------------------------------

HOST="$(bash "$(dirname "${BASH_SOURCE[0]}")/hostinfo.sh" -)"
BAZEL_VERSION="$(cd "$WORKSPACE" && bazel --version 2>/dev/null | head -1)"
TARGETS="$(cd "$WORKSPACE" && bazel query 'kind(rule, //...)' 2>/dev/null | wc -l)"
COMMIT="$(git -C "$(repo_root)" rev-parse --verify HEAD 2>/dev/null || echo unknown)"

REPORT="$OUT/bazel-bench.json"
jq -n \
  --argjson cold "$(jq -s '.' "$COLD")" \
  --argjson populate "$POPULATE" \
  --argjson cached "$(jq -s '.' "$CACHED")" \
  --argjson host "$HOST" \
  --arg bazel_version "$BAZEL_VERSION" \
  --argjson targets "${TARGETS:-0}" \
  --arg commit "$COMMIT" \
  --arg cache_url "$CACHE_URL" \
  --argjson identical "$IDENTICAL" \
  --argjson artifacts "$(wc -l < "$TMP/digest-cold.txt")" \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson runs "$RUNS" \
  --argjson objects_before_populate "${OBJECTS_BEFORE:-0}" '
  def median: sort | if length == 0 then 0 elif length % 2 == 1 then .[length/2|floor] else (.[length/2-1] + .[length/2]) / 2 end;
  def times: [ .[].seconds ];

  ($cold | times)   as $ct |
  ($cached | times) as $wt |
  ($ct | median)    as $cm |
  ($wt | median)    as $wm |
  {
    kind: "bazel-end-to-end",
    generated_at: $generated_at,
    git_commit: $commit,
    bazel_version: $bazel_version,
    cache_url: $cache_url,
    workspace: { targets: $targets, build_artifacts_compared: $artifacts },
    runs_per_measured_phase: $runs,
    cache_objects_before_populating: $objects_before_populate,
    populating_build_is_valid: ($objects_before_populate == 0),
    host: $host,

    cold_builds: $cold,
    populating_build: $populate,
    cached_builds: $cached,

    summary: {
      cold_median_seconds:   $cm,
      cold_min_seconds:      ($ct | min),
      cold_max_seconds:      ($ct | max),
      populate_seconds:      $populate.seconds,
      cached_median_seconds: $wm,
      cached_min_seconds:    ($wt | min),
      cached_max_seconds:    ($wt | max),
      median_reduction_fraction: (if $cm > 0 then ($cm - $wm) / $cm else 0 end),
      median_speedup:            (if $wm > 0 then $cm / $wm else 0 end),
      bytes_uploaded_populating: $populate.cache_bytes_in,
      bytes_downloaded_cached_median: ([ $cached[].cache_bytes_out ] | median),
      outputs_identical: $identical
    }
  }' > "$REPORT"

note ""
note "wrote $REPORT"
echo
jq -r '
  "=== Bazel end-to-end ===",
  "workspace            \(.workspace.targets) targets, \(.workspace.build_artifacts_compared) build artifacts",
  "bazel                \(.bazel_version)",
  "runs                 \(.runs_per_measured_phase) per measured phase",
  "",
  "cold build (no cache)      median \(.summary.cold_median_seconds | .*100 | round / 100)s   (range \(.summary.cold_min_seconds | .*100 | round / 100)–\(.summary.cold_max_seconds | .*100 | round / 100)s)",
  "populating build           \(.summary.populate_seconds | .*100 | round / 100)s, uploaded \(.summary.bytes_uploaded_populating / 1048576 | round) MiB" + (if .populating_build_is_valid then "" else "   [INVALID: cache was not empty]" end),
  "rebuild from KilnCache     median \(.summary.cached_median_seconds | .*100 | round / 100)s   (range \(.summary.cached_min_seconds | .*100 | round / 100)–\(.summary.cached_max_seconds | .*100 | round / 100)s)",
  "",
  "median reduction           \(.summary.median_reduction_fraction * 1000 | round / 10)%",
  "median speedup             \(.summary.median_speedup | .*100 | round / 100)x",
  "outputs identical          \(.summary.outputs_identical)"
' "$REPORT"
