#!/usr/bin/env bash
# Another machine for a Garrison installation: it joins the cluster as a k3s
# agent with a ZFS pool of its own, and runs apps, their data on its pool
# (OpenEBS ZFS LocalPV), and agents' sandboxes (docs/nodes.md). The platform
# stays on the main machine.
#
# On the main machine, the command to run on the new one:
#   sudo ./install.sh --join-command
# On the new machine, with this repository checked out:
#   sudo ./install.sh --join ADDRESS --domain DOMAIN --token TOKEN --ca-sha256 FINGERPRINT
#        [--pool-device DEVICE] [--pool-size SIZE]
#
# Safe to run again: every step checks what already exists.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ETC=/etc/garrison
POOL=garrison

say()  { printf '\n==> [%dm%02ds] %s\n' $((SECONDS / 60)) $((SECONDS % 60)) "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "run as root (sudo)"

# ---------- on the main machine: the command for a new one ----------
if [ "${1:-}" = --join-command ]; then
  [ -s "$ETC/install.env" ] || fail "--join-command runs on the main machine, where Garrison is installed"
  domain=$(. "$ETC/install.env" && echo "$DOMAIN")
  # The address the other machines reach this one at: the one of its
  # default route.
  addr=$(ip -4 route get 1.1.1.1 | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }')
  # k3s's token for joining; it also names the cluster's CA, so the agent
  # trusts only this cluster.
  token=$(cat /var/lib/rancher/k3s/server/node-token)
  # The new machine fetches the installation's certificate from Traefik and
  # trusts it only if it has this fingerprint.
  fp=$(openssl x509 -in "$ETC/tls.crt" -noout -fingerprint -sha256 | cut -d= -f2)
  echo "sudo ./install.sh --join $addr --domain $domain --token $token --ca-sha256 $fp"
  exit 0
fi

# ---------- on the new machine ----------
[ "${1:-}" = --join ] || fail "usage: install.sh --join ADDRESS --domain DOMAIN --token TOKEN --ca-sha256 FINGERPRINT [--pool-device DEVICE] [--pool-size SIZE]"
ADDR=${2:-}
shift 2 || true
DOMAIN= TOKEN= FP= POOL_DEVICE= POOL_SIZE=20G
while [ $# -gt 0 ]; do
  case $1 in
    --domain)      DOMAIN=${2:-}; shift 2 ;;
    --token)       TOKEN=${2:-}; shift 2 ;;
    --ca-sha256)   FP=${2:-}; shift 2 ;;
    --pool-device) POOL_DEVICE=${2:-}; shift 2 ;;
    --pool-size)   POOL_SIZE=${2:-}; shift 2 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
[ -n "$ADDR" ] && [ -n "$DOMAIN" ] && [ -n "$TOKEN" ] && [ -n "$FP" ] ||
  fail "--join needs the address, --domain, --token and --ca-sha256; the main machine prints them: sudo ./install.sh --join-command"
. /etc/os-release
[ "${ID:-}" = ubuntu ] || fail "only Ubuntu is supported"
[ ! -s "$ETC/install.env" ] || fail "Garrison's main machine is installed here; it cannot join another"

say "Installing ZFS, gVisor and crun"
. "$ROOT/installer/node.sh"
node_packages

say "Trusting the installation's registry"
mkdir -p "$ETC"
printf '# Written by installer/join.sh.\nDOMAIN=%s\nMAIN_ADDRESS=%s\n' "$DOMAIN" "$ADDR" > "$ETC/node.env"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
openssl s_client -connect "$ADDR:443" -servername "git.$DOMAIN" </dev/null 2>/dev/null | openssl x509 > "$work/tls.crt" 2>/dev/null ||
  fail "cannot fetch the certificate of git.$DOMAIN from $ADDR:443"
got=$(openssl x509 -in "$work/tls.crt" -noout -fingerprint -sha256 | cut -d= -f2)
[ "$got" = "$FP" ] || fail "the certificate at $ADDR:443 has the fingerprint $got, not $FP: not joining"
install -m 644 "$work/tls.crt" "$ETC/tls.crt"
registry_hosts "$ROOT"

say "Setting up the ZFS pool"
zfs_pool "$POOL" "$POOL_DEVICE" "$POOL_SIZE"
sanoid_datasets "$POOL/ns"

say "Joining the cluster"
k3s_node agent "$DOMAIN" "$ETC/tls.crt" "server: https://$ADDR:6443" "token: $TOKEN"
deadline=$((SECONDS + 300))
until systemctl is-active --quiet k3s-agent; do
  [ $SECONDS -lt $deadline ] || fail "k3s-agent did not start within 5 minutes; see journalctl -u k3s-agent"
  sleep 3
done

say "Done"
cat <<MSG
  $(sed -n 's/^node-name: //p' /etc/rancher/k3s/config.yaml) is a machine of $DOMAIN now. On the main machine,
  kubectl get nodes lists it; once it is Ready, apps and sandboxes may run here.
MSG
