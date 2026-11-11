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
