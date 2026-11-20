#!/usr/bin/env bash
# Adds the two test people, with the well-known password 12345678 (or
# TEST_PASSWORD), so they can also sign in by hand. Test installations only.
# Runs on the machine, as root, from the repo root. Safe to run again.
#
#   alice  root admin: may change the platform
#   bob    regular user: may look at everything, change nothing
#   carol  regular user: becomes an admin of the unit todo in the talk step
set -euo pipefail
password=${TEST_PASSWORD:-12345678}
USER_PASSWORD=$password garrison/keycloak/add-user.sh alice "alice@$(. /etc/garrison/install.env && echo "$DOMAIN")" --admin
USER_PASSWORD=$password garrison/keycloak/add-user.sh bob   "bob@$(. /etc/garrison/install.env && echo "$DOMAIN")"
USER_PASSWORD=$password garrison/keycloak/add-user.sh carol "carol@$(. /etc/garrison/install.env && echo "$DOMAIN")"
