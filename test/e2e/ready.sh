#!/usr/bin/env bash
# ready.sh SECONDS: waits until every pod of the platform runs, is ready, and
# has started since this machine booted, for up to SECONDS. Right after a
# boot, Kubernetes still shows the pods of before the shutdown as ready until
# the kubelet has looked again, so waiting for the Deployments alone returns
# at once.
set -uo pipefail
deadline=$((SECONDS + ${1:-300}))
not_ready() {
  kubectl --namespace garrison get pods --output json 2>/dev/null | python3 -c '
import json, sys, time
from datetime import datetime
boot = time.time() - float(open("/proc/uptime").read().split()[0])
bad = []
for p in json.load(sys.stdin)["items"]:
    if p["metadata"].get("deletionTimestamp") or p["status"].get("phase") not in ("Running", "Pending"):
        continue  # going, or a pod of before a reboot that a new one replaces
    cs = p["status"].get("containerStatuses") or []
    def fresh(c):
        run = (c.get("state") or {}).get("running")
        return c.get("ready") and run and datetime.fromisoformat(run["startedAt"].replace("Z", "+00:00")).timestamp() > boot
    if not cs or not all(fresh(c) for c in cs):
        bad.append(p["metadata"]["name"])
print(" ".join(bad))
' || echo "(the API does not answer)"
}
while :; do
  pending=$(not_ready)
  [ -z "$pending" ] && exit 0
  if [ $SECONDS -ge $deadline ]; then
    echo "not ready: $pending" >&2
    exit 1
  fi
  sleep 3
done
