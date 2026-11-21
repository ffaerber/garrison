#!/usr/bin/env bash
# Points the registry's name, git.<domain>, at this machine in /etc/hosts,
# where the host's containerd (k3s) and Docker look it up to pull and push
# images through Traefik here. Run at boot by garrison-hosts.service, before
# k3s and Docker, as cloud-init or Multipass may write /etc/hosts anew at
# boot; and by the installer.
set -euo pipefail
domain=$(. /etc/garrison/install.env && echo "$DOMAIN")
[ -n "$domain" ] || exit 0
line="127.0.0.1 git.$domain # garrison registry"
grep -qxF "$line" /etc/hosts && exit 0
sed -i '/# garrison registry$/d' /etc/hosts
echo "$line" >> /etc/hosts
echo "put back the hosts entry of git.$domain"
