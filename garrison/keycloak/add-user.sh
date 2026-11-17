#!/usr/bin/env bash
# Adds a person to the installation, or updates one. Run as root on the machine.
#
#   add-user.sh NAME EMAIL            a regular user
#   add-user.sh NAME EMAIL --admin    a root admin, who may change the platform
#
# The password comes from USER_PASSWORD, or is generated and printed. Running
# it again sets the password again and adds or removes the admin role.
# Interim: people will be managed through the dashboard and config repos.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../../installer/lib.sh"
. /etc/garrison/install.env

[ $# -ge 2 ] || { echo "usage: add-user.sh NAME EMAIL [--admin]" >&2; exit 2; }
export NAME=$1 EMAIL=$2 ADMIN=0
[ "${3:-}" = --admin ] && ADMIN=1
generated=
if [ -z "${USER_PASSWORD:-}" ]; then
  USER_PASSWORD=$(openssl rand -hex 12)
  generated=1
fi
export USER_PASSWORD

keycloak_login >/dev/null
# `-e VAR` without a value hands the variable over without putting it on a command line.
docker exec -i -e NAME -e EMAIL -e ADMIN -e USER_PASSWORD "$(container keycloak_keycloak)" bash -s <<'IN_CONTAINER'
set -euo pipefail
realm=garrison
kc() { /opt/keycloak/bin/kcadm.sh "$@" --config /tmp/kcadm.config; }
id_of() { kc get "$1" -r "$realm" "${@:2}" --fields id --format csv --noquotes; }

user=$(id_of users -q "username=$NAME" -q exact=true)
if [ -z "$user" ]; then
  kc create users -r "$realm" -s "username=$NAME" -s "email=$EMAIL" -s emailVerified=true \
    -s enabled=true -s "firstName=$NAME" -s lastName=User >/dev/null
  user=$(id_of users -q "username=$NAME" -q exact=true)
fi
kc set-password -r "$realm" --username "$NAME" --new-password "$USER_PASSWORD"

group=$(id_of groups -q search=root-admins -q exact=true)
if [ "$ADMIN" = 1 ]; then
  kc update "users/$user/groups/$group" -r "$realm" -s "realm=$realm" -s "userId=$user" -s "groupId=$group" -n
else
  kc delete "users/$user/groups/$group" -r "$realm" 2>/dev/null || true
fi
IN_CONTAINER

role=user; [ "$ADMIN" = 1 ] && role="root admin"
echo "$NAME is a $role"
[ -z "$generated" ] || echo "password: $USER_PASSWORD"
