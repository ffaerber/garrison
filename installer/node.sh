# What every machine of an installation needs, the main one (install.sh) and
# the others that join it (installer/join.sh): packages, k3s with its
# runtimes and registry, the ZFS pool, Sanoid and the registry's hosts entry.
# Sourced; uses say and fail of the script that sources it, and sets nothing
# else of its own but K3S_VERSION.

# k3s's version, the same on every machine.
K3S_VERSION=v1.37.2+k3s1

# apt waits for dpkg's lock instead of failing: on a freshly booted machine
# cloud-init or unattended-upgrades often holds it for a few minutes. The
# timeout covers dpkg's lock but not the package lists' one, which
# `apt-get update` takes: that is retried, for up to 10 minutes.
apt-get() {
  if [ "${1:-}" = update ]; then
    local i
    for i in $(seq 120); do
      command apt-get -o DPkg::Lock::Timeout=600 "$@" 2>/dev/null && return 0
      sleep 5
    done
  fi
  command apt-get -o DPkg::Lock::Timeout=600 "$@"
}

# node_packages [PACKAGE...]: installs what a machine needs, and the given
# packages, and gVisor (runsc) from its own repository.
node_packages() {
  export DEBIAN_FRONTEND=noninteractive
  local need=("$@")
  command -v zfs     >/dev/null || need+=(zfsutils-linux)
  command -v openssl >/dev/null || need+=(openssl)
  command -v curl    >/dev/null || need+=(curl)
  command -v rsync   >/dev/null || need+=(rsync)    # copies a dataset back from a snapshot
  command -v sanoid  >/dev/null || need+=(sanoid)   # scheduled snapshots
  command -v python3 >/dev/null || need+=(python3)
  command -v gpg     >/dev/null || need+=(gnupg)    # gVisor's repository key
  command -v crun    >/dev/null || need+=(crun)     # the pods' runtime (see k3s_node)
  if [ ${#need[@]} -gt 0 ]; then
    apt-get update -qq
    apt-get install -y -qq "${need[@]}" >/dev/null
  fi
  # gVisor (runsc): the agents' sandboxes run under its kernel in user space,
  # so code an agent runs as root in its sandbox meets gVisor before Linux.
  # From gVisor's own apt repository; Ubuntu has no package of it.
  if ! command -v runsc >/dev/null; then
    say "Installing gVisor"
    local arch; arch=$(dpkg --print-architecture)
    curl -fsSL https://gvisor.dev/archive.key | gpg --batch --yes --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
    echo "deb [arch=$arch signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
      > /etc/apt/sources.list.d/gvisor.list
    apt-get update -qq
    apt-get install -y -qq runsc >/dev/null
  fi
}

# k3s_node ROLE DOMAIN CA [CONFIG...]: installs k3s as ROLE (server, with
# k3s's own Traefik left out, or agent) with Garrison's settings and the
# given lines of config.yaml, e.g. "server: https://..." for an agent.
# containerd trusts CA (a file) for Gitea's registry at git.DOMAIN.
k3s_node() {
  local role=$1 domain=$2 ca=$3; shift 3
  local unit=k3s; [ "$role" = server ] || unit=k3s-agent
  mkdir -p /etc/rancher/k3s
  # containerd pulls from Gitea's registry at git.<domain> and trusts the
  # installation's certificate for it. Read when k3s starts.
  local registries
  registries=$(printf '# Written by the Garrison installer; changes are overwritten.\nconfigs:\n  "git.%s":\n    tls:\n      ca_file: %s\n' "$domain" "$ca")
  local restart=
  if [ "$(cat /etc/rancher/k3s/registries.yaml 2>/dev/null)" != "$registries" ]; then
    printf '%s\n' "$registries" > /etc/rancher/k3s/registries.yaml
    restart=1
  fi
  # Pods run with crun, not k3s's own runc: that runc (1.4.2) puts containers
  # under the AppArmor label cri-containerd.apparmor.d//&unconfined on Ubuntu
  # 26.04, which the profile does not let signal its own processes, so
  # Postgres could not stop its helpers and never started. crun, like
  # Ubuntu's runc, applies the profile as it is. k3s finds crun on the host.
  # The node keeps the name it was installed with, also when the machine's
  # hostname changes (a cloned test VM): a new name would be a new node, and
  # the pods would wait on the old one, unreachable, for minutes.
  local node_name
  node_name=$(sed -n 's/^node-name: //p' /etc/rancher/k3s/config.yaml 2>/dev/null || true)
  [ -n "$node_name" ] || node_name=$(hostname -s)
  # gVisor as a runtime of containerd, runsc, for the agents' sandboxes (the
  # RuntimeClass gvisor, garrison/platform). k3s writes containerd's config
  # from this template when it starts: its own, and this.
  local tmpl=/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl
  local want='{{ template "base" . }}

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"'
  if [ "$(cat "$tmpl" 2>/dev/null)" != "$want" ]; then
    mkdir -p "$(dirname "$tmpl")"
    printf '%s\n' "$want" > "$tmpl"
    restart=1
  fi
  local config
  config=$(printf '# Written by the Garrison installer; changes are overwritten.\ndefault-runtime: crun\nnode-name: %s\n' "$node_name"; [ $# -eq 0 ] || printf '%s\n' "$@")
  if [ "$(cat /etc/rancher/k3s/config.yaml 2>/dev/null)" != "$config" ]; then
    (umask 077; printf '%s\n' "$config" > /etc/rancher/k3s/config.yaml)
    restart=1
  fi
  # An installed k3s reads them when it starts again.
  [ -z "$restart" ] || ! systemctl is-active --quiet "$unit" || systemctl restart "$unit"
  if ! command -v k3s >/dev/null || [ "$(k3s --version | awk 'NR == 1 { print $3 }')" != "$K3S_VERSION" ] || ! systemctl is-enabled --quiet "$unit" 2>/dev/null; then
    say "Installing k3s $K3S_VERSION"
    local exec=$role
    [ "$role" != server ] || exec="server --disable traefik --write-kubeconfig-mode 600"
    curl -fsSL "https://raw.githubusercontent.com/k3s-io/k3s/${K3S_VERSION/+/%2B}/install.sh" -o /tmp/k3s-install.sh
    INSTALL_K3S_VERSION=$K3S_VERSION INSTALL_K3S_EXEC="$exec" sh /tmp/k3s-install.sh >/dev/null
    rm -f /tmp/k3s-install.sh
  fi
}

# zfs_pool POOL DEVICE SIZE: makes the pool POOL on DEVICE, or without one
# on a sparse file of SIZE on the root disk, and its dataset ns, which
# holds the units' data.
zfs_pool() {
  local pool=$1 device=$2 size=$3 vdev
  if ! zpool list "$pool" >/dev/null 2>&1; then
    if [ -n "$device" ]; then
      vdev=$device
    else
      vdev=/var/lib/garrison/pool.img
      mkdir -p /var/lib/garrison
      [ -e "$vdev" ] || truncate -s "$size" "$vdev"
    fi
    zpool create -o ashift=12 -O compression=lz4 -O atime=off -m "/$pool" "$pool" "$vdev"
  fi
  zfs list "$pool/ns" >/dev/null 2>&1 || zfs create "$pool/ns"
}

# sanoid_datasets DATASET...: scheduled snapshots of the datasets and all
# below them. Sanoid's timer takes and prunes them every 15 minutes. The
# deployer also snapshots an app before each redeploy, and keeps its own;
# restores use either.
sanoid_datasets() {
  mkdir -p /etc/sanoid
  {
    echo "# Written by the garrison installer; changes are overwritten."
    local ds
    for ds in "$@"; do printf '[%s]\n  use_template = garrison\n  recursive = yes\n\n' "$ds"; done
    cat <<'SANOID'
[template_garrison]
  frequently = 0
  hourly = 24
  daily = 30
  monthly = 6
  yearly = 0
  autosnap = yes
  autoprune = yes
SANOID
  } > /etc/sanoid/sanoid.conf
  # Sanoid caches its list of datasets; without --force-update a new app's
  # datasets would wait for the cache to expire before their first snapshot.
  mkdir -p /etc/systemd/system/sanoid.service.d
  cat > /etc/systemd/system/sanoid.service.d/garrison.conf <<UNIT
[Service]
ExecStart=
ExecStart=/usr/sbin/sanoid --take-snapshots --force-update --verbose
UNIT
  systemctl daemon-reload
  systemctl enable --now sanoid.timer >/dev/null
}

# registry_hosts ROOT: Gitea's registry, at git.<domain>, is used by the
# machine's containerd, which pulls the images (k3s_node), and on the main
# machine also by Docker, for the pipelines. Both reach it through Traefik
# on the main machine. cloud-init or Multipass may write /etc/hosts anew at
# boot: a unit puts the entry back each time, before k3s and Docker start
# (host/hosts.sh, from ROOT).
registry_hosts() {
  install -D -m 755 "$1/host/hosts.sh" /usr/local/lib/garrison/hosts.sh
  cat > /etc/systemd/system/garrison-hosts.service <<UNIT
[Unit]
Description=Garrison: point the registry's name at the main machine in /etc/hosts
After=local-fs.target cloud-init.service
Before=k3s.service k3s-agent.service docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/lib/garrison/hosts.sh

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable garrison-hosts >/dev/null 2>&1
  /usr/local/lib/garrison/hosts.sh >/dev/null
  if [ -d /etc/cloud/cloud.cfg.d ]; then
    printf '# Written by the Garrison installer: keep its /etc/hosts entry.\nmanage_etc_hosts: false\n' > /etc/cloud/cloud.cfg.d/99-garrison.cfg
  fi
}
