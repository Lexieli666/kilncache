#!/usr/bin/env bash
#
# Inject latency and packet loss between KilnCache containers with tc netem.
#
# What this is for: every benchmark in this repository runs with the client and
# the servers on one host, so the inter-node hop costs microseconds. A real
# deployment has a network. Rather than claim the numbers transfer, one scenario
# is re-run with realistic latency and loss applied, and the result records that
# it was.
#
# How it works: the qdisc is applied *inside* each container's network
# namespace, on its own eth0, so it affects that node's outbound traffic to
# every peer. That needs NET_ADMIN, which the container does not have by
# default -- so the tc command runs in a separate privileged container that
# joins the target's network namespace. Nothing about the cache image changes.
#
# Usage:
#   scripts/netem.sh apply [delay] [jitter] [loss]   # defaults: 20ms 5ms 0.5%
#   scripts/netem.sh clear
#   scripts/netem.sh show
#   scripts/netem.sh verify                          # prove the delay is real
#
# Environment: KILNCACHE_COMPOSE_PROJECT (default kilncache),
#              KILNCACHE_COMPOSE_FILE (default deploy/compose/docker-compose.yml)

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

PROJECT="${KILNCACHE_COMPOSE_PROJECT:-kilncache}"
COMPOSE_FILE="${KILNCACHE_COMPOSE_FILE:-$(repo_root)/deploy/compose/docker-compose.yml}"
# A tiny image that has iproute2. Pinned so a benchmark re-run gets the same tc.
TC_IMAGE="${KILNCACHE_TC_IMAGE:-alpine:3.20}"

have docker || die "docker is required"

containers() {
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" ps -q 2>/dev/null
}

container_names() {
  local ids; ids="$(containers)"
  [ -n "$ids" ] || return 0
  # shellcheck disable=SC2086
  docker inspect --format '{{.Name}}' $ids | sed 's|^/||'
}

# in_netns runs a command inside a container's network namespace, with the
# privileges tc needs. --network container:<id> joins the namespace; --cap-add
# NET_ADMIN grants the capability there.
in_netns() {
  local target="$1"; shift
  docker run --rm \
    --network "container:$target" \
    --cap-add NET_ADMIN \
    "$TC_IMAGE" \
    sh -c "apk add --no-cache iproute2 >/dev/null 2>&1; $*"
}

cmd_apply() {
  local delay="${1:-20ms}" jitter="${2:-5ms}" loss="${3:-0.5%}"
  local names; names="$(container_names)"
  [ -n "$names" ] || die "no containers for project $PROJECT; start the cluster first"

  note "applying netem: delay ${delay} ± ${jitter}, loss ${loss}, to each node's eth0"
  local n
  for n in $names; do
    # `replace` rather than `add` so the command is idempotent.
    if in_netns "$n" "tc qdisc replace dev eth0 root netem delay $delay $jitter distribution normal loss $loss"; then
      note "  $n: applied"
    else
      note "  $n: FAILED -- tc needs NET_ADMIN in the container's namespace"
    fi
  done

  note ""
  note "Record this in the benchmark result, or the numbers are not interpretable:"
  note "  bin/bench -netem 'delay ${delay} +/- ${jitter}, loss ${loss}' ..."
}

cmd_clear() {
  local names; names="$(container_names)"
  [ -n "$names" ] || die "no containers for project $PROJECT"
  local n
  for n in $names; do
    in_netns "$n" "tc qdisc del dev eth0 root 2>/dev/null || true" >/dev/null 2>&1
    note "  $n: cleared"
  done
}

cmd_show() {
  local names; names="$(container_names)"
  [ -n "$names" ] || die "no containers for project $PROJECT"
  local n
  for n in $names; do
    echo "== $n"
    in_netns "$n" "tc qdisc show dev eth0" 2>/dev/null || echo "  (could not read)"
  done
}

# cmd_verify measures the round trip before and after, so a run made "under
# netem" has evidence that netem was actually in effect. An unverified claim
# that a fault was injected is the same class of error as an unmeasured number.
cmd_verify() {
  local names; names="$(container_names)"
  local first; first="$(echo "$names" | head -1)"
  [ -n "$first" ] || die "no containers for project $PROJECT"

  note "round-trip from $first to its peers (min/avg/max/mdev, milliseconds):"
  in_netns "$first" "apk add --no-cache iputils >/dev/null 2>&1; for h in node-a node-b node-c; do echo -n \"  \$h: \"; ping -c 5 -q \$h 2>/dev/null | grep 'rtt\\|round-trip' || echo 'unreachable'; done" \
    || note "  (ping unavailable)"
}

case "${1:-}" in
  apply)  shift; cmd_apply "$@" ;;
  clear)  cmd_clear ;;
  show)   cmd_show ;;
  verify) cmd_verify ;;
  *) sed -n '2,26p' "${BASH_SOURCE[0]}"; exit 1 ;;
esac
