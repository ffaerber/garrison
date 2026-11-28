#!/bin/sh
# zfs-metrics.sh: every minute, this machine's ZFS pools and datasets as
# Prometheus metrics, into $OUT/zfs.prom, which node-exporter serves (its
# textfile collector). Runs in the zfs DaemonSet's pod, in the host's mount
# namespace for zfs and zpool. Each metric names the machine ($NODE_NAME),
# as Prometheus knows node-exporter only by its pod's address.
#
#   garrison_zfs_pool_{size,allocated,free}_bytes{machine,pool}
#   garrison_zfs_pool_fragmentation_percent{machine,pool}
#   garrison_zfs_pool_healthy{machine,pool,health}       1 if ONLINE
#   garrison_zfs_dataset_{used,available,snapshots,quota}_bytes{machine,dataset,unit,kind}
#       kind: app (an environment's data), share, moved (an old copy left
#       by a move), other; unit: from garrison/ns/<unit>/...; quota 0: none
#   garrison_zfs_metrics_updated_seconds{machine}
set -u
OUT=${OUT:-/metrics}
host() { nsenter -t 1 -m -- "$@"; }

# family NAME HELP: a gauge's header, then its lines from stdin.
family() {
  echo "# HELP $1 $2"
  echo "# TYPE $1 gauge"
  sed "s/^/$1/"
}

# pools AWK-EXPRESSION: "{labels} value" per pool, from zpool list.
pools() {
  printf '%s\n' "$POOLS" | awk -F'\t' -v m="$NODE_NAME" '
    NF >= 6 { l = "{machine=\"" m "\",pool=\"" $1 "\""; v = '"$1"'; if (v != "-") print l "} " v }'
}

# datasets FIELD: "{labels} value" per dataset, the value from FIELD of
# zfs list (2 used, 3 available, 4 snapshots, 5 quota).
datasets() {
  printf '%s\n' "$DATASETS" | awk -F'\t' -v m="$NODE_NAME" -v f="$1" '
    NF >= 5 {
      n = split($1, a, "/")
      unit = (a[2] == "ns" && n >= 3) ? a[3] : ""
      kind = "other"
      if ($1 ~ /\.moved-/) kind = "moved"
      else if ($1 ~ /\/stacks\//) kind = "app"
      else if ($1 ~ /\/shares\//) kind = "share"
      print "{machine=\"" m "\",dataset=\"" $1 "\",unit=\"" unit "\",kind=\"" kind "\"} " $f
    }'
}

mkdir -p "$OUT"
while :; do
  POOLS=$(host zpool list -Hp -o name,size,alloc,free,frag,health 2>/dev/null)
  DATASETS=$(host zfs list -Hp -t filesystem -o name,used,avail,usedbysnapshots,quota 2>/dev/null)
  if [ -n "$POOLS" ]; then
    {
      pools '$2' | family garrison_zfs_pool_size_bytes "Size of the ZFS pool."
      pools '$3' | family garrison_zfs_pool_allocated_bytes "Space allocated in the ZFS pool."
      pools '$4' | family garrison_zfs_pool_free_bytes "Free space in the ZFS pool."
      pools '$5' | family garrison_zfs_pool_fragmentation_percent "Fragmentation of the pool's free space."
      printf '%s\n' "$POOLS" | awk -F'\t' -v m="$NODE_NAME" 'NF >= 6 { print "{machine=\"" m "\",pool=\"" $1 "\",health=\"" $6 "\"} " ($6 == "ONLINE" ? 1 : 0) }' |
        family garrison_zfs_pool_healthy "1 if the pool is ONLINE; its health in the label."
      datasets 2 | family garrison_zfs_dataset_used_bytes "Space a dataset uses, with its children and snapshots."
      datasets 3 | family garrison_zfs_dataset_available_bytes "Space a dataset may still use: its quota's rest, or the pool's."
      datasets 4 | family garrison_zfs_dataset_snapshots_bytes "Space only a dataset's snapshots hold."
      datasets 5 | family garrison_zfs_dataset_quota_bytes "A dataset's quota; 0 for none."
      echo "{machine=\"$NODE_NAME\"} $(date +%s)" | family garrison_zfs_metrics_updated_seconds "When these ZFS metrics were written."
    } > "$OUT/zfs.prom.tmp" && mv "$OUT/zfs.prom.tmp" "$OUT/zfs.prom"
  fi
  sleep 60
done
