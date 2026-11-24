#!/usr/bin/env bash
# Garrison installer. See docs/spec.md, "Installation".
#
#   sudo ./install.sh --answers FILE
#   sudo ./install.sh --join-command   on this machine: how another one joins
#   sudo ./install.sh --join ...       on another machine (installer/join.sh, docs/nodes.md)
#
# Safe to run again: every step checks what already exists.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
case ${1:-} in --join|--join-command) exec "$ROOT/installer/join.sh" "$@" ;; esac
ETC=/etc/garrison
POOL=garrison

say()  { printf '\n==> [%dm%02ds] %s\n' $((SECONDS / 60)) $((SECONDS % 60)) "$*"; }
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
LLM_BASE_URL=https://ollama.com/v1 LLM_MODEL=gpt-oss:120b LLM_API_KEY= TIMEZONE=
# shellcheck disable=SC1090
. "$ANSWERS"
# The time zone routines' schedules are read in; the machine's by default.
[ -n "$TIMEZONE" ] || TIMEZONE=$(timedatectl show -p Timezone --value 2>/dev/null || true)
[ -n "$TIMEZONE" ] || TIMEZONE=UTC
[ -n "$DOMAIN" ] || fail "answers file must set DOMAIN"
[ -n "$ADMIN_EMAIL" ] || ADMIN_EMAIL="$ADMIN_USER@$DOMAIN"
case $TLS_MODE in
  selfsigned) ;;
  letsencrypt) fail "TLS_MODE=letsencrypt (DNS challenge) is not built yet; use selfsigned" ;;
  *) fail "unknown TLS_MODE: $TLS_MODE" ;;
esac

# ---------- packages ----------
say "Installing Docker and ZFS"
. "$ROOT/installer/node.sh"
extra=()
command -v docker  >/dev/null || extra+=(docker.io)
command -v git     >/dev/null || extra+=(git)
command -v age-keygen >/dev/null || extra+=(age)   # the secrets' recovery key
node_packages "${extra[@]}"
systemctl enable --now docker >/dev/null

# Docker's settings: gVisor as the runtime runsc, and an optional
# pull-through mirror for Docker Hub images, e.g. to avoid its rate limit on
# anonymous pulls. Merged into daemon.json, keeping what else it says.
changed=$(python3 - "$REGISTRY_MIRROR" <<'PYTHON'
import json, os, sys
path = "/etc/docker/daemon.json"
try:
    with open(path) as f:
        cfg = json.load(f)
except FileNotFoundError:
    cfg = {}
before = json.dumps(cfg, sort_keys=True)
cfg.setdefault("runtimes", {})["runsc"] = {"path": "/usr/bin/runsc"}
if sys.argv[1]:
    cfg["registry-mirrors"] = [sys.argv[1]]
if json.dumps(cfg, sort_keys=True) != before:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(cfg, f, indent=2)
        f.write("\n")
    print("yes")
PYTHON
)
if [ "$changed" = yes ] || ! docker info --format '{{range $k, $v := .Runtimes}}{{$k}} {{end}}' | grep -qw runsc; then
  systemctl restart docker
fi
docker info --format '{{range $k, $v := .Runtimes}}{{$k}} {{end}}' | grep -qw runsc || fail "Docker does not know the runtime runsc (gVisor)"

# ---------- Kubernetes ----------
# k3s, a Kubernetes in one binary, runs the platform and the apps, in its
# own containerd (installer/node.sh). Docker stays for what still needs it:
# the pipelines' runners and BuildKit. Other machines join as k3s agents
# (installer/join.sh).
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
k3s_node server "$DOMAIN" "$ETC/tls.crt"
say "Waiting for Kubernetes"
deadline=$((SECONDS + 300))
until kubectl get --raw /readyz >/dev/null 2>&1 && kubectl wait --for=condition=Ready node --all --timeout=10s >/dev/null 2>&1; do
  [ $SECONDS -lt $deadline ] || fail "k3s did not become ready within 5 minutes; see journalctl -u k3s"
  sleep 3
done
kubectl get namespace garrison >/dev/null 2>&1 || kubectl create namespace garrison >/dev/null
# The pipelines' containers and BuildKit meet on this Docker network.
# Fixed ranges, which the platform's NetworkPolicies name (garrison/platform):
# jobs may reach Gitea, sandboxes the agent service, and nothing else of it.
docker_net() {
  local name=$1 subnet=$2; shift 2
  if docker network inspect "$name" >/dev/null 2>&1; then
    [ "$(docker network inspect "$name" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')" = "$subnet" ] && return 0
    docker network rm "$name" >/dev/null || fail "Docker network $name is in use with another range; stop its containers and run the installer again"
  fi
  docker network create --subnet "$subnet" "$@" "$name" >/dev/null
}
docker_net garrison-builds 10.200.0.0/24
# The agents' sandboxes were Docker containers on this network before they were pods.
! docker network inspect garrison-sandboxes >/dev/null 2>&1 || docker network rm garrison-sandboxes >/dev/null 2>&1 || true

# ---------- storage ----------
say "Setting up the ZFS pool"
zfs_pool "$POOL" "$POOL_DEVICE" "$POOL_SIZE"
for ds in system system/postgres system/gitea system/gitea-runner; do
  zfs list "$POOL/$ds" >/dev/null 2>&1 || zfs create "$POOL/$ds"
done
sanoid_datasets "$POOL/system" "$POOL/ns"

# ---------- secrets ----------
say "Generating secrets"
mkdir -p "$ETC"
chmod 700 "$ETC"
# Kubernetes Secrets in the namespace garrison, one key each named like the
# Secret: a pod mounts the ones it needs at /run/secrets/<name>.
. "$ROOT/installer/lib.sh"
for s in postgres-password keycloak-db-password gitea-db-password grafana-db-password garrison-db-password alerts-token gitea-oidc-secret sso-oidc-secret grafana-oidc-secret openwebui-oidc-secret openwebui-secret-key; do
  has_secret "$s" || openssl rand -hex 32 | tr -d '\n' | put_secret "$s"
done
# The login proxy needs a cookie secret of exactly 32 bytes.
has_secret sso-cookie-secret || openssl rand -hex 16 | tr -d '\n' | put_secret sso-cookie-secret

# The admin password is kept on disk, root-only, so a re-run can use it.
if [ ! -s "$ETC/admin-password" ]; then
  (umask 077; printf '%s' "${ADMIN_PASSWORD:-$(openssl rand -hex 16)}" > "$ETC/admin-password")
fi
has_secret admin-password || put_secret admin-password < "$ETC/admin-password"

if [ ! -s "$ETC/tls.crt" ]; then
  (umask 077; openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
    -subj "/CN=$DOMAIN" -addext "subjectAltName=DNS:$DOMAIN,DNS:*.$DOMAIN" \
    -keyout "$ETC/tls.key" -out "$ETC/tls.crt" 2>/dev/null)
fi
# One Secret of type tls, keys tls.crt and tls.key: Traefik's default
# certificate; other pods mount only tls.crt, to trust it.
has_secret tls || kubectl --namespace garrison create secret tls tls --cert "$ETC/tls.crt" --key "$ETC/tls.key" >/dev/null

cat > "$ETC/install.env" <<ENV
DOMAIN=$DOMAIN
TLS_MODE=$TLS_MODE
ADMIN_USER=$ADMIN_USER
ADMIN_EMAIL=$ADMIN_EMAIL
LLM_BASE_URL=$LLM_BASE_URL
LLM_MODEL=$LLM_MODEL
TIMEZONE=$TIMEZONE
ENV

# The agents' LLM key, root-only. It may also be put in this file by hand:
# only the agent service reads it, on each request.
touch "$ETC/llm-api-key"
chmod 600 "$ETC/llm-api-key"
if [ -n "$LLM_API_KEY" ]; then
  printf '%s' "$LLM_API_KEY" > "$ETC/llm-api-key"
fi
# The keys of further LLM providers (llm/providers.yaml in garrison's config),
# a file each, named after the provider. The install's LLM keeps the file
# above.
mkdir -p "$ETC/llm-keys"
chmod 700 "$ETC/llm-keys"
install -m 644 "$ROOT/garrison/platform/reserved-hostnames.yaml" "$ETC/reserved-hostnames.yaml"

# The units' secrets are encrypted for their own keys, which the
# deployer makes here, and for the recovery key. Only its public half stays;
# the private half is shown once, when the install is done, to be kept
# offline: it decrypts every secret should the machine be lost.
mkdir -p "$ETC/sops"
chmod 700 "$ETC/sops"
if [ ! -s "$ETC/sops/recovery.pub" ]; then
  (umask 077; age-keygen -o "$ETC/sops/recovery-key.txt" 2>/dev/null)
  age-keygen -y "$ETC/sops/recovery-key.txt" > "$ETC/sops/recovery.pub"
fi

registry_hosts "$ROOT"
# Earlier versions repaired Swarm's tasks at boot.
if [ -e /etc/systemd/system/garrison-repair.service ]; then
  systemctl disable garrison-repair >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/garrison-repair.service /usr/local/lib/garrison/repair-services.sh
fi
install -D -m 644 "$ETC/tls.crt" "/etc/docker/certs.d/git.$DOMAIN/ca.crt"
# With the containerd image store, Docker fetches registry tokens without
# certs.d, so a self-signed certificate must also be in the system's trust
# store. Docker reads that store when it starts.
if [ "$TLS_MODE" = selfsigned ] && ! cmp -s "$ETC/tls.crt" /usr/local/share/ca-certificates/garrison.crt; then
  install -D -m 644 "$ETC/tls.crt" /usr/local/share/ca-certificates/garrison.crt
  update-ca-certificates >/dev/null 2>&1
  systemctl restart docker
fi
# Inside the cluster, every name of the installation is Traefik's Service,
# as outside it is this machine: one URL for Keycloak, Gitea and the apps
# everywhere. k3s's CoreDNS reads *.override files of this ConfigMap, but
# only when it starts: its reload watches the Corefile alone, and a pod that
# started before the file reached it never rewrites. So a changed ConfigMap
# restarts it.
domain_re=$(printf '%s' "$DOMAIN" | sed 's/\./\\./g')
coredns=$(kubectl apply -f - <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns-custom
  namespace: kube-system
data:
  garrison.override: |
    rewrite stop {
      name regex ^(.+\.)?$domain_re\.$ traefik.garrison.svc.cluster.local.
      answer auto
    }
YAML
)
if ! grep -q unchanged <<<"$coredns"; then
  # k3s deploys CoreDNS on its own, a moment after its API answers.
  for _ in $(seq 120); do
    kubectl --namespace kube-system get deployment coredns >/dev/null 2>&1 && break
    sleep 1
  done
  kubectl --namespace kube-system rollout restart deployment coredns >/dev/null
  kubectl --namespace kube-system rollout status deployment coredns --timeout 300s >/dev/null
fi

# ---------- system stack ----------
# wait_stack NAME: waits until every Deployment, StatefulSet and DaemonSet
# of the stack has rolled out.
wait_stack() {
  local w
  for w in $(kubectl --namespace garrison get deployments,statefulsets,daemonsets -l "garrison.io/stack=$1" -o name); do
    kubectl --namespace garrison rollout status "$w" --timeout 600s >/dev/null || {
      kubectl --namespace garrison get pods -l "garrison.io/stack=$1" >&2 || true
      fail "stack $1 did not become ready: $w"
    }
  done
}

# The installer deploys only what is needed to build and run the deployer:
# the database, routing, login, Gitea and its Actions runner. Each is a repo of
# the `garrison` organisation (see garrison/). Then the deployer takes over and
# deploys every repo of that organisation, these included.
# platform first: its NetworkPolicies say who reaches whom from the start;
# then openebs, which the apps' volumes need.
BOOTSTRAP="platform openebs postgres traefik keycloak sso gitea runner"

# The host has the CLI too: it applies these stacks as the deployer later
# does, and stays for admins (garrison applied, apply, ...).
# Interim: it is built into bin/ before installing; it will ship as a release binary.
if [ -x "$ROOT/bin/garrison" ]; then
  install -m 755 "$ROOT/bin/garrison" /usr/local/bin/garrison
fi
command -v garrison >/dev/null || fail "garrison binary missing: run 'make build' first"

export DOMAIN ADMIN_USER ADMIN_EMAIL ETC
for name in $BOOTSTRAP; do
  dir=$ROOT/garrison/$name
  say "Deploying $name"
  garrison system -wait 0 "$dir" || fail "stack $name was not applied"
  wait_stack "$name"
  # Optional step that needs the running service, e.g. creating accounts.
  if [ -x "$dir/configure.sh" ]; then
    "$dir/configure.sh"
    wait_stack "$name"
  fi
done

# ---------- BuildKit, for the user units' pipelines ----------
# It pushes to Gitea's registry at its public name, which it reaches on the
# host through Traefik and trusts with the installation's certificate.
{
  echo "# Written by the Garrison installer."
  echo "[registry.\"git.$DOMAIN\"]"
  echo "  ca = [\"/etc/garrison/ca.crt\"]"
  if [ -n "$REGISTRY_MIRROR" ]; then
    echo "[registry.\"docker.io\"]"
    echo "  mirrors = [\"${REGISTRY_MIRROR#https://}\"]"
  fi
  echo "[worker.oci]"
  echo "  max-parallelism = 2"
} > "$ETC/buildkitd.toml"
install -D -m 755 "$ROOT/host/buildkit.sh" /usr/local/lib/garrison/buildkit.sh
cat > /etc/systemd/system/garrison-buildkit.service <<UNIT
[Unit]
Description=Garrison: BuildKit for the user units' pipelines
After=docker.service
Requires=docker.service

[Service]
ExecStartPre=-/usr/bin/docker rm -f garrison-buildkit
ExecStart=/usr/local/lib/garrison/buildkit.sh
ExecStop=/usr/bin/docker stop garrison-buildkit
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable garrison-buildkit >/dev/null 2>&1
systemctl restart garrison-buildkit

# ---------- control plane ----------
say "Building the control plane in Gitea"
# Pushing garrison/control-plane started its build in Gitea Actions. The build
# pushes the image to Gitea's registry and sets its tag in the repo's
# kubernetes.yaml; until then the tag is "bootstrap".
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
. "$ROOT/installer/lib.sh"
gitea_session "$work"
deadline=$((SECONDS + 1800))
until [ "$(gitea_api GET /repos/garrison/control-plane/raw/kubernetes.yaml)" = 200 ] && ! grep -q 'garrison/control-plane:bootstrap' "$work/body"; do
  gitea_api GET "/repos/garrison/control-plane/actions/tasks?limit=1" >/dev/null
  if grep -q '"status":"failure"' "$work/body"; then
    run=$(grep -o '"id":[0-9]*' "$work/body" | head -n1 | cut -d: -f2)
    gitea_api GET "/repos/garrison/control-plane/actions/runs/$run/jobs" >/dev/null
    job=$(grep -o '"id":[0-9]*' "$work/body" | head -n1 | cut -d: -f2)
    gitea_api GET "/repos/garrison/control-plane/actions/jobs/$job/logs" >/dev/null
    grep -vE 'evaluat|Writing entry|Extracting' "$work/body" | tail -n 15 >&2
    fail "the control plane build failed; see https://git.$DOMAIN/garrison/control-plane/actions"
  fi
  if [ $SECONDS -ge $deadline ]; then
    gitea_api GET "/repos/garrison/control-plane/actions/tasks?limit=3" >/dev/null
    echo "latest builds: $(cat "$work/body")" >&2
    fail "the control plane was not built within 30 minutes"
  fi
  sleep 10
done
echo "  built $(grep -oE 'garrison/control-plane:[A-Za-z0-9._-]+' "$work/body" | head -n1)"

say "Handing over to the deployer"
# The deployer deploys once here, from the host, so that failures stop the install.
# Earlier versions ran the deployer as a host service; it now runs in Swarm.
if [ -e /etc/systemd/system/garrison-controller.service ]; then
  systemctl disable --now garrison-controller >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/garrison-controller.service
  systemctl daemon-reload
fi
mkdir -p -m 700 /var/lib/garrison/deployer
garrison deploy -once
for name in $(garrison applied | awk '{ print $1 }'); do
  wait_stack "$name"
done
image=$(kubectl --namespace garrison get deployment deployer -o jsonpath='{.spec.template.spec.containers[0].image}')
case $image in "git.$DOMAIN/garrison/control-plane:"*) ;; *) fail "the deployer runs $image, not an image built in Gitea" ;; esac
echo "  the deployer runs $image"

say "Done"
cat <<MSG
  Keycloak  https://keycloak.$DOMAIN  (people sign in to the realm "garrison")
  Gitea     https://git.$DOMAIN
  Chat      https://chat.$DOMAIN  (one agent per unit)
  Grafana   https://grafana.$DOMAIN
  Admin     $ADMIN_USER (password in $ETC/admin-password)
  TLS       self-signed; certificate in $ETC/tls.crt
MSG
# Shown once: the file goes now.
if [ -s "$ETC/sops/recovery-key.txt" ]; then
  cat <<MSG

  Recovery key for every secret. Keep it offline, e.g. printed or in a
  password manager; it is not kept on this machine and is not shown again.

$(grep -v '^#' "$ETC/sops/recovery-key.txt" | sed 's/^/    /')

  With it, \`SOPS_AGE_KEY_FILE=<file holding it> sops decrypt secrets/<name>.yaml\`
  in a unit's config repo shows a secret.
MSG
  rm -f "$ETC/sops/recovery-key.txt"
fi
