#!/usr/bin/env bash
# Creates the realm people sign in to, the clients of the tools that use it,
# and the first admin account. Changes nothing that already exists, except
# client settings, which are brought back in line.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../lib.sh"

# Log the admin CLI in; this also waits until Keycloak has started.
login() {
  local id
  id=$(container keycloak_keycloak) && [ -n "$id" ] && docker exec "$id" bash -c '
    KC_CLI_PASSWORD=$(</run/secrets/admin-password) /opt/keycloak/bin/kcadm.sh config credentials \
      --config /tmp/kcadm.config --server http://localhost:8080 --realm master --user "$0"' "$ADMIN_USER"
}
wait_for 600 "Keycloak to accept the admin login" login

docker exec -i -e DOMAIN -e ADMIN_USER -e ADMIN_EMAIL "$(container keycloak_keycloak)" bash -s < "$here/realm.sh"
