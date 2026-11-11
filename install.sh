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

DOMAIN= TLS_MODE=selfsigned ADMIN_USER=admin ADMIN_EMAIL= ADMIN_PASSWORD= POOL_DEVICE= POOL_SIZE=20G REGISTRY_MIRROR=
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
command -v git     >/dev/null || need+=(git)
if [ ${#need[@]} -gt 0 ]; then
  apt-get update -qq
  apt-get install -y -qq "${need[@]}" >/dev/null
fi
systemctl enable --now docker >/dev/null

# Optional pull-through mirror for Docker Hub images, e.g. to avoid its rate
# limit on anonymous pulls. Docker rejects unknown keys in daemon.json, so the
# installer marks the file it owns with a copy kept next to its own state.
if [ -n "$REGISTRY_MIRROR" ]; then
  want=$(printf '{\n  "registry-mirrors": ["%s"]\n}' "$REGISTRY_MIRROR")
  owned=$ETC/daemon.json
  mkdir -p "$ETC"
  if [ -e /etc/docker/daemon.json ] && ! cmp -s /etc/docker/daemon.json "$owned"; then
    fail "/etc/docker/daemon.json was not written by this installer; add the registry mirror to it by hand"
  fi
  if [ "$(cat /etc/docker/daemon.json 2>/dev/null)" != "$want" ]; then
    printf '%s\n' "$want" > /etc/docker/daemon.json
    cp /etc/docker/daemon.json "$owned"
    systemctl restart docker
    # Services of an existing install come back without their secrets and configs.
    [ "$(docker info --format '{{.Swarm.LocalNodeState}}')" = inactive ] || "$ROOT/host/repair-services.sh"
  fi
fi

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
for ds in system system/postgres system/gitea system/gitea-runner ns; do
  zfs list "$POOL/$ds" >/dev/null 2>&1 || zfs create "$POOL/$ds"
done

# ---------- secrets ----------
say "Generating secrets"
mkdir -p "$ETC"
chmod 700 "$ETC"
has_secret() { docker secret inspect "$1" >/dev/null 2>&1; }
for s in postgres-password keycloak-db-password gitea-db-password grafana-db-password gitea-oidc-secret sso-oidc-secret grafana-oidc-secret; do
  has_secret "$s" || openssl rand -hex 32 | tr -d '\n' | docker secret create "$s" - >/dev/null
done
# The login proxy needs a cookie secret of exactly 32 bytes.
has_secret sso-cookie-secret || openssl rand -hex 16 | tr -d '\n' | docker secret create sso-cookie-secret - >/dev/null

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
install -m 644 "$ROOT/system/platform/reserved-hostnames.yaml" "$ETC/reserved-hostnames.yaml"

# Gitea's registry, at git.<domain>, is used by this machine's Docker daemon:
# builds push to it and Swarm pulls from it. The daemon reaches it through
# Traefik here and trusts the installation's certificate.
sed -i '/# homecloud registry$/d' /etc/hosts
echo "127.0.0.1 git.$DOMAIN # homecloud registry" >> /etc/hosts
install -D -m 644 "$ETC/tls.crt" "/etc/docker/certs.d/git.$DOMAIN/ca.crt"
# With the containerd image store, Docker fetches registry tokens without
# certs.d, so a self-signed certificate must also be in the system's trust
# store. Docker reads that store when it starts.
if [ "$TLS_MODE" = selfsigned ] && ! cmp -s "$ETC/tls.crt" /usr/local/share/ca-certificates/homecloud.crt; then
  install -D -m 644 "$ETC/tls.crt" /usr/local/share/ca-certificates/homecloud.crt
  update-ca-certificates >/dev/null 2>&1
  systemctl restart docker
  # Services of an existing install come back without their secrets and configs.
  "$ROOT/host/repair-services.sh" >/dev/null
fi

# ---------- system stack ----------
# Wait until every service of a stack has all its replicas running and healthy
# and has finished rolling out. During an update the old container still counts
# as a running replica, so the replica count alone is not enough.
wait_stack() {
  local stack=$1 deadline=$((SECONDS + 600)) pending name replicas id update
  while :; do
    pending=
    while read -r id name replicas; do
      update=$(docker service inspect --format '{{if .UpdateStatus}}{{.UpdateStatus.State}}{{end}}' "$id")
      case $update in ""|completed) ;; *) pending+=" $name"; continue ;; esac
      [ "${replicas%/*}" = "${replicas#*/}" ] && [ "${replicas#*/}" != 0 ] || pending+=" $name"
    done < <(docker service ls --filter "label=com.docker.stack.namespace=$stack" --format '{{.ID}} {{.Name}} {{.Replicas}}')
    [ -z "$pending" ] && return 0
    if [ $SECONDS -ge $deadline ]; then
      # shellcheck disable=SC2086
      docker service ps --no-trunc $pending >&2 || true
      fail "stack $stack did not become ready:$pending"
    fi
    sleep 3
  done
}

# The installer deploys only what is needed to build and run the controller:
# the database, routing, login, Gitea and its Actions runner. Each is a repo of
# the `system` organisation (see system/). Then the controller takes over and
# deploys every repo of that organisation, these included.
BOOTSTRAP="postgres traefik keycloak sso gitea runner"

export DOMAIN ADMIN_USER ADMIN_EMAIL ETC
for name in $BOOTSTRAP; do
  dir=$ROOT/system/$name
  say "Deploying $name"
  # Swarm configs cannot change, so their names carry a hash of the stack's
  # files. The controller computes the same hash (controller/internal/system).
  CONFIG_HASH=$(export LC_ALL=C; cat "$dir"/* | sha256sum | cut -c1-12)
  export CONFIG_HASH
  # Images are not resolved to digests: versions are pinned by tag, and a
  # registry that is briefly unreachable must not change the services.
  docker stack deploy --detach=true --resolve-image never --compose-file "$dir/compose.yaml" "$name" >/dev/null
  wait_stack "$name"
  # Optional step that needs the running service, e.g. creating accounts.
  if [ -x "$dir/configure.sh" ]; then
    "$dir/configure.sh"
    wait_stack "$name"
  fi
done

# ---------- boot repair ----------
install -D -m 755 "$ROOT/host/repair-services.sh" /usr/local/lib/homecloud/repair-services.sh
cat > /etc/systemd/system/homecloud-repair.service <<UNIT
[Unit]
Description=Home Cloud: give Swarm services back their secrets and configs after Docker starts
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/lib/homecloud/repair-services.sh

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable homecloud-repair >/dev/null 2>&1
# Also now: a container left without its secrets by an earlier Docker restart
# is replaced instead of being taken for healthy.
/usr/local/lib/homecloud/repair-services.sh

# ---------- controller ----------
say "Building the controller in Gitea"
# Pushing system/controller started its build in Gitea Actions. The build
# pushes the image to Gitea's registry and sets its tag in the repo's
# compose.yaml; until then the tag is "bootstrap".
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
. "$ROOT/installer/lib.sh"
gitea_session "$work"
deadline=$((SECONDS + 1800))
until [ "$(gitea_api GET /repos/system/controller/raw/compose.yaml)" = 200 ] && ! grep -q 'system/controller:bootstrap' "$work/body"; do
  gitea_api GET "/repos/system/controller/actions/tasks?limit=1" >/dev/null
  if grep -q '"status":"failure"' "$work/body"; then
    run=$(grep -o '"id":[0-9]*' "$work/body" | head -n1 | cut -d: -f2)
    gitea_api GET "/repos/system/controller/actions/runs/$run/jobs" >/dev/null
    job=$(grep -o '"id":[0-9]*' "$work/body" | head -n1 | cut -d: -f2)
    gitea_api GET "/repos/system/controller/actions/jobs/$job/logs" >/dev/null
    grep -vE 'evaluat|Writing entry|Extracting' "$work/body" | tail -n 15 >&2
    fail "the controller build failed; see https://git.$DOMAIN/system/controller/actions"
  fi
  if [ $SECONDS -ge $deadline ]; then
    gitea_api GET "/repos/system/controller/actions/tasks?limit=3" >/dev/null
    echo "latest builds: $(cat "$work/body")" >&2
    fail "the controller was not built within 30 minutes"
  fi
  sleep 10
done
echo "  built $(grep -oE 'system/controller:[A-Za-z0-9._-]+' "$work/body" | head -n1)"

say "Handing over to the controller"
# The host has a controller CLI too: it deploys once here, so that failures stop
# the install, and stays for admins (homecloud-controller applied, apply, ...).
# Interim: it is built into bin/ before installing; it will ship as a release binary.
if [ -x "$ROOT/bin/controller" ]; then
  install -m 755 "$ROOT/bin/controller" /usr/local/bin/homecloud-controller
fi
command -v homecloud-controller >/dev/null || fail "controller binary missing: run 'make build' first"
# Earlier versions ran the controller as a host service; it now runs in Swarm.
if [ -e /etc/systemd/system/homecloud-controller.service ]; then
  systemctl disable --now homecloud-controller >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/homecloud-controller.service
  systemctl daemon-reload
fi
mkdir -p -m 700 /var/lib/homecloud/controller
homecloud-controller serve -once
for name in $(homecloud-controller applied | awk '{ print $1 }'); do
  wait_stack "$name"
done
image=$(docker service inspect controller_controller --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}')
case $image in "git.$DOMAIN/system/controller:"*) ;; *) fail "the controller runs $image, not an image built in Gitea" ;; esac
echo "  the controller runs $image"

say "Done"
cat <<MSG
  Keycloak  https://keycloak.$DOMAIN  (people sign in to the realm "homecloud")
  Gitea     https://git.$DOMAIN
  Grafana   https://grafana.$DOMAIN
  Admin     $ADMIN_USER (password in $ETC/admin-password)
  TLS       self-signed; certificate in $ETC/tls.crt
MSG
