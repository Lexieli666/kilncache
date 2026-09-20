#!/usr/bin/env bash
#
# Record the machine a measurement was taken on.
#
# CONTRIBUTING rule 3 says a performance claim ships with its hardware line.
# This script produces that line as JSON so the benchmark driver embeds it
# verbatim, instead of a human retyping "8-core Linux workstation" into a README
# and hoping. Fields the kernel will not tell us are reported as "unknown"
# rather than guessed: a result that claims "NVMe SSD" when it was taken on a
# virtual disk is a false claim, not an approximation.
#
# Usage: scripts/hostinfo.sh [output.json|-]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

command -v jq >/dev/null 2>&1 || die "jq is required (paths on this host contain characters that must be JSON-escaped)"

out="${1:-}"
if [ -z "$out" ]; then
  out="$(results_dir)/hostinfo.json"
fi

root="$(repo_root)"

cpu_model=$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || true)
cpu_model=${cpu_model:-unknown}
cpu_cores=$(nproc 2>/dev/null || echo 0)
mem_kb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null || echo 0)

data_dir="${KILNCACHE_DATA_DIR:-$root}"
fs_type=$(df -PT "$data_dir" 2>/dev/null | awk 'NR==2{print $2}' || true)
fs_device=$(df -P "$data_dir" 2>/dev/null | awk 'NR==2{print $1}' || true)
fs_mount=$(df -P "$data_dir" 2>/dev/null | awk 'NR==2{print $NF}' || true)
mount_opts=$(awk -v m="$fs_mount" '$2==m{print $4; exit}' /proc/mounts 2>/dev/null || true)

disk_model="unknown"
base_dev=$(basename "${fs_device:-}" 2>/dev/null || true)
whole_dev="${base_dev%%[0-9]*}"
if [ -n "$whole_dev" ] && [ -r "/sys/block/${whole_dev}/device/model" ]; then
  disk_model=$(tr -d '\n' < "/sys/block/${whole_dev}/device/model" | sed 's/[[:space:]]*$//')
fi
rotational="unknown"
if [ -n "$whole_dev" ] && [ -r "/sys/block/${whole_dev}/queue/rotational" ]; then
  rotational=$(cat "/sys/block/${whole_dev}/queue/rotational")
fi

go_version=$( (have go && go version) || echo "not installed" )
docker_version=$( (have docker && docker version --format '{{.Server.Version}}' 2>/dev/null) || echo "not available" )
compose_version=$( (have docker && docker compose version --short 2>/dev/null) || echo "not available" )
# Report the Bazel version the benchmark fixture would actually use: bazelisk
# resolves .bazelversion relative to the workspace, so asking from the repo root
# would report whatever the latest release happens to be.
bazel_ws="$root"
[ -d "$root/fixtures/bazel-cpp" ] && bazel_ws="$root/fixtures/bazel-cpp"
bazel_version=$( (have bazel && (cd "$bazel_ws" && bazel --version 2>/dev/null | head -1)) || echo "not installed" )
fio_version=$( (have fio && fio --version) || echo "not installed" )

git_commit=$(git -C "$root" rev-parse --verify HEAD 2>/dev/null || echo "unknown")
if git -C "$root" status --porcelain 2>/dev/null | grep -q .; then git_dirty=true; else git_dirty=false; fi

virt="bare-metal"
grep -qi microsoft /proc/version 2>/dev/null && virt="wsl2"
[ -f /.dockerenv ] && virt="container"

jq -n \
  --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg hostname "$(hostname)" \
  --arg cpu_model "$cpu_model" \
  --argjson cpu_cores "${cpu_cores:-0}" \
  --argjson mem_total_kb "${mem_kb:-0}" \
  --arg kernel "$(uname -sr)" \
  --arg arch "$(uname -m)" \
  --arg virtualization "$virt" \
  --arg data_dir "$data_dir" \
  --arg filesystem "${fs_type:-unknown}" \
  --arg fs_device "${fs_device:-unknown}" \
  --arg fs_mount "${fs_mount:-unknown}" \
  --arg mount_options "${mount_opts:-unknown}" \
  --arg disk_model "$disk_model" \
  --arg rotational "$rotational" \
  --arg go_version "$go_version" \
  --arg docker_version "$docker_version" \
  --arg docker_compose_version "$compose_version" \
  --arg bazel_version "$bazel_version" \
  --arg fio_version "$fio_version" \
  --arg git_commit "$git_commit" \
  --argjson git_dirty "$git_dirty" \
  '$ARGS.named' > "${out%-}.tmp.$$" 2>/dev/null || die "jq failed"

if [ "$out" = "-" ]; then
  cat "${out%-}.tmp.$$"
  rm -f "${out%-}.tmp.$$"
else
  mv "${out}.tmp.$$" "$out"
  echo "wrote $out" >&2
  cat "$out"
fi
