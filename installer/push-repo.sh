#!/usr/bin/env bash
# push-repo.sh [--update] ORG/NAME SOURCE_DIR
#
# Creates the repo in Gitea if it is missing and fills its main branch with
# SOURCE_DIR. An existing repo is left alone unless --update is given; then
# main is made to match SOURCE_DIR in a new commit. Image tags that the repo's
# own build has set (ORG/NAME:<tag> in kubernetes.yaml) are kept.
#
# Runs as root on the machine. Prints created, updated <commit> or unchanged.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
. "$ROOT/installer/lib.sh"
ETC=${ETC:-/etc/garrison}
[ -n "${DOMAIN:-}" ] || . "$ETC/install.env"

update=
[ "${1:-}" = --update ] && { update=1; shift; }
[ $# = 2 ] || { echo "usage: push-repo.sh [--update] ORG/NAME SOURCE_DIR" >&2; exit 2; }
full=$1 src=$2 org=${1%/*} name=${1#*/}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gitea_session "$work"
remote=https://git.$DOMAIN/$full.git

created=
case $(gitea_api GET "/repos/$full") in
  200) [ -n "$update" ] || { echo unchanged; exit 0; } ;;
  404)
    status=$(gitea_api POST "/orgs/$org/repos" '{"name":"'"$name"'","private":false,"default_branch":"main"}')
    [ "$status" = 201 ] || { echo "could not create $full: $status $(cat "$work/body")" >&2; exit 1; }
    created=1 ;;
  *) echo "could not look up $full: $(cat "$work/body")" >&2; exit 1 ;;
esac

if [ -n "$created" ]; then
  mkdir "$work/repo"
  gitea_git -C "$work/repo" init -q -b main
else
  gitea_git clone -q --branch main "$remote" "$work/repo" 2>/dev/null || {
    mkdir -p "$work/repo"; gitea_git -C "$work/repo" init -q -b main; }
fi

# Remember tags this repo's build set, replace the contents, put the tags back.
tag=
# Most repos have no image of their own: grep then finds nothing, which is no error.
if [ -f "$work/repo/kubernetes.yaml" ]; then
  tag=$(grep -oE "/$full:[A-Za-z0-9._-]+" "$work/repo/kubernetes.yaml" | head -n1 | sed 's/.*://' || true)
fi
find "$work/repo" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
cp -r "$src"/. "$work/repo/"
if [ -n "$tag" ] && [ "$tag" != bootstrap ]; then
  sed -i -E "s#(/$full:)[A-Za-z0-9._-]+#\1$tag#g" "$work/repo/kubernetes.yaml"
fi

gitea_git -C "$work/repo" add -A
if gitea_git -C "$work/repo" rev-parse -q --verify HEAD >/dev/null && gitea_git -C "$work/repo" diff --cached --quiet; then
  echo unchanged
  exit 0
fi
gitea_git -C "$work/repo" commit -q -m "$([ -n "$created" ] && echo "Initial contents from the installer" || echo "Update from $(hostname)")"
gitea_git -C "$work/repo" push -q "$remote" main
[ -n "$created" ] && echo created || echo "updated $(gitea_git -C "$work/repo" rev-parse --short=12 HEAD)"
