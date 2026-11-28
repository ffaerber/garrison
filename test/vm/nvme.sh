#!/usr/bin/env bash
# test/vm/nvme.sh [SIZE]: gives a test VM one virtual NVMe drive, as a real
# machine has, for Garrison's pool (POOL_DEVICE in test/answers): Multipass
# cannot attach disks, so it is a loop device, /dev/loop40, on a sparse file
# of SIZE (20G by default; a larger SIZE grows it). A systemd unit attaches
# it at every boot, before ZFS imports its pools. Runs on the VM, as root;
# safe to run again.
set -euo pipefail
size=${1:-20G}
dir=/var/lib/garrison-test
img=$dir/nvme0.img
dev=/dev/loop40
mkdir -p "$dir"
[ -e "$img" ] || truncate -s "$size" "$img"
# Grows, never shrinks: a smaller size leaves the file as it is.
if [ "$(stat -c %s "$img")" -lt "$(numfmt --from=iec "$size")" ]; then
  truncate -s "$size" "$img"
fi

cat > /usr/local/sbin/garrison-test-nvme <<EOF
#!/bin/sh
# Attaches the test VM's virtual NVMe drive (test/vm/nvme.sh).
set -e
[ -e $dev ] || mknod -m 660 $dev b 7 40
if losetup $dev >/dev/null 2>&1; then
  losetup -c $dev # picks up a grown file
else
  losetup $dev $img
fi
EOF
chmod 755 /usr/local/sbin/garrison-test-nvme

cat > /etc/systemd/system/garrison-test-nvme.service <<'EOF'
[Unit]
Description=The test VM's virtual NVMe drive (a loop device)
DefaultDependencies=no
After=systemd-remount-fs.service
Before=zfs-import-cache.service zfs-import-scan.service zfs-import.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/garrison-test-nvme

[Install]
WantedBy=zfs-import.target local-fs.target
EOF
systemctl daemon-reload
systemctl enable garrison-test-nvme.service >/dev/null 2>&1
systemctl restart garrison-test-nvme.service
# A pool on it grows with it.
for pool in $(zpool list -H -o name 2>/dev/null); do
  zpool online -e "$pool" "$dev" >/dev/null 2>&1 || true
done
lsblk -dn -o NAME,SIZE "$dev"
