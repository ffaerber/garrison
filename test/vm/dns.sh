#!/usr/bin/env bash
# Wildcard DNS for the test VM, so every hostname of the test installation
# opens in a browser on this computer without editing /etc/hosts.
#
#   test/vm/dns.sh        *.<domain> and <domain> resolve to the VM
#   test/vm/dns.sh --off  undo it on this computer
#
# A small DNS server in the VM answers for the domain, and systemd-resolved on
# this computer is told to ask it for that domain only. The setting lasts until
# the next reboot or until the VM is recreated; run the script again then.
set -euo pipefail

VM=${VM:-garrison-k3s}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck disable=SC1090
DOMAIN=$(. "$ROOT/${ANSWERS:-test/answers/selfsigned.env}" && echo "$DOMAIN")

ip=$(multipass info "$VM" | awk '/^IPv4:/ { print $2; exit }')
[ -n "$ip" ] || { echo "VM $VM has no address" >&2; exit 1; }
# The interface on this computer that reaches the VM.
link=$(ip -o route get "$ip" | sed -n 's/.* dev \([^ ]*\).*/\1/p')

if [ "${1:-}" = --off ]; then
  sudo resolvectl revert "$link"
  echo "DNS for $DOMAIN removed from $link"
  exit 0
fi

# Passed as an argument: `multipass exec` does not forward stdin reliably.
vm_script=$(cat <<'VMSCRIPT'
set -euo pipefail
domain=$1 ip=$2
# dnsmasq-base is the bare program; this script supplies its own service.
command -v dnsmasq >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq dnsmasq-base >/dev/null
rm -f /etc/dnsmasq.d/garrison-test.conf
cat > /etc/garrison-test-dns.conf <<CONF
# Answers only for the test domain, only on the VM's own address.
listen-address=$ip
bind-interfaces
no-resolv
no-hosts
address=/$domain/$ip
CONF
cat > /etc/systemd/system/garrison-test-dns.service <<UNIT
[Unit]
Description=Wildcard DNS for the Garrison test domain
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/sbin/dnsmasq --keep-in-foreground --conf-file=/etc/garrison-test-dns.conf
Restart=on-failure

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable garrison-test-dns >/dev/null 2>&1
# A clone of a VM this ran on starts the service with the other VM's
# address, which fails until systemd stops trying ("start request
# repeated too quickly"): that is cleared before it starts again.
systemctl reset-failed garrison-test-dns 2>/dev/null || true
systemctl restart garrison-test-dns
VMSCRIPT
)
multipass exec "$VM" -- sudo bash -c "$vm_script" setup "$DOMAIN" "$ip"

sudo resolvectl dns "$link" "$ip"
sudo resolvectl domain "$link" "~$DOMAIN"
echo "*.$DOMAIN now resolves to $ip (via $link)"
