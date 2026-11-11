#!/usr/bin/env bash
# Adds the two test people, with the well-known password 12345678 (or
# TEST_PASSWORD), so they can also sign in by hand. Test installations only.
# Runs on the machine, as root, from the repo root. Safe to run again.
#
#   alice  root admin: may change the platform
#   bob    regular user: may look at everything, change nothing
set -euo pipefail
password=${TEST_PASSWORD:-12345678}
USER_PASSWORD=$password system/keycloak/add-user.sh alice "alice@$(. /etc/homecloud/install.env && echo "$DOMAIN")" --admin
USER_PASSWORD=$password system/keycloak/add-user.sh bob   "bob@$(. /etc/homecloud/install.env && echo "$DOMAIN")"
