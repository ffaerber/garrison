#!/usr/bin/env bash
# Creates the local admin account, adds Keycloak as the login source, creates
# the `garrison` organisation with one repo per system stack, and prepares Gitea
# Actions: the runner's registration and the credentials the control plane build
# deploys with.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../../installer/lib.sh"

# As the user git, as Gitea refuses to run as root.
gitea() { in_pod deployment/gitea su-exec git gitea "$@"; }
wait_for 300 "Gitea to start" gitea admin user list

# Local admin: used by the installer and, later, the deployer. People sign in through Keycloak.
if ! gitea admin user list --admin | awk 'NR > 1 { print $2 }' | grep -qx "$ADMIN_USER"; then
  gitea admin user create --admin --must-change-password=false \
    --username "$ADMIN_USER" --email "$ADMIN_EMAIL" --password "$(cat "$ETC/admin-password")" >/dev/null
fi

# Keycloak login. Members of the Keycloak group root-admins are Gitea admins;
# Gitea re-reads the groups at every login, so leaving the group removes the right.
# A running Gitea keeps using the old settings after an update, so a changed
# login source is followed by a restart, at the end of this script. A
# fingerprint of the settings, kept in Gitea's data, tells whether they changed.
auth=$(in_pod deployment/gitea su-exec git sh -c '
  set -- --name keycloak --provider openidConnect --key gitea \
    --secret "$(cat /run/secrets/gitea-oidc-secret)" --scopes "openid email profile" \
    --auto-discover-url "https://keycloak.$0/realms/garrison/.well-known/openid-configuration" \
    --group-claim-name groups --admin-group root-admins
  want=$(printf "%s\n" "$@" | sha256sum)
  mark=/data/gitea/garrison-login-source
  id=$(gitea admin auth list | awk "NR > 1 && \$2 == \"keycloak\" { print \$1 }")
  if [ -z "$id" ]; then
    gitea admin auth add-oauth "$@" >/dev/null && echo added
  elif [ "$(cat $mark 2>/dev/null)" != "$want" ]; then
    gitea admin auth update-oauth --id "$id" "$@" >/dev/null && echo updated
  fi
  echo "$want" > $mark
' "$DOMAIN")

# ---------- the garrison organisation ----------
# A unit is a Gitea organisation and each of its stacks a repo. The
# system stack is the unit `system`. Everyone signed in can read it; only
# Gitea admins, i.e. root admins, can change it.
ROOT=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gitea_session "$work"
# A function, so the check runs again on every try.
gitea_up() { [ "$(gitea_api GET /version)" = 200 ]; }
wait_for 120 "the Gitea API" gitea_up

if [ "$(gitea_api GET /orgs/garrison)" != 200 ]; then
  [ "$(gitea_api POST /orgs '{"username":"garrison","visibility":"limited"}')" = 201 ] || { echo "could not create the garrison organisation: $(cat "$work/body")" >&2; exit 1; }
fi
# Brought back in line on every run: signed-in people only.
[ "$(gitea_api PATCH /orgs/garrison '{"visibility":"limited"}')" = 200 ] || { echo "could not set the garrison organisation's visibility" >&2; exit 1; }

# ---------- Actions: runner and build credentials ----------
# The runner registers for this organisation only (garrison/runner explains why).
if ! has_secret gitea-runner-token; then
  gitea actions generate-runner-token --scope garrison | tr -d '\r\n' | put_secret gitea-runner-token
fi
# The user runner registers for the whole instance; jobs pick it with
# runs-on: garrison-user. Only the system runner can reach Docker directly.
if ! has_secret gitea-user-runner-token; then
  gitea actions generate-runner-token | tr -d '\r\n' | put_secret gitea-user-runner-token
fi
# Kubernetes pulls the platform's own images, garrison/*, from the registry
# with a token that may only read packages, as the admin; garrison's
# packages are visible to signed-in accounts only.
if ! has_secret registry-pull; then
  gitea_api DELETE "/users/$ADMIN_USER/tokens/registry-pull" >/dev/null
  pull=$(gitea admin user generate-access-token --username "$ADMIN_USER" --token-name registry-pull \
    --scopes read:package --raw | tr -d '\r\n')
  [ -n "$pull" ] || { echo "could not create the registry's pull token" >&2; exit 1; }
  # Through a file, so the token is on no command line.
  (umask 077; printf '{"auths":{"git.%s":{"auth":"%s"}}}' "$DOMAIN" "$(printf '%s:%s' "$ADMIN_USER" "$pull" | base64 -w0)" > "$work/pull.json")
  kubectl --namespace garrison create secret generic registry-pull --type kubernetes.io/dockerconfigjson \
    --from-file ".dockerconfigjson=$work/pull.json" >/dev/null
  rm -f "$work/pull.json"
fi
# The control plane build pushes its image and commits its tag as the admin,
# with a token limited to repositories and packages. Kept root-only in $ETC
# and as organisation secrets, which only root admins can read or change.
if [ ! -s "$ETC/ci-token" ]; then
  gitea_api DELETE "/users/$ADMIN_USER/tokens/control-plane-ci" >/dev/null
  token=$(gitea admin user generate-access-token --username "$ADMIN_USER" --token-name control-plane-ci \
    --scopes write:repository,write:package --raw | tr -d '\r\n')
  [ -n "$token" ] || { echo "could not create the build token" >&2; exit 1; }
  (umask 077; printf '%s' "$token" > "$ETC/ci-token")
fi
(umask 077
 printf '{"data":"%s"}' "$(cat "$ETC/ci-token")" > "$work/token.json"
 printf '{"data":"%s"}' "$ADMIN_USER" > "$work/user.json")
for s in PLATFORM_TOKEN:token PLATFORM_USER:user; do
  status=$(gitea_api PUT "/orgs/garrison/actions/secrets/${s%%:*}" "@$work/${s#*:}.json")
  case $status in 201|204) ;; *) echo "could not set the organisation secret ${s%%:*}: $status" >&2; exit 1 ;; esac
done
rm -f "$work/token.json" "$work/user.json"
# Not secret: the registry's public name, which builds tag and push images to.
printf '{"value":"git.%s"}' "$DOMAIN" > "$work/registry.json"
case $(gitea_api PUT /orgs/garrison/actions/variables/REGISTRY "@$work/registry.json") in
  204|200|201) ;;
  *) [ "$(gitea_api POST /orgs/garrison/actions/variables/REGISTRY "@$work/registry.json")" = 201 ] ||
       { echo "could not set the organisation variable REGISTRY: $(cat "$work/body")" >&2; exit 1; } ;;
esac

# ---------- one repo per system stack ----------
# Pushed once; after that Gitea holds the truth and the installer leaves the
# contents alone.
for dir in "$ROOT"/garrison/*/; do
  name=$(basename "$dir")
  src=$dir
  if [ "$name" = control-plane ]; then
    src=$work/control-plane-tree
    "$ROOT/installer/control-plane-tree.sh" "$src"
  fi
  printf '  garrison/%s: %s\n' "$name" "$("$ROOT/installer/push-repo.sh" "garrison/$name" "$src")"
done
[ "$(gitea_api PATCH /repos/garrison/control-plane '{"has_actions":true}')" = 200 ] || { echo "could not enable Actions on garrison/control-plane" >&2; exit 1; }

if [ "$auth" = updated ]; then
  kubectl --namespace garrison rollout restart deployment/gitea >/dev/null
fi
