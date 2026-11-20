#!/usr/bin/env bash
# revive-multipass.sh: brings Multipass's daemon back after it crashed.
# A crashed multipassd leaves its dnsmasq and VMs running in its cgroup;
# each restart then fails on dnsmasq's address ("Address already in use")
# until systemd gives up. This stops what is left over and starts it again.
# The VMs that were running stop; steps.sh starts them as it needs them.
set -euo pipefail
unit=snap.multipass.multipassd.service
if multipass version 2>/dev/null | grep -q '^multipassd'; then
  echo "Multipass's daemon answers; nothing to do."
  exit 0
fi
for pid in $(systemctl show -p ControlGroup --value "$unit" | xargs -I{} cat /sys/fs/cgroup{}/cgroup.procs 2>/dev/null); do
  echo "stopping left-over $(ps -o comm= -p "$pid" 2>/dev/null || echo "$pid")"
  sudo kill "$pid" 2>/dev/null || true
done
sleep 5
sudo systemctl reset-failed "$unit"
sudo snap start multipass.multipassd
for _ in $(seq 1 30); do
  multipass version 2>/dev/null | grep -q '^multipassd' && { multipass list; exit 0; }
  sleep 2
done
echo "Multipass's daemon did not come back; see: journalctl -u $unit" >&2
exit 1
