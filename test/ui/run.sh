#!/usr/bin/env bash
# test/ui/run.sh [NAME]: a browser scenario, test/ui/NAME.py (ui by default), in Playwright's Chromium,
# in a container on this machine, which reaches every name of the
# installation at 127.0.0.1. Runs on the machine, as root. Prints ok/FAIL
# lines; screenshots of each step, and of every failure, go to $OUT
# (/var/tmp/garrison-ui).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
# shellcheck disable=SC1091
. /etc/garrison/install.env
out=${OUT:-/var/tmp/garrison-ui}
rm -rf "$out" && mkdir -p "$out"
# Pulling Playwright's image of over a gigabyte can drop halfway ("failed to
# send write: EOF"); a clone has none yet, so it is tried again.
for try in 1 2 3; do
  docker build -q -t garrison-ui . >/dev/null && break
  [ "$try" = 3 ] && exit 1
  echo "building the browser's image failed; trying again" >&2
  sleep 10
done
docker run --rm --network host -v "$PWD:/ui:ro" -v "$out:/out" -w /ui \
  -e OUT=/out -e DOMAIN="$DOMAIN" -e TIMEZONE="${TIMEZONE:-UTC}" -e ADMIN_USER="$ADMIN_USER" \
  -e ADMIN_PASSWORD="$(cat /etc/garrison/admin-password)" -e PASSWORD="${TEST_PASSWORD:-12345678}" \
  garrison-ui python3 -u "${1:-ui}.py"
