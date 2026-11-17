#!/usr/bin/env bash
# Creates the realm people sign in to, the clients of the tools that use it,
# the root-admins group and the first admin account. Changes nothing that
# already exists, except client settings, which are brought back in line.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../../installer/lib.sh"

wait_for 600 "Keycloak to accept the admin login" keycloak_login
docker exec -i -e DOMAIN -e ADMIN_USER -e ADMIN_EMAIL "$(container keycloak_keycloak)" bash -s < "$here/realm.sh"
