#!/usr/bin/env bash
# Creates the role and database of the dashboard's feed, homecloud, where
# they are missing: on a new installation, and on one installed before the
# feed (init-databases.sh runs only on an empty data directory). Changes
# nothing that exists.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
. "$here/../../installer/lib.sh"

ready() { docker exec "$(container postgres_postgres)" pg_isready -h 127.0.0.1 -U postgres; }
wait_for 300 "Postgres to answer" ready
docker exec -i "$(container postgres_postgres)" sh -s <<'SH'
set -eu
password=$(cat /run/secrets/homecloud-db-password)
q() { psql -v ON_ERROR_STOP=1 -U postgres -tAc "$1"; }
[ "$(q "SELECT 1 FROM pg_roles WHERE rolname = 'homecloud'")" = 1 ] || q "CREATE ROLE homecloud LOGIN PASSWORD '$password'"
[ "$(q "SELECT 1 FROM pg_database WHERE datname = 'homecloud'")" = 1 ] || q "CREATE DATABASE homecloud OWNER homecloud"
SH
