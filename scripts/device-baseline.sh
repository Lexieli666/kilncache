#!/usr/bin/env bash
#
# Measure what the storage device underneath the cache can do, before measuring
# what the cache does with it.
#
# Why this exists: "KilnCache sustains N MiB/s" is only interesting next to what
# the disk sustains. A cache that hits 90% of the device's sequential write
# bandwidth is a good cache; one that hits 9% has a bug. Three fio jobs, chosen
# to bracket the three access patterns the store actually produces:
#
#   1. 4K random write, QD1, fdatasync every write  -> the publish path. Every
#      object write ends in fsync(file) + fsync(dir); this is that cost.
#   2. 4K random read,  QD32                        -> metadata and small-object
#      GET under concurrency.
#   3. 1M sequential write, QD8                     -> large-object PUT streaming.
#
# Usage: scripts/device-baseline.sh [target-dir]
#
# The target directory matters more than usual on this host: a WSL2 checkout on
# /mnt/d is a 9p mount whose numbers say nothing about the ext4 volume the
# containers use. The script refuses to publish a baseline taken on 9p unless
# KILNCACHE_ALLOW_SLOW_FS=1 is set, and records the filesystem either way.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

have fio || die "fio is not installed. Build it user-local with: curl -fsSL https://github.com/axboe/fio/archive/refs/tags/fio-3.38.tar.gz | tar xz && cd fio-fio-3.38 && ./configure --prefix=\$HOME/.local && make -j && make install"
have jq  || die "jq is required"

target="${1:-${KILNCACHE_BASELINE_DIR:-$HOME/.cache/kilncache-baseline}}"
mkdir -p "$target"

fs_type=$(df -PT "$target" | awk 'NR==2{print $2}')
case "$fs_type" in
  9p|drvfs|cifs|fuse*|nfs*)
    if [ "${KILNCACHE_ALLOW_SLOW_FS:-0}" != "1" ]; then
      die "$target is on a $fs_type filesystem. Baselines taken there do not describe the device the cache runs on. Pass a path on a local filesystem, or set KILNCACHE_ALLOW_SLOW_FS=1 to record it anyway."
    fi
    note "WARNING: baseline is being taken on $fs_type; the result is recorded but is not a device baseline"
    ;;
esac

# Pick an asynchronous engine that this fio build actually has. A fio compiled
# without libaio-dev silently lacks libaio, and the failure mode is a hard error
# in the middle of a 60-second run rather than a fallback, so probe up front.
async_engine="${KILNCACHE_FIO_ENGINE:-}"
if [ -z "$async_engine" ]; then
  for candidate in io_uring libaio posixaio psync; do
    if fio --enghelp 2>/dev/null | tr -d '\t' | grep -qx "$candidate"; then
      async_engine="$candidate"
      break
    fi
  done
fi
[ -n "$async_engine" ] || die "fio reports no usable async io engine"
note "async io engine: $async_engine"

size="${KILNCACHE_BASELINE_SIZE:-512M}"
runtime="${KILNCACHE_BASELINE_RUNTIME:-20}"
outdir="$(results_dir)"
raw="$outdir/device-baseline.fio.json"
summary="$outdir/device-baseline.json"
scratch="$target/fio-scratch"
mkdir -p "$scratch"

note "fio baseline in $scratch (fs=$fs_type, size=$size, runtime=${runtime}s per job)"

# --group_reporting keeps one result row per job. --end_fsync=1 on the
# sequential job means the reported bandwidth includes getting the data to the
# device, not just into the page cache.
fio --output-format=json --output="$raw" \
  --directory="$scratch" \
  --filename_format='baseline.$jobnum' \
  --group_reporting=1 \
  --name=randwrite_4k_qd1_fsync \
    --rw=randwrite --bs=4k --iodepth=1 --ioengine=psync --fdatasync=1 \
    --size="$size" --runtime="$runtime" --time_based=1 --direct=0 --numjobs=1 \
  --name=randread_4k_qd32 \
    --stonewall --rw=randread --bs=4k --iodepth=32 --ioengine="$async_engine" \
    --size="$size" --runtime="$runtime" --time_based=1 --direct=1 --numjobs=1 \
  --name=seqwrite_1m_qd8 \
    --stonewall --rw=write --bs=1M --iodepth=8 --ioengine="$async_engine" \
    --size="$size" --runtime="$runtime" --time_based=1 --direct=0 --end_fsync=1 --numjobs=1

rm -rf "$scratch"

hostinfo=$("$(dirname "${BASH_SOURCE[0]}")/hostinfo.sh" - )

jq -n \
  --arg target "$target" \
  --arg filesystem "$fs_type" \
  --arg async_engine "$async_engine" \
  --argjson host "$hostinfo" \
  --argjson fio "$(cat "$raw")" \
  '{
     kind: "device-baseline",
     target: $target,
     filesystem: $filesystem,
     async_io_engine: $async_engine,
     fio_version: $fio."fio version",
     host: $host,
     jobs: [ $fio.jobs[] | {
       name: .jobname,
       read_iops_mean: .read.iops_mean,
       read_bw_kib_s: .read.bw,
       read_lat_ns_p50: (.read.clat_ns.percentile["50.000000"] // null),
       read_lat_ns_p99: (.read.clat_ns.percentile["99.000000"] // null),
       write_iops_mean: .write.iops_mean,
       write_bw_kib_s: .write.bw,
       write_lat_ns_p50: (.write.clat_ns.percentile["50.000000"] // null),
       write_lat_ns_p99: (.write.clat_ns.percentile["99.000000"] // null),
       sync_lat_ns_p50: (.sync.lat_ns.percentile["50.000000"] // null),
       sync_lat_ns_p99: (.sync.lat_ns.percentile["99.000000"] // null)
     } ]
   }' > "$summary"

note "wrote $raw"
note "wrote $summary"
jq -r '
  "device baseline on \(.filesystem) at \(.target)",
  (.jobs[] | "  \(.name): read \(.read_bw_kib_s // 0) KiB/s @ \(.read_iops_mean // 0 | floor) IOPS, write \(.write_bw_kib_s // 0) KiB/s @ \(.write_iops_mean // 0 | floor) IOPS")
' "$summary"
