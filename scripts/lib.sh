# Shared helpers for the scripts in this directory. Sourced, not executed.

set -euo pipefail

repo_root() {
  git rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
}

iso_date() { date -u +%Y-%m-%d; }

host_short() { hostname -s 2>/dev/null || hostname; }

# results_dir echoes bench/results/<ISO-date>-<hostname> and creates it. Rule 1
# of CONTRIBUTING requires every published number to live under exactly this
# path, so no script is allowed to invent its own layout.
results_dir() {
  local root dir
  root="$(repo_root)"
  dir="${KILNCACHE_RESULTS_DIR:-$root/bench/results/$(iso_date)-$(host_short)}"
  mkdir -p "$dir"
  echo "$dir"
}

have() { command -v "$1" >/dev/null 2>&1; }

die() { echo "error: $*" >&2; exit 1; }

note() { echo "== $*" >&2; }
