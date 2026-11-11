#!/usr/bin/env bash
# Creates the local admin account and adds Keycloak as the login source.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../lib.sh"

gitea() { docker exec -u git "$(container gitea_gitea)" gitea "$@"; }
wait_for 300 "Gitea to start" gitea admin user list

# Local admin: used by the installer and, later, the controller. People sign in through Keycloak.
if ! gitea admin user list --admin | awk 'NR > 1 { print $2 }' | grep -qx "$ADMIN_USER"; then
  gitea admin user create --admin --must-change-password=false \
    --username "$ADMIN_USER" --email "$ADMIN_EMAIL" --password "$(cat "$ETC/admin-password")" >/dev/null
fi

if ! gitea admin auth list | awk 'NR > 1 { print $2 }' | grep -qx keycloak; then
  docker exec -u git "$(container gitea_gitea)" sh -c '
    gitea admin auth add-oauth --name keycloak --provider openidConnect --key gitea \
      --secret "$(cat /run/secrets/gitea-oidc-secret)" --scopes "openid email profile" \
      --auto-discover-url "https://auth.$0/realms/homecloud/.well-known/openid-configuration"' "$DOMAIN" >/dev/null
fi
