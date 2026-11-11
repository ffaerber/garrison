# Helpers for the configure.sh steps the installer runs after deploying a stack.
# They get DOMAIN, ADMIN_USER, ADMIN_EMAIL and ETC from the installer.

# container SERVICE: the ID of the running container of a Swarm service.
container() {
  docker ps --quiet --filter "label=com.docker.swarm.service.name=$1" --filter status=running | head -n1
}

# wait_for SECONDS DESCRIPTION COMMAND...: retry a command until it succeeds.
wait_for() {
  local deadline=$((SECONDS + $1)) what=$2
  shift 2
  until "$@" >/dev/null 2>&1; do
    if [ $SECONDS -ge $deadline ]; then
      echo "timed out waiting for $what" >&2
      return 1
    fi
    sleep 3
  done
}

# keycloak_login: log the admin CLI inside the Keycloak container in.
# Fails while Keycloak is still starting, so it doubles as a readiness check.
keycloak_login() {
  local id
  id=$(container keycloak_keycloak) && [ -n "$id" ] && docker exec "$id" bash -c '
    KC_CLI_PASSWORD=$(</run/secrets/admin-password) /opt/keycloak/bin/kcadm.sh config credentials \
      --config /tmp/kcadm.config --server http://localhost:8080 --realm master --user "$0"' "$ADMIN_USER"
}
