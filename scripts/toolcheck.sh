#!/usr/bin/env bash
#
# Report the version of every external tool this repository depends on, and say
# which parts stop working when one is missing. Run it first on a new machine.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
set +e

row() {
  local name="$1" needed_for="$2"; shift 2
  local version
  if command -v "$name" >/dev/null 2>&1; then
    version=$("$@" 2>&1 | head -1)
    printf '  %-16s %-42s %s\n' "$name" "${version:0:42}" "$needed_for"
  else
    printf '  %-16s %-42s %s\n' "$name" "MISSING" "$needed_for"
  fi
}

echo "KilnCache toolchain"
printf '  %-16s %-42s %s\n' "TOOL" "VERSION" "NEEDED FOR"
row go             "everything"                        go version
row gofmt          "make fmt / make lint"              gofmt -h
row golangci-lint  "make lint"                         golangci-lint --version
row docker         "make compose-up / integration"     docker version --format '{{.Server.Version}}'
row jq             "benchmark and baseline scripts"    jq --version
row fio            "make device-baseline"              fio --version
row bazel          "make fixture / Phase 5 benchmark"  bazel --version
row tc             "scripts/netem.sh fault injection"  tc -V
row gh             "release and repository creation"   gh --version

if command -v docker >/dev/null 2>&1; then
  if docker compose version >/dev/null 2>&1; then
    printf '  %-16s %-42s %s\n' "docker compose" "$(docker compose version --short)" "three-node cluster"
  else
    printf '  %-16s %-42s %s\n' "docker compose" "MISSING" "three-node cluster"
  fi
fi
