#!/usr/bin/env bash
# Runs on the test machine, as root, from the repo root. Makes every repo of
# the `garrison` organisation in Gitea match garrison/ in this checkout, as an
# admin would by pushing, and waits until the deployer has deployed it all.
# Does nothing before the first install.
#
# This is how a test run gets changes onto an installed machine: through Git
# and the deployer, like every other change.
set -euo pipefail

ETC=/etc/garrison
[ -s $ETC/install.env ] || exit 0
. $ETC/install.env
export DOMAIN ADMIN_USER ADMIN_EMAIL ETC
. installer/lib.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gitea_session "$work"
[ "$(gitea_api GET /orgs/garrison)" = 200 ] || { echo "no garrison organisation yet; the installer creates it"; exit 0; }

plane_commit=
for dir in garrison/*/; do
  name=$(basename "$dir")
  src=$dir
  if [ "$name" = control-plane ]; then
    src=$work/control-plane-tree
    installer/control-plane-tree.sh "$src"
  fi
  result=$(installer/push-repo.sh --update "garrison/$name" "$src")
  echo "  garrison/$name: $result"
  [ "$name" = control-plane ] && [ "${result%% *}" = updated ] && plane_commit=${result#updated }
done

# The deployer only follows Gitea while it runs in Swarm.
docker service inspect control-plane_deployer >/dev/null 2>&1 || exit 0

# A changed control plane is built first; the build sets the new tag.
if [ -n "$plane_commit" ]; then
  echo "  waiting for the control plane build of $plane_commit"
  deadline=$((SECONDS + 1200))
  until [ "$(gitea_api GET /repos/garrison/control-plane/raw/compose.yaml)" = 200 ] && grep -q "garrison/control-plane:$plane_commit" "$work/body"; do
    [ $SECONDS -ge $deadline ] && { echo "the control plane was not built" >&2; exit 1; }
    sleep 10
  done
fi

# Wait until the deployer has deployed the tip of every stack's repo.
echo "  waiting for the deployer to deploy"
deadline=$((SECONDS + 600))
while :; do
  behind=
  for dir in garrison/*/; do
    name=$(basename "$dir")
    [ -f "$dir/compose.yaml" ] || continue
    [ "$(gitea_api GET "/repos/garrison/$name/branches/main")" = 200 ] || { behind+=" $name"; continue; }
    head=$(grep -o '"id":"[0-9a-f]*"' "$work/body" | head -n1 | cut -d'"' -f4)
    garrison applied 2>/dev/null | grep -qx "$name $head" || behind+=" $name"
  done
  [ -z "$behind" ] && break
  if [ $SECONDS -ge $deadline ]; then
    echo "the deployer has not deployed:$behind" >&2
    docker service logs --tail 20 control-plane_deployer >&2 || true
    exit 1
  fi
  sleep 5
done
echo "  deployed"
