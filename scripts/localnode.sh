#!/usr/bin/env bash
#
# Start, stop and check KilnCache nodes running directly on this host (no
# Docker), for benchmarking and for the Bazel end-to-end runs.
#
# It keeps a PID file per node instead of using pkill. `pkill -f kilncache`
# matches the command line of the shell that invoked it as well as the server,
# so it kills the caller -- a genuinely confusing failure the first time it
# happens. A PID file is boring and cannot do that.
#
# Data directories default to $HOME, never the repository: the checkout may be
# on a 9p mount whose fsync and rename semantics the store depends on and does
# not get there.
#
# Usage:
#   scripts/localnode.sh start <name> <port> [extra kilncache flags...]
#   scripts/localnode.sh start-cluster [base-port]      # three nodes, RF=2
#   scripts/localnode.sh stop <name>
#   scripts/localnode.sh stop-all
#   scripts/localnode.sh status
#   scripts/localnode.sh wipe <name>                    # stop and delete its data

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RUN_DIR="${KILNCACHE_RUN_DIR:-$HOME/.kilncache-run}"
DATA_BASE="${KILNCACHE_LOCAL_DATA:-$HOME/.kilncache-data}"
BIN="$(repo_root)/bin/kilncache"

mkdir -p "$RUN_DIR" "$DATA_BASE"

need_bin() {
  [ -x "$BIN" ] || die "$BIN not built. Run: make build"
}

pidfile() { echo "$RUN_DIR/$1.pid"; }
logfile() { echo "$RUN_DIR/$1.log"; }
portfile() { echo "$RUN_DIR/$1.port"; }

is_running() {
  local pf; pf="$(pidfile "$1")"
  [ -f "$pf" ] || return 1
  local pid; pid="$(cat "$pf")"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

wait_ready() {
  local port="$1" name="$2"
  for _ in $(seq 1 100); do
    if curl -fsS --max-time 2 "http://127.0.0.1:$port/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  echo "--- last 30 log lines for $name ---" >&2
  tail -30 "$(logfile "$name")" >&2 || true
  die "node $name did not become ready on port $port"
}

cmd_start() {
  need_bin
  local name="$1" port="$2"; shift 2
  if is_running "$name"; then
    note "node $name already running (pid $(cat "$(pidfile "$name")"), port $(cat "$(portfile "$name")" 2>/dev/null))"
    return 0
  fi
  local data="$DATA_BASE/$name"
  mkdir -p "$data"

  setsid "$BIN" \
    --node-name="$name" \
    --listen="127.0.0.1:$port" \
    --data-dir="$data" \
    "$@" > "$(logfile "$name")" 2>&1 < /dev/null &
  local pid=$!
  echo "$pid" > "$(pidfile "$name")"
  echo "$port" > "$(portfile "$name")"

  wait_ready "$port" "$name"
  note "node $name up on 127.0.0.1:$port (pid $pid, data $data)"
}

cmd_start_cluster() {
  need_bin
  local base="${1:-18080}"
  local a=$((base)) b=$((base+1)) c=$((base+2))
  local peers="node-a=http://127.0.0.1:$a,node-b=http://127.0.0.1:$b,node-c=http://127.0.0.1:$c"
  shift || true
  cmd_start node-a "$a" --peers="$peers" --replica-count=2 --max-bytes="${KILNCACHE_MAX_BYTES:-40GiB}" --dev "$@"
  cmd_start node-b "$b" --peers="$peers" --replica-count=2 --max-bytes="${KILNCACHE_MAX_BYTES:-40GiB}" --dev "$@"
  cmd_start node-c "$c" --peers="$peers" --replica-count=2 --max-bytes="${KILNCACHE_MAX_BYTES:-40GiB}" --dev "$@"
}

cmd_stop() {
  local name="$1"
  if ! is_running "$name"; then
    rm -f "$(pidfile "$name")"
    note "node $name is not running"
    return 0
  fi
  local pid; pid="$(cat "$(pidfile "$name")")"
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    note "node $name did not stop gracefully; sending SIGKILL"
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$(pidfile "$name")"
  note "node $name stopped"
}

cmd_stop_all() {
  shopt -s nullglob
  for pf in "$RUN_DIR"/*.pid; do
    cmd_stop "$(basename "$pf" .pid)"
  done
}

cmd_status() {
  shopt -s nullglob
  local any=0
  for pf in "$RUN_DIR"/*.pid; do
    any=1
    local name; name="$(basename "$pf" .pid)"
    local port; port="$(cat "$(portfile "$name")" 2>/dev/null || echo '?')"
    if is_running "$name"; then
      printf '  %-8s running  pid %-7s port %-6s %s\n' "$name" "$(cat "$pf")" "$port" \
        "$(curl -fsS --max-time 2 "http://127.0.0.1:$port/readyz" 2>/dev/null || echo 'unreachable')"
    else
      printf '  %-8s stopped  (stale pid file)\n' "$name"
    fi
  done
  [ "$any" -eq 1 ] || echo "  no nodes"
}

cmd_wipe() {
  local name="$1"
  cmd_stop "$name"
  rm -rf "${DATA_BASE:?}/$name"
  note "wiped $DATA_BASE/$name"
}

case "${1:-}" in
  start)         shift; cmd_start "$@" ;;
  start-cluster) shift; cmd_start_cluster "$@" ;;
  stop)          shift; cmd_stop "$@" ;;
  stop-all)      cmd_stop_all ;;
  status)        cmd_status ;;
  wipe)          shift; cmd_wipe "$@" ;;
  *) sed -n '2,25p' "${BASH_SOURCE[0]}"; exit 1 ;;
esac
