#!/usr/bin/env bash
# Home Cloud Platform installer. See docs/spec.md, "Installation".
#
#   sudo ./install.sh --answers FILE
#
# Safe to run again: every step checks what already exists.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ETC=/etc/homecloud
POOL=homecloud

say()  { printf '\n==> %s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# ---------- arguments and answers ----------
ANSWERS=
while [ $# -gt 0 ]; do
  case $1 in
    --answers) ANSWERS=${2:-}; shift 2 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" = 0 ] || fail "run as root (sudo)"
. /etc/os-release
[ "${ID:-}" = ubuntu ] || fail "only Ubuntu is supported"
[ -n "$ANSWERS" ] || fail "the setup wizard is not built yet; pass --answers FILE (see test/answers)"
[ -r "$ANSWERS" ] || fail "cannot read answers file $ANSWERS"

DOMAIN= TLS_MODE=selfsigned ADMIN_USER=admin ADMIN_EMAIL= ADMIN_PASSWORD= POOL_DEVICE= POOL_SIZE=20G
# shellcheck disable=SC1090
. "$ANSWERS"
[ -n "$DOMAIN" ] || fail "answers file must set DOMAIN"
[ -n "$ADMIN_EMAIL" ] || ADMIN_EMAIL="$ADMIN_USER@$DOMAIN"
case $TLS_MODE in
  selfsigned) ;;
  letsencrypt) fail "TLS_MODE=letsencrypt (DNS challenge) is not built yet; use selfsigned" ;;
  *) fail "unknown TLS_MODE: $TLS_MODE" ;;
esac

# ---------- packages ----------
say "Installing Docker and ZFS"
export DEBIAN_FRONTEND=noninteractive
need=()
command -v docker  >/dev/null || need+=(docker.io)
command -v zfs     >/dev/null || need+=(zfsutils-linux)
command -v openssl >/dev/null || need+=(openssl)
command -v curl    >/dev/null || need+=(curl)
if [ ${#need[@]} -gt 0 ]; then
  apt-get update -qq
  apt-get install -y -qq "${need[@]}" >/dev/null
fi
systemctl enable --now docker >/dev/null

# ---------- swarm ----------
say "Setting up Docker Swarm"
state=$(docker info --format '{{.Swarm.LocalNodeState}}')
if [ "$state" = inactive ]; then
  addr=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
  docker swarm init --advertise-addr "$addr" >/dev/null
elif [ "$(docker info --format '{{.Swarm.ControlAvailable}}')" != true ]; then
  fail "this machine is already a worker in another swarm"
fi
for net in traefik-public homecloud-system; do
  docker network inspect "$net" >/dev/null 2>&1 || docker network create --driver overlay --attachable "$net" >/dev/null
done

# ---------- storage ----------
say "Setting up the ZFS pool"
if ! zpool list "$POOL" >/dev/null 2>&1; then
  if [ -n "$POOL_DEVICE" ]; then
    vdev=$POOL_DEVICE
  else
    # No dedicated device: back the pool with a sparse file on the root disk.
    vdev=/var/lib/homecloud/pool.img
    mkdir -p /var/lib/homecloud
    [ -e "$vdev" ] || truncate -s "$POOL_SIZE" "$vdev"
  fi
  zpool create -o ashift=12 -O compression=lz4 -O atime=off -m "/$POOL" "$POOL" "$vdev"
fi
for ds in system system/postgres system/gitea ns; do
  zfs list "$POOL/$ds" >/dev/null 2>&1 || zfs create "$POOL/$ds"
done

# ---------- secrets ----------
say "Generating secrets"
mkdir -p "$ETC"
chmod 700 "$ETC"
has_secret() { docker secret inspect "$1" >/dev/null 2>&1; }
for s in postgres-password keycloak-db-password gitea-db-password grafana-db-password; do
  has_secret "$s" || openssl rand -hex 32 | tr -d '\n' | docker secret create "$s" - >/dev/null
done

# The admin password is kept on disk, root-only, so a re-run can use it.
if [ ! -s "$ETC/admin-password" ]; then
  (umask 077; printf '%s' "${ADMIN_PASSWORD:-$(openssl rand -hex 16)}" > "$ETC/admin-password")
fi
has_secret admin-password || docker secret create admin-password "$ETC/admin-password" >/dev/null

if [ ! -s "$ETC/tls.crt" ]; then
  (umask 077; openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
    -subj "/CN=$DOMAIN" -addext "subjectAltName=DNS:$DOMAIN,DNS:*.$DOMAIN" \
    -keyout "$ETC/tls.key" -out "$ETC/tls.crt" 2>/dev/null)
fi
has_secret tls-cert || docker secret create tls-cert "$ETC/tls.crt" >/dev/null
has_secret tls-key  || docker secret create tls-key  "$ETC/tls.key" >/dev/null

cat > "$ETC/install.env" <<ENV
DOMAIN=$DOMAIN
TLS_MODE=$TLS_MODE
ADMIN_USER=$ADMIN_USER
ADMIN_EMAIL=$ADMIN_EMAIL
ENV
install -m 644 "$ROOT/platform/reserved-hostnames.yaml" "$ETC/reserved-hostnames.yaml"

# ---------- system stack ----------
# Wait until every service of a stack has all its replicas running and healthy.
wait_stack() {
  local stack=$1 deadline=$((SECONDS + 600)) pending
  while :; do
    pending=$(docker service ls --filter "label=com.docker.stack.namespace=$stack" --format '{{.Name}} {{.Replicas}}' \
      | awk '{ split($2, r, "/"); if (r[1] != r[2] || r[2] == 0) print $1 }')
    [ -z "$pending" ] && return 0
    if [ $SECONDS -ge $deadline ]; then
      docker service ps --no-trunc $pending >&2 || true
      fail "stack $stack did not become ready: $pending"
    fi
    sleep 3
  done
}

export DOMAIN ADMIN_USER
for dir in "$ROOT"/platform/system/[0-9]*/; do
  [ -f "$dir/compose.yaml" ] || continue
  name=$(basename "$dir"); name=${name#*-}
  say "Deploying $name"
  docker stack deploy --detach=true --compose-file "$dir/compose.yaml" "$name" >/dev/null
  wait_stack "$name"
done

# ---------- Gitea admin ----------
say "Creating the Gitea admin account"
gitea=$(docker ps --quiet --filter label=com.docker.swarm.service.name=gitea_gitea | head -n1)
[ -n "$gitea" ] || fail "Gitea container not found"
if ! docker exec -u git "$gitea" gitea admin user list --admin | awk 'NR > 1 { print $2 }' | grep -qx "$ADMIN_USER"; then
  docker exec -u git "$gitea" gitea admin user create --admin --must-change-password=false \
    --username "$ADMIN_USER" --email "$ADMIN_EMAIL" --password "$(cat "$ETC/admin-password")" >/dev/null
fi

# ---------- controller ----------
# Interim: the controller will ship as an image in the system stack.
if [ -x "$ROOT/bin/controller" ]; then
  install -m 755 "$ROOT/bin/controller" /usr/local/bin/homecloud-controller
fi

say "Done"
cat <<MSG
  Keycloak  https://auth.$DOMAIN
  Gitea     https://git.$DOMAIN
  Admin     $ADMIN_USER (password in $ETC/admin-password)
  TLS       self-signed; certificate in $ETC/tls.crt
MSG
