#!/usr/bin/env bash
# Creates the local admin account, adds Keycloak as the login source and puts
# the platform repo into Gitea.
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

# Keycloak login. Members of the Keycloak group root-admins are Gitea admins;
# Gitea re-reads the groups at every login, so leaving the group removes the right.
# A running Gitea keeps using the old settings after an update, so a changed
# login source is followed by a restart, at the end of this script. A
# fingerprint of the settings, kept in Gitea's data, tells whether they changed.
auth=$(docker exec -u git "$(container gitea_gitea)" sh -c '
  set -- --name keycloak --provider openidConnect --key gitea \
    --secret "$(cat /run/secrets/gitea-oidc-secret)" --scopes "openid email profile" \
    --auto-discover-url "https://keycloak.$0/realms/homecloud/.well-known/openid-configuration" \
    --group-claim-name groups --admin-group root-admins
  want=$(printf "%s\n" "$@" | sha256sum)
  mark=/data/gitea/homecloud-login-source
  id=$(gitea admin auth list | awk "NR > 1 && \$2 == \"keycloak\" { print \$1 }")
  if [ -z "$id" ]; then
    gitea admin auth add-oauth "$@" >/dev/null && echo added
  elif [ "$(cat $mark 2>/dev/null)" != "$want" ]; then
    gitea admin auth update-oauth --id "$id" "$@" >/dev/null && echo updated
  fi
  echo "$want" > $mark
' "$DOMAIN")

# ---------- platform repo ----------
# The system stack lives in the private repo system/platform. Only Gitea
# admins, i.e. root admins, can read or change it. It is pushed once; after
# that Gitea holds the truth and the installer leaves the repo alone.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Credentials go in a netrc file, read by both curl and git, never on a command line.
(umask 077; printf 'machine git.%s login %s password %s\n' "$DOMAIN" "$ADMIN_USER" "$(cat "$ETC/admin-password")" > "$work/.netrc")
resolve="git.$DOMAIN:443:127.0.0.1"
api() { # METHOD PATH [JSON]; prints the status code
  curl -sS -o /dev/null -w '%{http_code}' --netrc-file "$work/.netrc" --cacert "$ETC/tls.crt" --resolve "$resolve" \
    -X "$1" "https://git.$DOMAIN/api/v1$2" ${3:+-H 'Content-Type: application/json' -d "$3"}
}
wait_for 120 "the Gitea API" test "$(api GET /version)" = 200

if [ "$(api GET /orgs/system)" != 200 ]; then
  [ "$(api POST /orgs '{"username":"system","visibility":"private"}')" = 201 ] || { echo "could not create the system organisation" >&2; exit 1; }
fi
if [ "$(api GET /repos/system/platform)" != 200 ]; then
  [ "$(api POST /orgs/system/repos '{"name":"platform","private":true,"default_branch":"main"}')" = 201 ] || { echo "could not create the platform repo" >&2; exit 1; }
  cp -r "$here/../.." "$work/repo"
  (
    cd "$work/repo"
    export HOME=$work GIT_CONFIG_GLOBAL=/dev/null
    git init -q -b main
    git add -A
    git -c user.name="Home Cloud installer" -c user.email="$ADMIN_EMAIL" commit -q -m "Platform repo as installed"
    git -c http.sslCAInfo="$ETC/tls.crt" -c http.curloptResolve="$resolve" push -q "https://git.$DOMAIN/system/platform.git" main
  )
fi

if [ "$auth" = updated ]; then
  docker service update --quiet --detach --force gitea_gitea >/dev/null
fi
