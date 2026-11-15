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

# gitea_session DIR: writes DIR/.netrc for the admin account and defines
# gitea_api and gitea_git, which reach Gitea at its public name through Traefik
# on this machine. Credentials stay in the netrc file, off every command line.
gitea_session() {
  GITEA_WORK=$1
  (umask 077; printf 'machine git.%s login %s password %s\n' "$DOMAIN" "$ADMIN_USER" "$(cat "$ETC/admin-password")" > "$GITEA_WORK/.netrc")
}
# gitea_api METHOD PATH [JSON | @FILE]: prints the HTTP status; the body goes to $GITEA_WORK/body.
gitea_api() {
  curl -sS -o "$GITEA_WORK/body" -w '%{http_code}' --netrc-file "$GITEA_WORK/.netrc" --cacert "$ETC/tls.crt" \
    --resolve "git.$DOMAIN:443:127.0.0.1" -X "$1" "https://git.$DOMAIN/api/v1$2" \
    ${3:+-H 'Content-Type: application/json' --data-binary "$3"}
}
gitea_git() {
  HOME=$GITEA_WORK GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
    git -c http.sslCAInfo="$ETC/tls.crt" -c "http.curloptResolve=git.$DOMAIN:443:127.0.0.1" \
        -c user.name="Garrison installer" -c user.email="$ADMIN_EMAIL" "$@"
}
