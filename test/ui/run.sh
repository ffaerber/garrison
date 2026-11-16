#!/usr/bin/env bash
# test/ui/run.sh: the UI scenarios (scenarios.py), in Playwright's Chromium,
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
docker build -q -t garrison-ui . >/dev/null
docker run --rm --network host -v "$PWD:/ui:ro" -v "$out:/out" -w /ui \
  -e OUT=/out -e DOMAIN="$DOMAIN" -e TIMEZONE="${TIMEZONE:-UTC}" -e ADMIN_USER="$ADMIN_USER" \
  -e ADMIN_PASSWORD="$(cat /etc/garrison/admin-password)" -e PASSWORD="${TEST_PASSWORD:-12345678}" \
  garrison-ui python3 -u scenarios.py
