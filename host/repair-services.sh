#!/usr/bin/env bash
# Replaces Swarm tasks whose container lacks its secrets or configs.
#
# When the Docker daemon restarts (a reboot, a package upgrade, a daemon.json
# change), Docker 29 restarts Swarm containers without mounting their secrets
# and configs: Traefik comes back without its certificate, Gitea without its
# passwords. A fresh task gets them again. Runs at boot (homecloud-repair.service)
# and on every install. Healthy services are left alone.
set -euo pipefail

# The installer points the registry's name at this machine in /etc/hosts, so
# Docker pulls app images through Traefik here. cloud-init, where present,
# rewrites /etc/hosts at boot; put the entry back.
if [ -s /etc/homecloud/install.env ]; then
  domain=$(. /etc/homecloud/install.env && echo "$DOMAIN")
  if [ -n "$domain" ] && ! grep -q "# homecloud registry$" /etc/hosts; then
    echo "127.0.0.1 git.$domain # homecloud registry" >> /etc/hosts
    echo "put back the hosts entry of git.$domain"
  fi
fi

# Wait until this node manages the swarm again.
for _ in $(seq 60); do
  [ "$(docker info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null)" = true ] && break
  sleep 2
done
[ "$(docker info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null)" = true ] || { echo "swarm is not available" >&2; exit 1; }

# The paths a service's containers must have: one secret and one config, if any.
expected() {
  docker service inspect "$1" --format '{{with .Spec.TaskTemplate.ContainerSpec}}{{range $i, $s := .Secrets}}{{if eq $i 0}}{{$s.File.Name}}{{"\n"}}{{end}}{{end}}{{range $i, $c := .Configs}}{{if eq $i 0}}{{$c.File.Name}}{{"\n"}}{{end}}{{end}}{{end}}'
}

for name in $(docker service ls --format '{{.Name}}'); do
  paths=$(expected "$name")
  [ -n "$paths" ] || continue
  broken=
  for c in $(docker ps -q --filter "label=com.docker.swarm.service.name=$name"); do
    while read -r p; do
      [ -n "$p" ] || continue
      case $p in /*) ;; *) p=/run/secrets/$p ;; esac
      # Looked up from the host, through the container's own root filesystem,
      # so images without a shell can be checked too. Needs root.
      pid=$(docker container inspect "$c" --format '{{.State.Pid}}' 2>/dev/null) || continue
      [ "$pid" -gt 0 ] && [ ! -e "/proc/$pid/root$p" ] && broken=1
    done <<<"$paths"
  done
  if [ -n "$broken" ]; then
    docker service update --quiet --detach --force "$name" >/dev/null
    echo "replaced $name: its container lacked its secrets or configs"
  fi
done
