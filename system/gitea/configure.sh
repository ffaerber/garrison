#!/usr/bin/env bash
# Creates the local admin account, adds Keycloak as the login source, creates
# the `system` organisation with one repo per system stack, and prepares Gitea
# Actions: the runner's registration and the credentials the controller build
# deploys with.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../../installer/lib.sh"

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

# ---------- the system organisation ----------
# A namespace is a Gitea organisation and each of its stacks a repo. The
# system stack is the namespace `system`. Everyone signed in can read it; only
# Gitea admins, i.e. root admins, can change it.
ROOT=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gitea_session "$work"
# A function, so the check runs again on every try.
gitea_up() { [ "$(gitea_api GET /version)" = 200 ]; }
wait_for 120 "the Gitea API" gitea_up

if [ "$(gitea_api GET /orgs/system)" != 200 ]; then
  [ "$(gitea_api POST /orgs '{"username":"system","visibility":"limited"}')" = 201 ] || { echo "could not create the system organisation: $(cat "$work/body")" >&2; exit 1; }
fi
# Brought back in line on every run: signed-in people only.
[ "$(gitea_api PATCH /orgs/system '{"visibility":"limited"}')" = 200 ] || { echo "could not set the system organisation's visibility" >&2; exit 1; }

# ---------- Actions: runner and build credentials ----------
# The runner registers for this organisation only (system/runner explains why).
if ! docker secret inspect gitea-runner-token >/dev/null 2>&1; then
  gitea actions generate-runner-token --scope system | tr -d '\r\n' | docker secret create gitea-runner-token - >/dev/null
fi
# The user runner registers for the whole instance; jobs pick it with
# runs-on: homecloud-user. Only the system runner can reach Docker directly.
if ! docker secret inspect gitea-user-runner-token >/dev/null 2>&1; then
  gitea actions generate-runner-token | tr -d '\r\n' | docker secret create gitea-user-runner-token - >/dev/null
fi
# The controller build pushes its image and commits its tag as the admin,
# with a token limited to repositories and packages. Kept root-only in $ETC
# and as organisation secrets, which only root admins can read or change.
if [ ! -s "$ETC/ci-token" ]; then
  gitea_api DELETE "/users/$ADMIN_USER/tokens/controller-ci" >/dev/null
  token=$(gitea admin user generate-access-token --username "$ADMIN_USER" --token-name controller-ci \
    --scopes write:repository,write:package --raw | tr -d '\r\n')
  [ -n "$token" ] || { echo "could not create the build token" >&2; exit 1; }
  (umask 077; printf '%s' "$token" > "$ETC/ci-token")
fi
(umask 077
 printf '{"data":"%s"}' "$(cat "$ETC/ci-token")" > "$work/token.json"
 printf '{"data":"%s"}' "$ADMIN_USER" > "$work/user.json")
for s in PLATFORM_TOKEN:token PLATFORM_USER:user; do
  status=$(gitea_api PUT "/orgs/system/actions/secrets/${s%%:*}" "@$work/${s#*:}.json")
  case $status in 201|204) ;; *) echo "could not set the organisation secret ${s%%:*}: $status" >&2; exit 1 ;; esac
done
rm -f "$work/token.json" "$work/user.json"
# Not secret: the registry's public name, which builds tag and push images to.
printf '{"value":"git.%s"}' "$DOMAIN" > "$work/registry.json"
case $(gitea_api PUT /orgs/system/actions/variables/REGISTRY "@$work/registry.json") in
  204|200|201) ;;
  *) [ "$(gitea_api POST /orgs/system/actions/variables/REGISTRY "@$work/registry.json")" = 201 ] ||
       { echo "could not set the organisation variable REGISTRY: $(cat "$work/body")" >&2; exit 1; } ;;
esac

# ---------- one repo per system stack ----------
# Pushed once; after that Gitea holds the truth and the installer leaves the
# contents alone.
for dir in "$ROOT"/system/*/; do
  name=$(basename "$dir")
  src=$dir
  if [ "$name" = controller ]; then
    src=$work/controller-tree
    "$ROOT/installer/controller-tree.sh" "$src"
  fi
  printf '  system/%s: %s\n' "$name" "$("$ROOT/installer/push-repo.sh" "system/$name" "$src")"
done
[ "$(gitea_api PATCH /repos/system/controller '{"has_actions":true}')" = 200 ] || { echo "could not enable Actions on system/controller" >&2; exit 1; }

if [ "$auth" = updated ]; then
  docker service update --quiet --detach --force gitea_gitea >/dev/null
fi
