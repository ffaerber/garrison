#!/usr/bin/env bash
# Points the registry's name, git.<domain>, at the main machine in
# /etc/hosts, where the host's containerd (k3s) and Docker look it up to pull
# and push images through Traefik there: this machine on the main one
# (install.env), the main machine's address on the others that joined it
# (node.env, installer/join.sh). Run at boot by garrison-hosts.service, before
# k3s and Docker, as cloud-init or Multipass may write /etc/hosts anew at
# boot; and by the installer.
set -euo pipefail
env=/etc/garrison/install.env
[ -r "$env" ] || env=/etc/garrison/node.env
read -r domain address < <(. "$env" && echo "$DOMAIN ${MAIN_ADDRESS:-127.0.0.1}")
[ -n "$domain" ] || exit 0
line="$address git.$domain # garrison registry"
grep -qxF "$line" /etc/hosts && exit 0
sed -i '/# garrison registry$/d' /etc/hosts
echo "$line" >> /etc/hosts
echo "put back the hosts entry of git.$domain"
