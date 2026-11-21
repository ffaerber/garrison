#!/usr/bin/env bash
# Another machine for a Garrison installation: it joins the swarm as a
# worker and runs apps, those without data (docs/nodes.md). The platform
# and every app with data stay on the main machine.
#
# On the main machine, the command to run on the new one:
#   sudo ./install.sh --join-command
# On the new machine, with this repository checked out:
#   sudo ./install.sh --join ADDRESS --domain DOMAIN --token TOKEN --ca-sha256 FINGERPRINT [--mirror URL]
#
# Safe to run again: every step checks what already exists.
set -euo pipefail

say()  { printf '\n==> %s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "run as root (sudo)"

# ---------- on the main machine: the command for a new one ----------
if [ "${1:-}" = --join-command ]; then
  [ "$(docker info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null)" = true ] ||
    fail "--join-command runs on the main machine, where Garrison is installed"
  [ -s /etc/garrison/install.env ] || fail "Garrison is not installed here (no /etc/garrison/install.env)"
  domain=$(. /etc/garrison/install.env && echo "$DOMAIN")
  addr=$(docker info --format '{{.Swarm.NodeAddr}}')
  token=$(docker swarm join-token -q worker)
  # The new machine fetches the installation's certificate from Traefik and
  # trusts it only if it has this fingerprint.
  fp=$(openssl x509 -in /etc/garrison/tls.crt -noout -fingerprint -sha256 | cut -d= -f2)
  mirror=$(python3 -c 'import json; print((json.load(open("/etc/docker/daemon.json")).get("registry-mirrors") or [""])[0])' 2>/dev/null || true)
  echo "sudo ./install.sh --join $addr --domain $domain --token $token --ca-sha256 $fp${mirror:+ --mirror $mirror}"
  exit 0
fi

# ---------- on the new machine ----------
[ "${1:-}" = --join ] || fail "usage: install.sh --join ADDRESS --domain DOMAIN --token TOKEN --ca-sha256 FINGERPRINT [--mirror URL]"
ADDR=${2:-}
shift 2 || true
DOMAIN= TOKEN= FP= MIRROR=
while [ $# -gt 0 ]; do
  case $1 in
    --domain)    DOMAIN=${2:-}; shift 2 ;;
    --token)     TOKEN=${2:-}; shift 2 ;;
    --ca-sha256) FP=${2:-}; shift 2 ;;
    --mirror)    MIRROR=${2:-}; shift 2 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
[ -n "$ADDR" ] && [ -n "$DOMAIN" ] && [ -n "$TOKEN" ] && [ -n "$FP" ] ||
  fail "--join needs the address, --domain, --token and --ca-sha256; the main machine prints them: sudo ./install.sh --join-command"
. /etc/os-release
[ "${ID:-}" = ubuntu ] || fail "only Ubuntu is supported"

say "Installing Docker"
export DEBIAN_FRONTEND=noninteractive
need=()
command -v docker  >/dev/null || need+=(docker.io)
command -v openssl >/dev/null || need+=(openssl)
command -v python3 >/dev/null || need+=(python3)
apt-get() { command apt-get -o DPkg::Lock::Timeout=600 "$@"; }
if [ ${#need[@]} -gt 0 ]; then
  apt-get update -qq
  apt-get install -y -qq "${need[@]}" >/dev/null
fi
systemctl enable --now docker >/dev/null

restart=
# The main machine's pull-through mirror for Docker Hub, if it has one.
if [ -n "$MIRROR" ]; then
  changed=$(python3 - "$MIRROR" <<'PYTHON'
import json, os, sys
path = "/etc/docker/daemon.json"
try:
    with open(path) as f:
        cfg = json.load(f)
except FileNotFoundError:
    cfg = {}
if cfg.get("registry-mirrors") != [sys.argv[1]]:
    cfg["registry-mirrors"] = [sys.argv[1]]
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(cfg, f, indent=2)
        f.write("\n")
    print("yes")
PYTHON
)
  [ "$changed" = yes ] && restart=1
fi

say "Trusting the installation's registry"
# Apps' images are in Gitea's registry at git.<domain>, which Traefik serves
# on the main machine: Docker here pulls them from there.
sed -i '/# garrison registry$/d' /etc/hosts
echo "$ADDR git.$DOMAIN # garrison registry" >> /etc/hosts
if [ -d /etc/cloud/cloud.cfg.d ]; then
  printf '# Written by the Garrison installer: keep its /etc/hosts entry.\nmanage_etc_hosts: false\n' > /etc/cloud/cloud.cfg.d/99-garrison.cfg
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
openssl s_client -connect "$ADDR:443" -servername "git.$DOMAIN" </dev/null 2>/dev/null | openssl x509 > "$work/tls.crt" 2>/dev/null ||
  fail "cannot fetch the certificate of git.$DOMAIN from $ADDR:443"
got=$(openssl x509 -in "$work/tls.crt" -noout -fingerprint -sha256 | cut -d= -f2)
[ "$got" = "$FP" ] || fail "the certificate at $ADDR:443 has the fingerprint $got, not $FP: not joining"
install -D -m 644 "$work/tls.crt" "/etc/docker/certs.d/git.$DOMAIN/ca.crt"
# With the containerd image store, Docker also needs it in the system's store.
if ! cmp -s "$work/tls.crt" /usr/local/share/ca-certificates/garrison.crt; then
  install -D -m 644 "$work/tls.crt" /usr/local/share/ca-certificates/garrison.crt
  update-ca-certificates >/dev/null 2>&1
  restart=1
fi
# Before joining: a restart afterwards would bring Swarm's containers back
# without their secrets (host/repair-services.sh).
[ -z "$restart" ] || systemctl restart docker

say "Joining the swarm"
case $(docker info --format '{{.Swarm.LocalNodeState}}') in
  inactive)
    docker swarm join --token "$TOKEN" "$ADDR:2377" >/dev/null ;;
  active)
    [ "$(docker info --format '{{.Swarm.ControlAvailable}}')" != true ] || fail "this machine manages a swarm of its own"
    docker info --format '{{range .Swarm.RemoteManagers}}{{.Addr}} {{end}}' | grep -q "^$ADDR:\| $ADDR:" ||
      fail "this machine is already in another swarm"
    echo "  already in the swarm" ;;
  *) fail "Swarm is $(docker info --format '{{.Swarm.LocalNodeState}}') here; try again in a moment" ;;
esac

say "Done"
cat <<MSG
  $(hostname) runs apps of $DOMAIN without data now. On the main machine,
  docker node ls lists it.
MSG
