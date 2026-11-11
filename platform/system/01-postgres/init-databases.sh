#!/bin/sh
# Runs once, when Postgres initialises an empty data directory.
# Creates one role and database per system component. Passwords are hex.
set -eu
for app in keycloak gitea grafana; do
  password=$(cat "/run/secrets/$app-db-password")
  psql -v ON_ERROR_STOP=1 -U postgres <<SQL
CREATE ROLE $app LOGIN PASSWORD '$password';
CREATE DATABASE $app OWNER $app;
SQL
done
