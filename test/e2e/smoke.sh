#!/usr/bin/env bash
# Smoke test for an installed machine. Run on the machine, as root, from the repo root.
set -uo pipefail

. /etc/homecloud/install.env
ETC=/etc/homecloud
STACKS=test/e2e/stacks
CTL="homecloud"
failed=0

ok()   { printf 'ok    %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n' "$1"; [ $# -gt 1 ] && printf '      %s\n' "$2"; failed=$((failed + 1)); }
check() {
  local name=$1 t0=$SECONDS; shift
  if out=$("$@" 2>&1); then ok "$name"; else bad "$name" "$(printf '%s' "$out" | tail -n 3)"; fi
  [ $((SECONDS - t0)) -lt 10 ] || printf 'slow  %4ds  %s\n' $((SECONDS - t0)) "$name"
}

# curl a hostname of the installation, verifying the installed certificate.
req() { local host=$1; shift; curl -sS --max-time 10 --cacert "$ETC/tls.crt" --resolve "$host.$DOMAIN:443:127.0.0.1" "$@"; }

# Poll until a URL returns the wanted status code.
status_is() {
  local want=$1 host=$2 path=$3 got= deadline=$((SECONDS + ${4:-180}))
  while [ $SECONDS -lt $deadline ]; do
    got=$(req "$host" -o /dev/null -w '%{http_code}' "https://$host.$DOMAIN$path" 2>/dev/null)
    [ "$got" = "$want" ] && return 0
    sleep 3
  done
  echo "https://$host.$DOMAIN$path returned $got, want $want"; return 1
}

# curl with every hostname of the installation pointed at this machine.
web() {
  local args=() h
  for h in keycloak sso git grafana prometheus loki chat gateway agents hello dev-hello todo dev-todo; do args+=(--resolve "$h.$DOMAIN:443:127.0.0.1"); done
  curl -sS --max-time 20 --cacert "$ETC/tls.crt" "${args[@]}" "$@"
}

# sso_login JAR URL PASSWORD OUT [USER]: open URL, fill in the Keycloak login form
# it leads to, follow the redirects back. Prints the final status; body goes to OUT.
sso_login() {
  local jar=$1 url=$2 password=$3 out=$4 user=${5:-$ADMIN_USER} form action
  form=$(web -L -c "$jar" -b "$jar" "$url") || return 1
  action=$(grep -o 'action="[^"]*"' <<<"$form" | head -n1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
  [ -n "$action" ] || { echo "no login form behind $url"; return 1; }
  web -L -c "$jar" -b "$jar" -o "$out" -w '%{http_code}' \
    --data-urlencode "username=$user" --data-urlencode "password=$password" "$action"
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
PASSWORD=$(cat $ETC/admin-password)

# After a reboot, give every service time to come back before checking.
if [ -n "${WAIT_READY:-}" ]; then
  deadline=$((SECONDS + WAIT_READY))
  while :; do
    pending=$(docker service ls --format '{{.Name}} {{.Replicas}}' | awk '{ split($2, r, "/"); if (r[1] != r[2]) print $1 }')
    [ -z "$pending" ] && break
    [ $SECONDS -ge $deadline ] && { echo "services not ready after reboot: $pending"; break; }
    sleep 5
  done
fi


# ---------- shared helpers ----------
TEST_PASSWORD=${TEST_PASSWORD:-12345678}   # alice and bob, see test/e2e/add-test-users.sh
# gitea_api USER_TO_ACT_AS ARGS...: call the Gitea API as the local admin, or,
# with a name, with exactly that person's rights (Gitea's Sudo header).
gitea_api() {
  local as=$1; shift
  web -u "$ADMIN_USER:$PASSWORD" ${as:+-H "Sudo: $as"} "$@"
}
api=https://git.$DOMAIN/api/v1
# Every system stack is a repo of the system organisation; traefik stands in for them.
repo=$api/repos/system/traefik
# until_true SECONDS COMMAND...: retry until the command succeeds.
until_true() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do [ $SECONDS -ge $deadline ] && return 1; sleep 3; done
}
smoke_running() { docker service ls --filter name=smoke_web --format '{{.Replicas}}' | grep -qx 1/1; }
smoke_gone()    { ! docker stack ls --format '{{.Name}}' | grep -qx smoke; }
deployer_image() { docker service inspect control-plane_deployer --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}'; }
json() { curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "git.$DOMAIN:443:127.0.0.1" -u "$ADMIN_USER:$PASSWORD" "$@"; }
# grafana_api USER ARGS...: call Grafana with a signed-in person's session, or,
# with "", as the local installer account.
grafana_api() {
  local as=$1; shift
  if [ -n "$as" ]; then web -b "$tmp/grafana-$as.jar" "$@"; else web -u "installer:$PASSWORD" "$@"; fi
}
# poll SECONDS PATTERN URL: until the installer's GET of URL matches PATTERN.
poll() {
  local deadline=$((SECONDS + $1)) out=
  while [ $SECONDS -lt $deadline ]; do
    out=$(grafana_api "" "$3" 2>&1)
    grep -Eq "$2" <<<"$out" && return 0
    sleep 5
  done
  echo "$out" | cut -c1-300; return 1
}
g=https://grafana.$DOMAIN

chat=https://chat.$DOMAIN
# chat_api PERSON ARGS...: Open WebUI's API with that person's session, signed
# in through Keycloak (see section login).
chat_api() {
  local as=$1; shift
  web -b "$tmp/chat-$as.jar" -H "Authorization: Bearer $(chat_token "$as")" "$@"
}
chat_token() { awk '$6 == "token" { print $7 }' "$tmp/chat-$1.jar" | tail -n1; }
# chat_login PERSON: signs in to Open WebUI through Keycloak. Tried again for
# up to two minutes: right after a restore, Open WebUI or Keycloak may still be starting.
chat_login() {
  local i
  for i in $(seq 24); do
    rm -f "$tmp/chat-$1.jar"
    sso_login "$tmp/chat-$1.jar" "$chat/oauth/oidc/login" "$TEST_PASSWORD" "$tmp/chat-$1.html" "$1" >/dev/null 2>&1
    chat_api "$1" "$chat/api/v1/auths/" 2>/dev/null | grep -q "\"name\":\"$1\"" && return 0
    sleep 5
  done
  return 1
}
# system_channel [PERSON]: the id of the channel of the namespace system.
system_channel() { chat_api "${1:-bob}" "$chat/api/v1/channels/" | python3 -c 'import json,sys; print(next(c["id"] for c in json.load(sys.stdin) if c["name"] == "system"))' 2>/dev/null; }
# mcp_calls SINCE PERSON: the tools the gateway ran for PERSON, as "mcp: PERSON: TOOL".
mcp_calls() { docker service logs --since "$1" control-plane_gateway 2>&1 | grep -oE "mcp: $2: [a-z0-9._-]+/[a-z0-9-]+: [a-z_]+" | sed -E 's#: [^ ]+/[^ ]+: #: #' | sort -u; }
# mcp ARGS...: the gateway's MCP endpoint, which only Open WebUI reaches.
mcp() {
  docker exec "$(docker ps -q --filter label=com.docker.swarm.service.name=openwebui_openwebui | head -n1)" \
    curl -sS --max-time 20 "$@" http://control-plane_gateway:8080/mcp
}

# agents_api ARGS... PATH: the agent service, which only the system network reaches.
agents_api() {
  local path=${!#}
  docker exec "$(docker ps -q --filter label=com.docker.swarm.service.name=openwebui_openwebui | head -n1)" \
    curl -sS --max-time 20 "${@:1:$#-1}" "http://control-plane_agents:8070$path"
}

# ---------- sections ----------
# system: the stacks the installer deploys are up and safe.
section_system() {
echo "# system stack"
check "ZFS pool is online"                 test "$(zpool list -H -o health homecloud)" = ONLINE
check "Keycloak serves OIDC discovery over valid TLS" status_is 200 keycloak /realms/master/.well-known/openid-configuration 300
check "Keycloak issuer uses the public hostname" \
  bash -c "$(declare -f req); ETC=$ETC DOMAIN=$DOMAIN; req keycloak https://keycloak.$DOMAIN/realms/master/.well-known/openid-configuration | grep -q '\"issuer\":\"https://keycloak.$DOMAIN/realms/master\"'"
check "Gitea is healthy over valid TLS"    status_is 200 git /api/healthz
login=$(req git -u "$ADMIN_USER:$(cat $ETC/admin-password)" "https://git.$DOMAIN/api/v1/user" 2>&1)
check "Gitea admin can log in and is admin" grep -q '"is_admin":true' <<<"$login"
check "Gitea rejects a wrong password"     test "$(req git -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:wrong" "https://git.$DOMAIN/api/v1/user")" = 401
redirect=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "git.$DOMAIN:80:127.0.0.1" "http://git.$DOMAIN/")
check "HTTP redirects to HTTPS"            grep -q "^30[18] https://git.$DOMAIN/" <<<"$redirect"
check "Postgres is not published on the host" bash -c '! (ss -ltn | grep -q ":5432 ")'

}

# login: people sign in through Keycloak and get exactly their rights.
section_login() {
echo "# signing in"
check "the homecloud realm serves OIDC discovery" status_is 200 keycloak /realms/homecloud/.well-known/openid-configuration
check "Gitea settings need a login"        bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 https://git.$DOMAIN/user/settings)\" != 200 ]"
sso_login "$tmp/git.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$PASSWORD" "$tmp/git.html" >/dev/null 2>&1
check "the admin signs in to Gitea through Keycloak" test "$(web -b "$tmp/git.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/user/settings")" = 200

echo "# roles in Gitea"

for u in alice bob; do
  sso_login "$tmp/$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/$u.html" $u >/dev/null 2>&1
  check "$u signs in to Gitea through Keycloak" test "$(web -b "$tmp/$u.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/user/settings")" = 200
done
check "alice is a Gitea admin"             grep -q '"is_admin":true'  <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/alice")"
check "bob is not a Gitea admin"           grep -q '"is_admin":false' <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/bob")"
check "every system stack is a repo of the system organisation" \
  bash -c 'for n in postgres traefik keycloak sso gitea runner control-plane grafana prometheus loki; do grep -q "\"full_name\":\"system/$n\"" <<<"$0" || { echo "missing system/$n"; exit 1; }; done' "$(gitea_api "" "$api/orgs/system/repos?limit=50")"
check "the traefik repo holds its stack"   test "$(gitea_api "" -o /dev/null -w '%{http_code}' $repo/contents/compose.yaml)" = 200
check "alice can administer the system repos"    grep -q '"permissions":{"admin":true' <<<"$(gitea_api alice $repo)"
check "alice can change a system repo" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"YWxpY2UK","message":"smoke test","new_branch":"smoke-'$RANDOM$RANDOM'"}' $repo/contents/smoke-test.txt)" = 201
check "bob can see the system repos, read-only"  grep -q '"permissions":{"admin":false,"push":false,"pull":true}' <<<"$(gitea_api bob $repo)"
check "bob can read the system stack"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' $repo/contents/compose.yaml)" = 200
check "bob can see it in the browser too"  test "$(web -b "$tmp/bob.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/system/traefik")" = 200
check "bob cannot change a system repo" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Ym9iCg==","message":"smoke test"}' $repo/contents/bob-was-here.txt)" = 403
check "bob cannot change it on a new branch either" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Ym9iCg==","message":"smoke test","new_branch":"bob"}' $repo/contents/bob-was-here.txt)" = 403
check "bob's change did not land"          test "$(gitea_api "" -o /dev/null -w '%{http_code}' $repo/contents/bob-was-here.txt)" = 404
check "bob cannot change the repo's settings" test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"description":"bob"}' $repo)" = 403
check "someone not signed in cannot read the system repos" bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 $repo)\" != 200 ]"
check "someone not signed in cannot browse it either" bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 https://git.$DOMAIN/system/traefik)\" != 200 ]"
check "bob cannot make himself admin"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"login_name":"bob","source_id":0,"admin":true}' "https://git.$DOMAIN/api/v1/admin/users/bob")" = 403
check "bob is still not a Gitea admin"     grep -q '"is_admin":false' <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/bob")"

echo "# Open WebUI"
check "Open WebUI is healthy over valid TLS" status_is 200 chat /health 300
check "it sends visitors to the Keycloak login" \
  grep -q "^https://keycloak.$DOMAIN/realms/homecloud/protocol/openid-connect/auth" <<<"$(web -o /dev/null -w '%{redirect_url}' "$chat/oauth/oidc/login")"
check "it shows nothing without signing in" test "$(web -o /dev/null -w '%{http_code}' "$chat/api/models")" = 401
for u in alice bob; do
  chat_login $u
  check "$u signs in to Open WebUI through Keycloak" grep -q "\"name\":\"$u\"\|\"email\":\"$u@" <<<"$(chat_api $u "$chat/api/v1/auths/")"
done
check "alice is shown as alice"            grep -q '"name":"alice"' <<<"$(chat_api alice "$chat/api/v1/auths/")"
check "alice is an Open WebUI admin"       grep -q '"role":"admin"' <<<"$(chat_api alice "$chat/api/v1/auths/")"
check "bob is a user"                      grep -q '"role":"user"'  <<<"$(chat_api bob "$chat/api/v1/auths/")"
has_agent() { chat_api bob "$chat/api/models" | grep -q "\"id\":\"$1\""; }
check "bob has the operator agent of the namespace system" until_true 180 has_agent system/operator
check "the agents come from the agent service" grep -q '"owned_by":"homecloud"' <<<"$(chat_api bob "$chat/api/models")"
check "Open WebUI has no LLM key" \
  test -z "$(docker exec "$(docker ps -q --filter label=com.docker.swarm.service.name=openwebui_openwebui | head -n1)" printenv OPENAI_API_KEY)"
check "the agent service wants a token" \
  test "$(agents_api -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"model":"system/operator","messages":[]}' /v1/chat/completions)" = 401
check "it refuses a made-up token" \
  test "$(agents_api -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln' -d '{"model":"system/operator","messages":[]}' /v1/chat/completions)" = 401
check "the agent service is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' "https://agents.$DOMAIN/v1/models")" = 404
check "system has its config repo with the operator" \
  test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/system/config/raw/agents/operator/agent.yaml")" = 200
check "bob sees the channel of the namespace system" until_true 120 system_channel
cid=$(system_channel)
check "bob cannot rename it" \
  test "$(chat_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bobs-channel"}' "$chat/api/v1/channels/$cid/update")" = 403
check "bob cannot delete it"               test "$(chat_api bob -o /dev/null -w '%{http_code}' -X DELETE "$chat/api/v1/channels/$cid/delete")" = 403
check "it is still there, unchanged"       test "$(system_channel)" = "$cid"
howto_pinned() { chat_api bob "$chat/api/v1/channels/$cid/messages/pinned" | grep -q 'How to ask the agent of this namespace'; }
check "it has the how-to pinned"           until_true 120 howto_pinned
check "the gateway's account says it is not the agent" \
  grep -q '"name":"Gateway (not the agent)"' <<<"$(chat_api bob "$chat/api/v1/channels/$cid/members?page=1")"
check "the gateway is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' "https://gateway.$DOMAIN/mcp")" = 404
check "its MCP endpoint wants a token"     test "$(mcp -o /dev/null -w '%{http_code}' -X POST -d '{}')" = 401
check "it refuses a made-up token"         test "$(mcp -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln' -d '{}')" = 401
check "it refuses identity headers"        test "$(mcp -o /dev/null -w '%{http_code}' -X POST -H 'X-OpenWebUI-User-Name: alice' -H 'X-Forwarded-User: alice' -d '{}')" = 401

echo "# roles in Grafana"
check "Grafana is healthy over valid TLS"  status_is 200 grafana /api/health 300
check "Grafana sends visitors to the Keycloak login" \
  grep -q "^https://keycloak.$DOMAIN/realms/homecloud/protocol/openid-connect/auth" <<<"$(web -o /dev/null -w '%{redirect_url}' $g/login/generic_oauth)"
check "Grafana shows nothing without signing in" test "$(web -o /dev/null -w '%{http_code}' $g/api/search)" = 401
for u in alice bob; do
  sso_login "$tmp/grafana-$u.jar" "$g/login/generic_oauth" "$TEST_PASSWORD" "$tmp/grafana-$u.html" $u >/dev/null 2>&1
  check "$u signs in to Grafana through Keycloak" grep -q "\"login\":\"$u\"" <<<"$(grafana_api $u $g/api/user)"
done
check "alice is a Grafana admin"           grep -q '"isGrafanaAdmin":true'  <<<"$(grafana_api alice $g/api/user)"
check "bob is not a Grafana admin"         grep -q '"isGrafanaAdmin":false' <<<"$(grafana_api bob $g/api/user)"
check "bob is a viewer"                    grep -q '"role":"Viewer"' <<<"$(grafana_api bob $g/api/user/orgs)"
check "bob can look at dashboards"         test "$(grafana_api bob -o /dev/null -w '%{http_code}' $g/api/search)" = 200
check "bob can query metrics" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"queries":[{"refId":"A","datasource":{"uid":"prometheus"},"expr":"up","instant":true}],"from":"now-5m","to":"now"}' $g/api/ds/query)" = 200
check "bob cannot create a dashboard" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"dashboard":{"title":"bob"},"overwrite":false}' $g/api/dashboards/db)" = 403
check "bob cannot add a data source" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bob","type":"prometheus","url":"http://example.invalid","access":"proxy"}' $g/api/datasources)" = 403
check "bob cannot make himself admin" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"role":"Admin"}' $g/api/org/users/2)" = 403
check "alice can create a dashboard" \
  test "$(grafana_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"dashboard":{"uid":"smoke","title":"Smoke test"},"overwrite":true}' $g/api/dashboards/db)" = 200

}

# platform: the deployer, monitoring data, app stacks and private environments.
section_platform() {
echo "# deployer"

check "the deployer runs in Swarm"       test "$(docker service ls --filter name=control-plane_deployer --format '{{.Replicas}}')" = 1/1
check "it runs an image built in Gitea"    bash -c "case '$(deployer_image)' in git.$DOMAIN/system/control-plane:bootstrap) exit 1 ;; git.$DOMAIN/system/control-plane:*) exit 0 ;; *) exit 1 ;; esac"
tag=$(deployer_image | sed 's/.*://')
check "the image is in Gitea's registry"   test "$(json -o /dev/null -w '%{http_code}' "$api/packages/system/container/control-plane/$tag")" = 200
check "its build succeeded in Gitea Actions" grep -q '"status":"success"' <<<"$(json "$api/repos/system/control-plane/actions/tasks?limit=1")"
check "the runner is up"                 test "$(docker service ls --filter name=runner_runner --format '{{.Replicas}}')" = 1/1
check "bob cannot see the build's secrets" test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/orgs/system/actions/secrets")" = 403
check "the deployer no longer runs on the host" bash -c '! systemctl is-active --quiet homecloud-controller'
check "the deployer deployed grafana, prometheus and loki" \
  bash -c 'a=$(homecloud applied | cut -d" " -f1); for n in grafana prometheus loki; do grep -qx $n <<<"$a" || exit 1; done'

docker stack rm smoke >/dev/null 2>&1 || true
gitea_api "" -o /dev/null -X DELETE "$api/repos/system/smoke" 2>/dev/null || true
compose=$(printf 'services:\n  web:\n    image: traefik/whoami:v1.11\n' | base64 -w0)
check "bob cannot add a repo to the system organisation" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bobs-stack"}' "$api/orgs/system/repos")" = 403
check "alice adds a repo to the system organisation" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"smoke","auto_init":true,"default_branch":"main"}' "$api/orgs/system/repos")" = 201
check "alice gives it a compose.yaml"      test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"message":"smoke test","branch":"main","content":"'$compose'"}' "$api/repos/system/smoke/contents/compose.yaml")" = 201
check "the deployer deploys it as the stack smoke" until_true 120 smoke_running
check "alice deletes the repo"             test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$api/repos/system/smoke")" = 204
check "the deployer takes the stack down" until_true 120 smoke_gone
check "the other system stacks are untouched" bash -c 'l=$(docker stack ls --format "{{.Name}}"); grep -qx grafana <<<"$l" && grep -qx gitea <<<"$l" && grep -qx control-plane <<<"$l"'

echo "# monitoring data"
check "Prometheus scrapes the node"        poll 180 '"job":"node"[^]]*\},"value":\[[0-9.]+,"1"\]' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=up"
check "Prometheus scrapes the containers"  poll 180 '"job":"cadvisor"[^]]*\},"value":\[[0-9.]+,"1"\]' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=up"
check "container metrics name their Swarm service" poll 180 'gitea_gitea' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=count%20by%20(container_label_com_docker_swarm_service_name)(container_memory_usage_bytes)"
check "Loki has the logs of the Gitea stack" poll 180 '"values":\[\[' "$g/api/datasources/proxy/uid/loki/loki/api/v1/query_range?query=%7Bstack%3D%22gitea%22%7D&limit=1"
check "Prometheus is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' https://prometheus.$DOMAIN/)" = 404
check "Loki is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' https://loki.$DOMAIN/)" = 404
check "Prometheus and Loki publish no port on the host" bash -c '! (ss -ltn | grep -Eq ":(9090|3100) ")'

echo "# dashboards from Git"
folder_count() { grafana_api "" "$g/api/search?type=dash-db&folderUIDs=ns-system&tag=git" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }
six_dashboards() { [ "$(folder_count)" -ge 6 ]; }
check "the system folder holds the dashboards from Git" until_true 180 six_dashboards
sso_login "$tmp/grafana-bob.jar" "$g/login/generic_oauth" "$TEST_PASSWORD" "$tmp/grafana-bob.html" bob >/dev/null 2>&1
check "bob can open the overview"          test "$(grafana_api bob -o /dev/null -w '%{http_code}' "$g/api/dashboards/uid/homecloud-overview")" = 200
check "bob cannot change it" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"dashboard":{"uid":"homecloud-overview","title":"bob"},"overwrite":true}' "$g/api/dashboards/db")" = 403
# Every query of every panel must run. Variables get example values.
queries_run() {
  python3 - "$@" <<'PY'
import json, glob, sys, subprocess, urllib.parse
g, password, user = sys.argv[1], sys.argv[2], sys.argv[3]
def get(path):
    out = subprocess.run(["bash", "-c", 'source /dev/stdin; web -u "$0:$1" "$2"', user, password, g + path],
                         input=sys.argv[4], capture_output=True, text=True).stdout
    try: return json.loads(out)
    except Exception: return {"status": "error", "error": out[:200]}
bad = 0
for f in sorted(glob.glob("system/grafana/dashboards/*.json")):
    d = json.load(open(f))
    for p in d["panels"]:
        for t in p.get("targets", []):
            expr = t.get("expr", "")
            for k, v in {"$stack": "grafana", "$service": ".*", "$agent": ".*", "$search": "", "$__auto": "1m", "$__interval": "1m"}.items():
                expr = expr.replace(k, v)
            if t["datasource"]["uid"] == "loki":
                r = get("/api/datasources/proxy/uid/loki/loki/api/v1/query_range?limit=1&" + urllib.parse.urlencode({"query": expr}))
            else:
                r = get("/api/datasources/proxy/uid/prometheus/api/v1/query?" + urllib.parse.urlencode({"query": expr}))
            if r.get("status") != "success":
                bad += 1
                print(f"{f.split('/')[-1]}: {p['title']}: {r.get('error', r)}")
sys.exit(bad > 0)
PY
}
check "every panel's query runs"           queries_run "$g" "$PASSWORD" installer "$(declare -f web); ETC=$ETC; DOMAIN=$DOMAIN"
prom_has() { grafana_api "" "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$1")" | grep -q '"value":\['; }
check "the overview shows the machine's CPU" until_true 120 prom_has '100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))'
check "the stack dashboard shows grafana's services" until_true 120 prom_has 'sum by (container_label_com_docker_swarm_service_name) (rate(container_cpu_usage_seconds_total{container_label_com_docker_stack_namespace="grafana"}[5m]))'
check "Prometheus scrapes Traefik"        until_true 180 prom_has 'up{job="traefik"} == 1'
check "the traffic dashboard has requests" until_true 120 prom_has 'sum(rate(traefik_entrypoint_requests_total[5m])) > 0'
check "Traefik's metrics are not reachable from outside" bash -c '! (ss -ltn | grep -q ":8082 ")'
# A dashboard committed to a repo appears; deleting the file removes it.
smoke_dash=$(printf '{"uid":"smoke-test","title":"Smoke test","panels":[]}' | base64 -w0)
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"message":"Add a dashboard","branch":"main","content":"'$smoke_dash'"}' "$api/repos/system/platform/contents/dashboards/smoke.json"
dash_there() { test "$(grafana_api "" -o /dev/null -w '%{http_code}' "$g/api/dashboards/uid/smoke-test")" = 200; }
dash_gone()  { ! dash_there; }
check "a dashboard committed by alice appears" until_true 120 dash_there
sha=$(gitea_api alice "$api/repos/system/platform/contents/dashboards/smoke.json" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)
gitea_api alice -o /dev/null -X DELETE -H 'Content-Type: application/json' -d '{"message":"Remove a dashboard","branch":"main","sha":"'$sha'"}' "$api/repos/system/platform/contents/dashboards/smoke.json"
check "deleting it in Git removes it"      until_true 120 dash_gone
# An edit made in Grafana is put back.
overview=$(grafana_api "" "$g/api/dashboards/uid/homecloud-overview" | python3 -c 'import json,sys; d=json.load(sys.stdin)["dashboard"]; d["title"]="Edited in Grafana"; print(json.dumps({"dashboard": d, "overwrite": True}))')
grafana_api "" -o /dev/null -X POST -H 'Content-Type: application/json' -d "$overview" "$g/api/dashboards/db"
title_back() { grafana_api "" "$g/api/dashboards/uid/homecloud-overview" | grep -q '"title":"Overview"'; }
check "an edit made in Grafana is put back" until_true 120 title_back

echo "# deploying a stack"
check "the CLI deploys hello (main)"      $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "hello is reachable at its hostname" status_is 200 hello /
check "hello got its dataset with a quota" test "$(zfs get -H -o value quota homecloud/ns/demo/stacks/hello/hello-data)" = 1G
check "hello's dataset is mounted in the container" \
  bash -c 'c=$(docker ps -q --filter label=com.docker.swarm.service.name=hello_web | head -n1); docker inspect "$c" --format "{{range .Mounts}}{{.Source}}:{{.Destination}} {{end}}" | grep -q "/homecloud/ns/demo/stacks/hello/hello-data:/data"'
check "hello got a generated secret"       docker secret inspect hello.hello-token
check "the CLI deploys hello (dev)"       $CTL apply -domain "$DOMAIN" -env dev -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "dev has its own dataset"            zfs list homecloud/ns/demo/stacks/dev-hello/hello-data
check "redeploying hello is accepted"      $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml

echo "# private environments"
check "a private environment answers visitors with a redirect" status_is 302 dev-hello /
target=$(web -o /dev/null -w '%{redirect_url}' "https://dev-hello.$DOMAIN/")
check "the redirect goes to the Keycloak login" grep -q "^https://keycloak.$DOMAIN/realms/homecloud/protocol/openid-connect/auth" <<<"$target"
check "a forged identity header does not get in" \
  test "$(web -o /dev/null -w '%{http_code}' -H "X-Auth-Request-Email: $ADMIN_EMAIL" "https://dev-hello.$DOMAIN/")" = 302
sso_login "$tmp/bad.jar" "https://dev-hello.$DOMAIN/" "wrong-password" "$tmp/bad.html" >/dev/null 2>&1
check "a wrong password does not reach the private environment" bash -c "! grep -q '^Hostname:' $tmp/bad.html"
code=$(sso_login "$tmp/sso.jar" "https://dev-hello.$DOMAIN/" "$PASSWORD" "$tmp/app.html" 2>&1)
check "the admin signs in and reaches the private environment" bash -c "[ '$code' = 200 ] && grep -q '^Hostname:' $tmp/app.html"
check "the app is told who signed in"      grep -qi "^X-Auth-Request-Email: $ADMIN_EMAIL" "$tmp/app.html"
check "the session works for later requests" test "$(web -b "$tmp/sso.jar" -o /dev/null -w '%{http_code}' "https://dev-hello.$DOMAIN/again")" = 200
check "a made-up bearer token does not get in" \
  bash -c '! grep -q "^Hostname:" <<<"$0"' "$(web -H 'Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln' "https://dev-hello.$DOMAIN/")"
check "the app never sees an Authorization header" \
  bash -c 'grep -q "^Hostname:" <<<"$0" && ! grep -qi "^Authorization:" <<<"$0"' "$(web -b "$tmp/sso.jar" -H 'Authorization: Basic Zm9vOmJhcg==' "https://dev-hello.$DOMAIN/")"
check "the public environment needs no login" status_is 200 hello / 30

echo "# hostname claims"
check "a stack claiming a held hostname is rejected"     bash -c "! $CTL apply -domain $DOMAIN -reserved $ETC/reserved-hostnames.yaml $STACKS/hello-clash.yaml"
check "a stack claiming a reserved hostname is rejected" bash -c "! $CTL apply -domain $DOMAIN -reserved $ETC/reserved-hostnames.yaml $STACKS/git-reserved.yaml"
check "rejected stacks were not deployed"  bash -c '! (docker stack ls --format "{{.Name}}" | grep -qx git)'
check "hello still answers after the rejected deploys"   status_is 200 hello / 30

}

# agent: the namespace agents answer in their channels in Open WebUI, when
# mentioned, and propose through the gateway's MCP tools, with the
# rights of the person who mentioned them. They talk to the real
# LLM, so answers vary; the checks look at what the agents did (tools called,
# changes in Gitea), not at their wording.
section_agent() {
echo "# the namespace agents"
if [ ! -s $ETC/llm-api-key ]; then
  echo "skip  no LLM key in $ETC/llm-api-key"
  return
fi
for u in alice bob; do check "$u signs in to Open WebUI" chat_login $u; done
# The gateway's own Open WebUI account; it may delete any message.
admin_token=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" \
  "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
cid=$(system_channel)
: > "$tmp/posted"   # ids of the test's messages; ask runs in a subshell
# ask PERSON QUESTION: posts in the channel of system, mentioning its agent, as
# the chat page does, and waits for the agent's answer in the message's thread.
# Prints the answer.
ask() {
  local who=$1 question=$2 mid i
  mid=$(chat_api "$who" -X POST -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"content": "<@M:system/operator|system/operator> " + sys.argv[1]}))' "$question")" \
    "$chat/api/v1/channels/$cid/messages/post" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || return 1
  echo "$mid" >> "$tmp/posted"
  for i in $(seq 60); do
    chat_api "$who" "$chat/api/v1/channels/$cid/messages/$mid/thread" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    meta = m.get("meta") or {}
    if meta.get("model_id") == "system/operator" and meta.get("done"):
        print(m.get("content") or ""); sys.exit(0)
sys.exit(1)' && return 0
    sleep 5
  done
  return 1
}
reply() { cat; }
# Leaves the channel as it was: deletes the test's questions and their threads.
cleanup_channel() {
  local mid
  for mid in $(cat "$tmp/posted"); do
    for id in $(chat_api bob "$chat/api/v1/channels/$cid/messages/$mid/thread" | python3 -c 'import json,sys; [print(m["id"]) for m in json.load(sys.stdin)]' 2>/dev/null) $mid; do
      web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$cid/messages/$id/delete"
    done
  done
}

# The model sometimes writes a tool call out as text instead of making it. A
# question whose answer shows no sign of the system is asked once more, and
# the retry is printed so it shows in the log.
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask bob "Which stacks run in the namespace system? Use your tools and list their names." | reply)
if ! grep -qi grafana <<<"$answer"; then
  printf 'retry the agent answered %q; asking again\n' "$(head -c 200 <<<"$answer")"
  answer=$(ask bob "Which stacks run in the namespace system? Call list_stacks and list their names." | reply)
fi
check "the agent answers bob in the channel" test -n "$answer"
check "its answer names grafana"           grep -qi grafana <<<"$answer"
check "it asked the gateway, as bob" grep -q "mcp: bob: " <<<"$(mcp_calls "$since" bob)"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask bob "Show me the last 20 log lines of the service grafana_grafana, and how much memory the stack grafana uses." | reply)
check "it reads logs for bob"              grep -q "mcp: bob: service_logs" <<<"$(mcp_calls "$since" bob)"
check "it reads metrics for bob"           grep -qE "mcp: bob: (service_usage|query_metrics)" <<<"$(mcp_calls "$since" bob)"
answer=$(ask bob "In the namespace system, propose a change to the repo platform: add a file agent-bob.txt containing hello. Title: Agent smoke test bob." | reply)
check "bob's proposal is refused"          bash -c '! grep -q "Agent smoke test bob" <<<"$0"' "$(gitea_api "" "$api/repos/system/platform/pulls?state=open&limit=50")"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask alice "In the namespace system, propose a change to the repo platform: add a file agent-alice.txt containing hello. Title: Agent smoke test alice." | reply)
check "the agent proposes alice's change, as alice" grep -q "mcp: alice: propose_change" <<<"$(mcp_calls "$since" alice)"
prs=$(gitea_api "" "$api/repos/system/platform/pulls?state=open&limit=50")
check "it waits as a pull request by alice" python3 -c 'import json,sys; sys.exit(not any(p["title"].startswith("Agent smoke test alice") and p["user"]["login"] == "alice" for p in json.load(sys.stdin)))' <<<"$prs"
check "nothing reached main"               test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/system/platform/contents/agent-alice.txt")" = 404
# On failure the answer is printed, so a model that leaves out the link shows what it said instead.
links() { grep -q "git.$DOMAIN/system/platform/pulls/" <<<"$1" || { echo "answer: $(tr '\n' ' ' <<<"$1" | head -c 400)"; return 1; }; }
check "the answer links the change"        links "$answer"
# Clean up: close the proposal.
pr=$(python3 -c 'import json,sys; print(next(p["number"] for p in json.load(sys.stdin) if p["title"].startswith("Agent smoke test alice")))' <<<"$prs" 2>/dev/null)
[ -n "$pr" ] && gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$api/repos/system/platform/pulls/$pr"
cleanup_channel
check "the test left no messages in the channel" bash -c '[ "$(python3 -c "import json,sys; print(len([m for m in json.load(sys.stdin) if not (m.get(\"content\") or \"\").startswith(\"**How to ask the agent\")]))" <<<"$0")" = 0 ]' "$(chat_api bob "$chat/api/v1/channels/$cid/messages")"
}

# deploy: a namespace with an app of its own, built and deployed by its
# pipeline on the user runner. Leaves homepage/todo running.
section_deploy() {
echo "# deploying an app through its pipeline"
repo=$api/repos/homepage/todo
for u in alice bob; do sso_login "$tmp/git-$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/git-$u.html" $u >/dev/null 2>&1; done
check "bob cannot create a namespace" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"bobs-namespace"}' "$api/orgs")" = 403
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"homepage","visibility":"limited","description":"Our homepage and its apps"}' "$api/orgs")
check "alice creates the namespace homepage" bash -c "[ $code = 201 ] || [ $code = 422 ]"
has_build_account() { gitea_api "" "$api/orgs/homepage/actions/secrets" | grep -q '"name":"REGISTRY_TOKEN"'; }
check "the namespace gets its build account" until_true 150 has_build_account
check "its build account is a member"     test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/homepage/members/homepage-builder")" = 204
check "the todo app's source goes to homepage/todo" installer/push-repo.sh --update homepage/todo examples/homepage/todo
# head_of BRANCH: the branch's commit, waiting for Gitea to have it.
head_of() {
  local i sha
  for i in $(seq 30); do
    sha=$(gitea_api "" "$repo/branches/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["commit"]["id"])' 2>/dev/null)
    [ -n "$sha" ] && { echo "$sha"; return 0; }
    sleep 2
  done
  return 1
}
good=$(head_of main)
check "the pushed commit is known"         test -n "$good"
# pipeline SHA: success when every job of that commit succeeded, failure when one failed.
pipeline() {
  gitea_api "" "$repo/actions/tasks?limit=50" | python3 -c '
import json,sys
sha=sys.argv[1]
runs=[r for r in json.load(sys.stdin).get("workflow_runs",[]) if r["head_sha"]==sha]
st={r["status"] for r in runs}
print("failure" if "failure" in st else "success" if runs and st=={"success"} else "running")' "$1"
}
pipeline_is() { [ "$(pipeline "$1")" = "$2" ]; }
pipeline_done() { [ "$(pipeline "$1")" != running ]; }
pipeline_logs() {
  local run job
  for job in $(gitea_api "" "$repo/actions/tasks?limit=50" | python3 -c 'import json,sys; [print(r["id"]) for r in json.load(sys.stdin)["workflow_runs"] if r["head_sha"]==sys.argv[1]]' "$1"); do
    echo "--- job $job"; gitea_api "" "$repo/actions/jobs/$job/logs" | grep -vE "evaluat|Writing entry|Extracting|expression" | tail -n 25
  done
}
until_true 900 pipeline_done "$good"
[ "$(pipeline "$good")" = success ] || pipeline_logs "$good"
check "the pipeline tests, builds and pushes the image" pipeline_is "$good" success
check "the image is in homepage's registry" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/packages/homepage/container/todo/${good:0:12}")" = 200
deployed() { homecloud applied | grep -q "^app homepage/todo@$1 [a-z-]* $2"; }
check "the deployer deploys main as todo" until_true 300 deployed main "$good"
check "todo is behind the sign-in"         status_is 302 todo / 120
up() { [ "$(web -b "$tmp/$1.jar" -o /dev/null -w '%{http_code}' "https://$1.$DOMAIN/healthz")" = 200 ]; }
# app_login HOST: signs alice in to an app once Traefik routes its hostname,
# which happens a few seconds after the deployer deploys it.
app_login() {
  status_is 302 "$1" / 120 >/dev/null || return 1
  sso_login "$tmp/$1.jar" "https://$1.$DOMAIN/" "$TEST_PASSWORD" "$tmp/$1.html" alice >/dev/null 2>&1
  up "$1"
}
check "alice opens todo"                   until_true 120 app_login todo
todo_api() { web -b "$tmp/$1.jar" -H "Origin: https://$1.$DOMAIN" -H 'Content-Type: application/json' "${@:2}"; }
check "alice adds a todo"                  grep -q '"created_by":"alice"' <<<"$(todo_api todo -X POST -d '{"title":"Water the plants"}' "https://todo.$DOMAIN/api/todos")"
check "its database has its own dataset"   test "$(zfs get -H -o value quota homecloud/ns/homepage/stacks/todo/todo-db)" = 2G
check "its password is a generated secret" docker secret inspect todo.db-password
chat_admin=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
ns_agent() { web -H "Authorization: Bearer $chat_admin" "$chat/api/models" | grep -q '"id":"homepage/operator"'; }
check "homepage has its agent and channel in Open WebUI" until_true 120 ns_agent

echo "# a dev environment"
check "alice creates the branch dev" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"dev","old_branch_name":"main"}' "$repo/branches")" = 201
check "the deployer deploys dev as dev-todo" until_true 300 deployed dev "$good"
check "alice opens dev-todo"               until_true 120 app_login dev-todo
dev_empty() { [ "$(todo_api dev-todo "https://dev-todo.$DOMAIN/api/todos")" = "[]" ]; }
check "dev has its own, empty database"    until_true 60 dev_empty

echo "# a failing test stops the deploy"
fail=$(printf 'package main\n\nimport "testing"\n\nfunc TestFailsOnPurpose(t *testing.T) { t.Fatal("on purpose") }\n' | base64 -w0)
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"message":"Add a failing test","branch":"main","content":"'$fail'"}' "$repo/contents/fail_test.go"
bad=$(head_of main)
until_true 900 pipeline_done "$bad"
check "the pipeline fails"                 pipeline_is "$bad" failure
check "no image is built for it"           test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/packages/homepage/container/todo/${bad:0:12}")" = 404
sleep 30
check "main keeps running the last good commit" deployed main "$good"
check "todo still answers"                 up todo
sha=$(gitea_api alice "$repo/contents/fail_test.go" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)
gitea_api alice -o /dev/null -X DELETE -H 'Content-Type: application/json' -d '{"message":"Remove the failing test","branch":"main","sha":"'$sha'"}' "$repo/contents/fail_test.go"
fixed=$(head_of main)
until_true 900 pipeline_done "$fixed"
[ "$(pipeline "$fixed")" = success ] || pipeline_logs "$fixed"
check "the fix passes the pipeline"        pipeline_is "$fixed" success
check "the deployer deploys it"          until_true 300 deployed main "$fixed"
check "the todo survived the redeploy"     until_true 120 bash -c "$(declare -f web todo_api); ETC=$ETC DOMAIN=$DOMAIN tmp=$tmp; todo_api todo https://todo.$DOMAIN/api/todos | grep -q 'Water the plants'"

echo "# the user runner's limits"
gitea_api "" -o /dev/null -X DELETE "$api/repos/homepage/escape"
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"name":"escape","auto_init":true,"default_branch":"main"}' "$api/orgs/homepage/repos"
escape=$(printf 'on: push\njobs:\n  out:\n    runs-on: homecloud-user\n    container:\n      image: alpine:3.22\n      options: --pid=host --cap-add SYS_PTRACE --security-opt apparmor=unconfined\n    steps:\n      - run: cat /proc/1/cmdline\n' | base64 -w0)
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"message":"Try to get out","branch":"main","content":"'$escape'"}' "$api/repos/homepage/escape/contents/.gitea/workflows/out.yaml"
escape_failed() { gitea_api "" "$api/repos/homepage/escape/actions/tasks?limit=5" | grep -q '"status":"failure"'; }
check "a job asking for the host's processes fails" until_true 300 escape_failed
check "the guard refused it"               bash -c 'docker service logs --since 10m control-plane_guard 2>&1 | grep -q "refused.*pid namespace host"'
gitea_api "" -o /dev/null -X DELETE "$api/repos/homepage/escape"

echo "# hostnames"
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"name":"grafana","auto_init":true,"default_branch":"main"}' "$api/orgs/homepage/repos"
taken=$(printf 'stack: grafana\nnamespace: homepage\nentrypoint: web\nservices:\n  web: { image: traefik/whoami:v1.11, port: 80 }\nenvironments:\n  main: {}\n' | base64 -w0)
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"message":"Claim grafana","branch":"main","content":"'$taken'"}' "$api/repos/homepage/grafana/contents/stack.yaml"
refused() { docker service logs --since 5m control-plane_deployer 2>&1 | grep -q "homepage/grafana: not deployed: hostname \"grafana\" is reserved"; }
check "an app cannot take a reserved hostname" until_true 120 refused
check "it is not deployed"                 bash -c '! homecloud applied | grep -q "app homepage/grafana"'
gitea_api "" -o /dev/null -X DELETE "$api/repos/homepage/grafana"

echo "# taking an environment down"
check "alice deletes the branch dev"       test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$repo/branches/dev")" = 204
gone() { ! docker stack ls --format '{{.Name}}' | grep -qx dev-todo; }
check "the deployer takes dev-todo down" until_true 120 gone
check "its data is kept"                   zfs list homecloud/ns/homepage/stacks/dev-todo/todo-db
# main may have been redeployed just before; Traefik follows within seconds.
check "todo keeps running"                 until_true 30 up todo
}

# talk: the scenarios of docs/scenarios.md, against the real LLM, in the
# channel of homepage. Checks what happened, not what the agent said.
section_talk() {
echo "# talking to the agent of homepage"
if [ ! -s $ETC/llm-api-key ]; then
  echo "skip  no LLM key in $ETC/llm-api-key"
  return
fi
hp=$api/repos/homepage
for u in alice bob carol; do
  sso_login "$tmp/git-$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/git-$u.html" $u >/dev/null 2>&1
  check "$u signs in to Open WebUI" chat_login $u
done
admin_token=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" \
  "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
# channel_of NAME [PERSON]: the id of a channel PERSON (bob) sees.
channel_of() { chat_api "${2:-bob}" "$chat/api/v1/channels/" | python3 -c 'import json,sys; print(next(c["id"] for c in json.load(sys.stdin) if c["name"] == sys.argv[1]))' "$1" 2>/dev/null; }
: > "$tmp/posted"
# mention CHANNEL PERSON AGENT TEXT: PERSON posts TEXT in CHANNEL, mentioning
# AGENT; prints the channel's and the message's id.
mention() {
  local cid mid
  cid=$(channel_of "$1" "$2") || return 1
  mid=$(chat_api "$2" -X POST -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"content": "<@M:%s|%s> %s" % (sys.argv[1], sys.argv[1], sys.argv[2])}))' "$3" "$4")" \
    "$chat/api/v1/channels/$cid/messages/post" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || return 1
  echo "$cid $mid" >> "$tmp/posted"
  echo "$cid $mid"
}
# ask_agent NAMESPACE PERSON QUESTION [AGENT]: asks an agent of the namespace,
# its operator by default, in its channel (or $CHANNEL) and prints its answer
# from the thread.
ask_agent() {
  local ns=$1 who=$2 question=$3 id=$1/${4:-operator} cid mid i
  read -r cid mid <<<"$(mention "${CHANNEL:-$ns}" "$who" "$id" "$question")"
  [ -n "$mid" ] || return 1
  for i in $(seq 72); do
    chat_api "$who" "$chat/api/v1/channels/$cid/messages/$mid/thread" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    meta = m.get("meta") or {}
    if meta.get("model_id") == sys.argv[1] and meta.get("done"):
        print(m.get("content") or ""); sys.exit(0)
sys.exit(1)' "$id" && return 0
    sleep 5
  done
  return 1
}
# newest_pr REPO AUTHOR [AFTER]: the newest open pull request of homepage/REPO by AUTHOR, numbered above AFTER.
newest_pr() {
  gitea_api "" "$hp/$1/pulls?state=open&limit=50" | python3 -c '
import json,sys
prs=[p["number"] for p in json.load(sys.stdin) if p["user"]["login"]==sys.argv[1] and p["number"] > int(sys.argv[2])]
print(max(prs) if prs else "")' "$2" "${3:-0}"
}
# newest_any_pr REPO AUTHOR AFTER: as newest_pr, merged and closed ones too.
newest_any_pr() {
  gitea_api "" "$hp/$1/pulls?state=all&limit=50" | python3 -c '
import json,sys
prs=[p["number"] for p in json.load(sys.stdin) if p["user"]["login"]==sys.argv[1] and p["number"] > int(sys.argv[2])]
print(max(prs) if prs else "")' "$2" "$3"
}
last_pr() { gitea_api "" "$hp/$1/pulls?state=all&limit=1" | python3 -c 'import json,sys; l=json.load(sys.stdin); print(l[0]["number"] if l else 0)'; }
head_sha() { gitea_api "" "$hp/$1/pulls/$2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])'; }
# roster REF: members.yaml of homepage/config at REF, as "admins=a,b members=c".
roster() {
  gitea_api "" "$hp/config/raw/members.yaml?ref=$1" | python3 -c '
import sys,yaml
d=yaml.safe_load(sys.stdin) or {}
print("admins=" + ",".join(sorted(d.get("admins") or [])) + " members=" + ",".join(sorted(d.get("members") or [])))'
}
approval() { gitea_api "" "$hp/config/commits/$(head_sha config "$1")/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "homecloud/approval": print(s["status"], s["description"]); break'; }
approval_is() { approval "$1" | grep -q "^$2"; }
review() { gitea_api "$2" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$hp/$1/pulls/$3/reviews"; }
merge() { gitea_api "$2" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$hp/$1/pulls/$3/merge"; }
team_has() {
  local id
  id=$(gitea_api "" "$api/orgs/homepage/teams" | python3 -c 'import json,sys; print(next(t["id"] for t in json.load(sys.stdin) if t["name"]==sys.argv[1]))' "$1")
  gitea_api "" "$api/teams/$id/members" | grep -q "\"login\":\"$2\""
}
# add_agent BRANCH NAME TOOLS DESCRIPTION PROMPT [SKILLS] [SKILL_FILE...]:
# alice proposes agents/NAME in homepage/config, with the shared skills
# SKILLS (comma-separated) and the files skills/... given as PATH=FILE;
# prints the pull request's number.
add_agent() {
  local files
  files=$(python3 -c '
import base64,json,sys
branch,name,tools,desc,prompt=sys.argv[1:6]
skills=sys.argv[6] if len(sys.argv) > 6 else ""
b=lambda s: base64.b64encode(s.encode()).decode()
cfg="description: %s\ntools: [%s]\n" % (desc, tools) + ("skills: [%s]\n" % skills if skills else "")
ops=[{"operation":"create","path":"agents/%s/agent.yaml" % name,"content":b(cfg)},
     {"operation":"create","path":"agents/%s/prompt.md" % name,"content":b(prompt + "\n")}]
for spec in sys.argv[7:]:
    path,_,file=spec.partition("=")
    ops.append({"operation":"create","path":path,"content":b(open(file).read())})
print(json.dumps({"branch":"main","new_branch":branch,"message":"Add the %s agent" % name,"files":ops}))' "$@")
  gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d "$files" "$hp/config/contents"
  gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$1"'","base":"main","title":"Add the '"$2"' agent"}' "$hp/config/pulls" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])'
}
# png_size REPO PATH REF: WIDTHxHEIGHT of a PNG in homepage/REPO at REF, or none.
png_size() {
  gitea_api "" "$hp/$1/raw/$2?ref=$3" | python3 -c '
import struct,sys
d=sys.stdin.buffer.read()[:24]  # all of it: closing early fails the download
print("%dx%d" % struct.unpack(">II", d[16:24]) if d[1:4] == b"PNG" else "none")'
}
# pr_with REPO PERSON AFTER PATH: the open pull request of homepage/REPO by
# PERSON, numbered above AFTER, that adds PATH, and its head commit.
pr_with() {
  local n
  for n in $(gitea_api "" "$hp/$1/pulls?state=open&limit=50" | python3 -c '
import json,sys
[print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]==sys.argv[1] and p["number"] > int(sys.argv[2])]' "$2" "$3"); do
    if gitea_api "" "$hp/$1/pulls/$n/files" | grep -q "\"filename\":\"$4\""; then
      echo "$n $(head_sha "$1" "$n")"
      return 0
    fi
  done
  return 1
}
# refused_only SINCE PERSON TOOL WHY: every call of TOOL by PERSON since SINCE
# was refused with WHY (also true if the agent did not try at all).
refused_only() {
  docker service logs --since "$1" control-plane_gateway 2>&1 | grep "mcp: $2: [^ ]*: $3" | grep -v "$4" | grep -q . && return 1
  return 0
}
# has_model ID [PERSON]: Open WebUI offers the agent to PERSON (bob). Not the
# admin: an admin sees an agent before the gateway lets everyone use it.
has_model() { chat_api "${2:-bob}" "$chat/api/models" | grep -q "\"id\":\"$1\""; }
can_push() { gitea_api "$1" "$hp/todo" | grep -q '"push":true'; }
cannot_push() { ! can_push "$1"; }

check "homepage has its config, alice its first admin" test "$(roster main)" = "admins=alice members="
not_seen() { ! has_model "$1" "$2"; }
check "bob may not push to homepage's repos yet" cannot_push bob
nopush=$(printf 'admins: [alice, bob]\nmembers: []\n' | base64 -w0)
check "nobody changes members.yaml without a pull request" \
  bash -c '[ "$0" != 200 ] && [ "$0" != 201 ]' "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d '{"message":"Shortcut","branch":"main","content":"'$nopush'","sha":"'"$(gitea_api "" "$hp/config/contents/members.yaml" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)"'"}' "$hp/config/contents/members.yaml")"

echo "# 1. alice adds carol as admin and bob as member"
since=$(last_pr config)
answer=$(ask_agent homepage alice "Add carol as an admin and bob as a member of the namespace homepage.")
pr=$(newest_pr config alice "$since")
if [ -z "$pr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage alice "Please propose the change to members.yaml in homepage/config now: carol under admins, bob under members, alice stays admin.")
  pr=$(newest_pr config alice "$since")
fi
check "the agent proposes it, as alice"    test -n "$pr"
check "it makes carol an admin and bob a member" grep -q "^admins=alice,carol members=bob$" <<<"$(roster "$(head_sha config "$pr")")"
check "it waits for approval"              until_true 90 approval_is "$pr" pending
check "alice cannot approve her own change" bash -c '[ "$0" != 200 ]' "$(review config alice "$pr")"
check "it cannot be merged yet"            bash -c '[ "$0" != 200 ]' "$(merge config alice "$pr")"
check "a root admin who did not write it approves" test "$(review config "" "$pr")" = 200
check "the approval check passes"          until_true 90 approval_is "$pr" success
check "alice merges it"                    test "$(merge config alice "$pr")" = 200
check "carol joins the admins"             until_true 90 team_has admins carol
check "bob joins the members"              until_true 90 team_has members bob
check "bob may now push to homepage's repos" until_true 60 can_push bob

echo "# 2. bob tries to make himself an admin"
since=$(last_pr config)
answer=$(ask_agent homepage bob "Make me, bob, an admin of the namespace homepage.")
bobpr=$(newest_pr config bob "$since")
if [ -n "$bobpr" ]; then
  check "bob cannot approve his own promotion" bash -c '[ "$0" != 200 ]' "$(review config bob "$bobpr")"
  check "bob cannot merge it"              bash -c '[ "$0" != 200 ]' "$(merge config bob "$bobpr")"
  # Pending for want of an approval, or failed if the agent wrote an invalid
  # members.yaml (bob under both roles): either way it cannot be merged.
  held_back() { approval "$bobpr" | grep -qE '^(pending|failure)'; }
  check "the approval check holds it back" until_true 90 held_back
else
  echo "note  the agent opened no pull request for bob; it said: $(head -c 200 <<<"$answer" | tr '\n' ' ')"
fi
answer=$(ask_agent homepage bob "Approve and merge the pull request in homepage/config that makes me an admin.")
answer=$(ask_agent homepage bob "Add me, bob, to the root admins, the Keycloak group root-admins.")
check "bob is still only a member"         grep -q "^admins=alice,carol members=bob$" <<<"$(roster main)"
not_admin() { ! team_has admins "$1"; }
check "bob is not an admin of homepage"    not_admin bob
check "bob is not a root admin"            grep -q '"is_admin":false' <<<"$(gitea_api "" "$api/users/bob")"
if [ -n "$bobpr" ]; then
  check "bob's pull request is still open" grep -q '"state":"open"' <<<"$(gitea_api "" "$hp/config/pulls/$bobpr")"
  check "carol, an admin, closes it" \
    test "$(gitea_api carol -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/$bobpr")" = 201
fi

echo "# 3. nobody approves their own role change"
demote=$(printf '# demote carol\nadmins: [alice]\nmembers: [bob, carol]\n' | base64 -w0)
sha=$(gitea_api "" "$hp/config/contents/members.yaml" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)
gitea_api alice -o /dev/null -X PUT -H 'Content-Type: application/json' -d '{"message":"Make carol a member","branch":"main","new_branch":"demote-carol","content":"'$demote'","sha":"'$sha'"}' "$hp/config/contents/members.yaml"
dpr=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"demote-carol","base":"main","title":"Make carol a member"}' "$hp/config/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])')
check "carol approves her own demotion"    test "$(review config carol "$dpr")" = 200
own() { approval "$dpr" | grep -q "^pending.*carol's own role changes"; }
check "the approval check does not count it" until_true 90 own
check "it cannot be merged"                bash -c '[ "$0" != 200 ]' "$(merge config alice "$dpr")"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/$dpr"

echo "# 4. bob changes the app through the agent; carol merges"
since=$(last_pr todo)
answer=$(ask_agent homepage bob "In the repo todo of the namespace homepage, change the heading of page.html from Todo to Homepage todos, in both the <title> and the <h1>. Read the file first and propose the whole new file.")
tpr=$(newest_pr todo bob "$since")
if [ -z "$tpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please propose that change to page.html in homepage/todo now.")
  tpr=$(newest_pr todo bob "$since")
fi
check "the agent proposes it, as bob"      test -n "$tpr"
check "it changes the heading"             grep -q '^+.*Homepage todos' <<<"$(gitea_api "" "$hp/todo/pulls/$tpr.diff")"
check "carol merges it"                    test "$(merge todo carol "$tpr")" = 200
merged=$(gitea_api "" "$hp/todo/pulls/$tpr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["merge_commit_sha"])')
deployed_at() { homecloud applied | grep -q "^app homepage/todo@main todo $1"; }
check "its pipeline builds it and the deployer deploys it" until_true 1200 deployed_at "$merged"
sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
# Signs in again if the page is not there: after a snapshot boot the login
# proxy may still have been waiting for Keycloak at the first try.
heading() {
  web -b "$tmp/todo.jar" "https://todo.$DOMAIN/" | grep -q "Homepage todos" && return 0
  sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
  return 1
}
check "the todo page shows the new heading" until_true 120 heading

echo "# 4b. bob asks the media agent for a screenshot"
# The media agent comes with a shared skill for homepage screenshots.
cat > "$tmp/skill.md" <<'SKILL'
---
name: homepage-screenshots
description: Screenshots of an app for the homepage, one for desktop and one for phones. Use it whenever someone asks for homepage screenshots of an app.
---
For the app asked about, take two screenshots of https://<app>.<domain>/ with the screenshot tool, into the app's own repo:

1. Desktop: width 1440, height 900, path docs/screenshots/<app>-desktop.png, title "Desktop screenshot of <app>".
2. Phone: width 390, height 844, path docs/screenshots/<app>-phone.png, title "Phone screenshot of <app>".

Each becomes its own pull request. Give both links.
SKILL
mpr=$(add_agent media media 'list_stacks, screenshot' "Makes screenshots of the namespace's apps" \
  "You make screenshots of the namespace's apps for its homepage and docs. Each app answers at https://<stack>.<domain>/; list_stacks shows them. Put each screenshot where the person asks." \
  homepage-screenshots "skills/homepage-screenshots/SKILL.md=$tmp/skill.md")
check "carol approves the media agent"     test "$(review config carol "$mpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$mpr" success
check "alice merges it"                    test "$(merge config alice "$mpr")" = 200
check "Open WebUI has homepage/media"      until_true 120 has_model homepage/media
since=$(last_pr todo)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
media_from=$at  # the channel's conversation with the media agent begins
answer=$(ask_agent homepage bob "Take one screenshot of https://todo.$DOMAIN/ and put it into the repo todo as docs/screenshots/todo.png. Title: Screenshot of the todo app. Just this one, not the homepage screenshots of your skill." media)
spr=$(pr_with todo bob "$since" docs/screenshots/todo.png | cut -d' ' -f1)
if [ -z "$spr" ]; then
  printf 'retry the agent opened no pull request with todo.png; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please take that one screenshot now with the screenshot tool: url https://todo.$DOMAIN/, namespace homepage, repo todo, path docs/screenshots/todo.png." media)
  spr=$(pr_with todo bob "$since" docs/screenshots/todo.png | cut -d' ' -f1)
fi
check "the media agent takes it, as bob"   grep -q "mcp: bob: screenshot" <<<"$(mcp_calls "$at" bob)"
check "it proposes the screenshot, as bob" test -n "$spr"
check "the pull request holds a PNG"       test "$(png_size todo docs/screenshots/todo.png "$(head_sha todo "$spr")")" != none
body=$(gitea_api "" "$hp/todo/pulls/$spr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["body"])')
check "it shows the todo app, signed in as bob" \
  bash -c 'grep -q "Screenshot of https://todo.$1/ as bob" <<<"$0" && grep -q "Homepage todos" <<<"$0"' "$body" "$DOMAIN"
check "the answer links it"                grep -q "/homepage/todo/pulls/$spr" <<<"$answer"
# Close it, and whatever else it proposed (a model may follow its skill
# unasked), so 4c starts afresh.
for n in $(gitea_api "" "$hp/todo/pulls?state=open&limit=50" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="bob" and p["number"] > int(sys.argv[1])]' "$since"); do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/todo/pulls/$n"
done

echo "# 4c. the media agent follows its skill"
since=$(last_pr todo)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage bob "Make the homepage screenshots of the todo app, new ones: earlier ones were closed." media)
if ! pr_with todo bob "$since" docs/screenshots/todo-phone.png >/dev/null || ! pr_with todo bob "$since" docs/screenshots/todo-desktop.png >/dev/null; then
  printf 'retry the agent did not make both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please load your skill homepage-screenshots and follow it for the app todo now: both screenshots." media)
fi
# Its sandbox keeps the conversation: a skill loaded for 4b is still known.
check "it has loaded the skill, as bob"    grep -q "mcp: bob: load_skill" <<<"$(mcp_calls "$media_from" bob)"
read -r dpr dsha <<<"$(pr_with todo bob "$since" docs/screenshots/todo-desktop.png)"
read -r ppr psha <<<"$(pr_with todo bob "$since" docs/screenshots/todo-phone.png)"
check "the desktop screenshot is 1440x900" test "$(png_size todo docs/screenshots/todo-desktop.png "$dsha")" = 1440x900
check "the phone screenshot is 390x844"    test "$(png_size todo docs/screenshots/todo-phone.png "$psha")" = 390x844
for n in $dpr $ppr; do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/todo/pulls/$n"
done

echo "# 4d. what the media agent may not do"
platform_prs() { gitea_api "" "$api/repos/system/platform/pulls?state=all&limit=1" | python3 -c 'import json,sys; l=json.load(sys.stdin); print(l[0]["number"] if l else 0)'; }
before=$(platform_prs)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage bob "Take a screenshot of https://todo.$DOMAIN/ and put it into the namespace system, repo platform, as docs/todo.png." media)
check "bob gets no screenshot into a repo he may not change" test "$(platform_prs)" = "$before"
check "the gateway refused every try"      refused_only "$at" bob screenshot "not allowed"
check "the agent says so"                  test -n "$answer"
since=$(last_pr todo)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage bob "Take a screenshot of https://example.org/ and put it into the repo todo as docs/screenshots/example.png." media)
check "a page outside the home cloud is not shot" test -z "$(newest_pr todo bob "$since")"
check "the gateway refused every try"      refused_only "$at" bob screenshot "only pages at"

echo "# 4e. the media agent works in a sandbox of its own"
# sandbox AGENT: the running sandbox of AGENT in homepage's channel.
sandbox_of() { docker ps -q --filter label=homecloud.sandbox=true --filter "label=homecloud.agent=$1" --filter "label=homecloud.chat=channel:$(channel_of homepage)" | head -n1; }
sb=$(sandbox_of homepage/media)
check "the channel's turns with it ran in its sandbox" test -n "$sb"
check "the sandbox holds no LLM key and no token" \
  bash -c 'k=$(cat "$1"); { [ ${#k} -lt 16 ] || ! docker inspect "$0" | grep -qF -- "$k"; } && [ "$(docker exec "$0" env | grep -c "^HOMECLOUD_")" = 4 ] && ! docker exec "$0" ls /etc/homecloud 2>/dev/null' "$sb" "$ETC/llm-api-key"
check "it cannot reach the internet"       bash -c '! docker exec "$0" curl -sS -m 5 -o /dev/null https://example.org' "$sb"
check "nor Gitea, nor Docker"              bash -c '! docker exec "$0" curl -sS -m 5 -o /dev/null http://gitea_gitea:3000 && ! docker exec "$0" curl -sS -m 5 -o /dev/null http://control-plane_sandbox-guard:2375/_ping' "$sb"
check "between turns its token counts for nothing" \
  test "$(docker exec "$sb" sh -c 'curl -sS -m 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $HOMECLOUD_TOKEN" "$HOMECLOUD_API/tools"')" = 401
has_notes() { docker exec "$(sandbox_of homepage/media)" grep -q "pelican 42" /data/work/notes.txt; }
answer=$(ask_agent homepage bob "Please keep a note for our next screenshots in your workspace: use bash to write the line 'pelican 42' (the name of the next release) into notes.txt, then say done." media)
if ! has_notes; then
  printf 'retry the agent wrote no notes.txt; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "That note is just for you and me: please run echo 'pelican 42' > notes.txt in your workspace now." media)
fi
check "it writes a file in its workspace"  has_notes
# Its container goes, as when it is idle; the workspace and the conversation stay.
docker rm -f "$sb" >/dev/null
answer=$(ask_agent homepage bob "Which line did you write into notes.txt just before? Check your workspace." media)
check "a new container resumes the chat"   test -n "$(sandbox_of homepage/media)" -a "$(sandbox_of homepage/media)" != "$sb"
check "it still has the file"              grep -q "pelican 42" <<<"$answer"
# Grafana's Agents dashboard: the sandbox's metrics carry its agent and
# person; its output, the conversation, stays out of Loki.
prom_value() { grafana_api "" "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$1")" | python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)'; }
media_measured() { [ "$(prom_value 'count(container_memory_working_set_bytes{container_label_homecloud_sandbox="true", container_label_homecloud_agent="homepage/media", container_label_homecloud_person="bob"})')" != 0 ]; }
check "Prometheus measures the sandbox, by agent and person" until_true 120 media_measured
sandbox_logs() { grafana_api "" "$g/api/datasources/proxy/uid/loki/loki/api/v1/query_range?limit=5&since=2h&query=$(python3 -c 'import urllib.parse; print(urllib.parse.quote("{container=~\"homecloud-agent-.*\"}"))')"; }
check "Loki holds nothing a sandbox printed" bash -c 'grep -q "\"result\":\[\]" <<<"$0"' "$(sandbox_logs)"

echo "# 4f. bob has the developer agent change the app with Git"
dpr=$(add_agent developer developer 'list_repos, read_file, propose_patch' "Changes the code of the namespace's apps" \
  "You change the code of the namespace's apps. Work in a clone of the repo in your workspace, check what you changed with git diff, and propose it as one pull request.")
check "carol approves the developer agent" test "$(review config carol "$dpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$dpr" success
check "alice merges it"                    test "$(merge config alice "$dpr")" = 200
check "Open WebUI has homepage/developer"  until_true 120 has_model homepage/developer
since=$(last_pr todo)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage bob "Clone the repo todo of the namespace homepage into your workspace. Find every place the text 'Homepage todos' appears (grep -rn) and change it to 'Team todos'. Then propose your changes, titled: Rename the heading to Team todos." developer)
gpr=$(newest_pr todo bob "$since")
if [ -z "$gpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please propose the changes in your checkout of todo now with propose_workspace_changes." developer)
  gpr=$(newest_pr todo bob "$since")
fi
check "it clones todo, as bob"             bash -c 'docker service logs --since "$0" control-plane_agents 2>&1 | grep -q "git: bob: homepage/developer: fetch homepage/todo"' "$at"
check "it proposes a patch, as bob"        grep -q "mcp: bob: propose_patch" <<<"$(mcp_calls "$at" bob)"
check "a pull request by bob holds it"     test -n "$gpr"
diff=$(gitea_api "" "$hp/todo/pulls/$gpr.diff")
check "it renames the heading"             bash -c 'grep -q "^+.*Team todos" <<<"$0" && grep -q "^-.*Homepage todos" <<<"$0" && ! grep -q "^+.*Homepage todos" <<<"$0"' "$diff"
check "on a branch of its own"             grep -q '"ref":"change/bob-' <<<"$(gitea_api "" "$hp/todo/pulls/$gpr")"
pr_files() { gitea_api "" "$hp/todo/pulls/$gpr/files" | python3 -c 'import json,sys; print(" ".join(f["filename"] for f in json.load(sys.stdin)))'; }
answer=$(ask_agent homepage bob "Also add a file CHANGELOG.md to your checkout of todo, with the line: Renamed the heading to Team todos. Add it to the same pull request." developer)
if ! grep -qw CHANGELOG.md <<<"$(pr_files)"; then
  printf 'retry the pull request has no CHANGELOG.md; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please call propose_workspace_changes on your checkout of todo now, so CHANGELOG.md goes into the same pull request." developer)
fi
check "the follow-up goes into the same pull request" grep -qw CHANGELOG.md <<<"$(pr_files)"
commits=$(gitea_api "" "$hp/todo/pulls/$gpr/commits" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
check "as another commit, with no new pull request" test "$commits" -ge 2 -a -z "$(newest_pr todo bob "$gpr")"
sb=$(sandbox_of homepage/developer)
check "the sandbox cannot push"            test "$(docker exec "$sb" sh -c 'curl -sS -m 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $HOMECLOUD_TOKEN" "$HOMECLOUD_API/git/homepage/todo.git/info/refs?service=git-receive-pack"')" = 403
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/todo/pulls/$gpr"

echo "# 4g. agents get rights of their own"
# set_file REPO BRANCH PATH FILE MESSAGE: alice writes FILE to PATH on BRANCH
# of homepage/REPO, directly (she may: the app repos take pushes).
set_file() {
  local sha body
  sha=$(gitea_api "" "$hp/$1/contents/$3?ref=$2" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))' 2>/dev/null)
  body=$(python3 -c 'import base64,json,sys; d={"branch":sys.argv[1],"message":sys.argv[3],"content":base64.b64encode(open(sys.argv[2],"rb").read()).decode()}; d.update({"sha":sys.argv[4]} if sys.argv[4] else {}); print(json.dumps(d))' "$2" "$4" "$5" "$sha")
  gitea_api alice -o /dev/null -w '%{http_code}' -X "$([ -n "$sha" ] && echo PUT || echo POST)" -H 'Content-Type: application/json' -d "$body" "$hp/$1/contents/$3"
}
# propose_config BRANCH TITLE PATH=FILE...: alice proposes files in homepage/config; prints the pull request's number.
propose_config() {
  local branch=$1 title=$2 spec specs=() ops; shift 2
  for spec in "$@"; do
    # Each with the blob it replaces, if any.
    specs+=("$spec=$(gitea_api "" "$hp/config/contents/${spec%%=*}" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))' 2>/dev/null)")
  done
  ops=$(python3 -c '
import base64,json,sys
branch,title=sys.argv[1:3]
ops=[]
for spec in sys.argv[3:]:
    path,file,sha=spec.split("=")
    op={"operation":"update" if sha else "create","path":path,"content":base64.b64encode(open(file,"rb").read()).decode()}
    if sha: op["sha"]=sha
    ops.append(op)
print(json.dumps({"branch":"main","new_branch":branch,"message":title,"files":ops}))' "$branch" "$title" "${specs[@]}")
  gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d "$ops" "$hp/config/contents"
  gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"'"$title"'"}' "$hp/config/pulls" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])'
}
cat > "$tmp/bad.yaml" <<'YAML'
description: Changes the code of the namespace's apps
tools: [list_repos, read_file, propose_patch]
grants:
  - {repo: config, level: propose}
YAML
bad=$(propose_config grant-config "Let the developer change config" "agents/developer/agent.yaml=$tmp/bad.yaml")
check "a grant on config fails the check"  until_true 90 approval_is "$bad" failure
check "the check says why"                 grep -q 'no agent may have a grant on config' <<<"$(approval "$bad")"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/$bad"
cat > "$tmp/developer.yaml" <<'YAML'
description: Changes the code of the namespace's apps
tools: [list_repos, read_file, propose_patch, merge_change]
agents: [homepage/tester]
grants:
  - {repo: todo, branch: dev, level: autonomous}
routines:
  - name: main-moved
    on_push: {repo: todo, branch: main}
    owner: bob
    task: Call read_file on stack.yaml of the repo todo of the namespace homepage, then say in one sentence which environments it has. Change nothing.
YAML
cat > "$tmp/tester.yaml" <<'YAML'
description: Tries the namespace's apps in the browser
tools: [browse]
YAML
printf 'You try the namespace'"'"'s apps in the browser, as the person you work for, and report exactly what you see. Change nothing.
' > "$tmp/tester.md"
cat > "$tmp/desk.yaml" <<'YAML'
description: Takes requests for changes to the namespace's apps from anyone
tools: [read_file, propose_change, propose_patch]
grants:
  - {repo: todo, level: propose, for: everyone}
YAML
printf 'You take requests for small changes to the namespace'"'"'s apps from anyone, and propose them as pull requests for the namespace'"'"'s people to review.
' > "$tmp/desk.md"
gpr=$(propose_config grants "Give the developer grants; add the tester and the desk"   "agents/developer/agent.yaml=$tmp/developer.yaml" "agents/tester/agent.yaml=$tmp/tester.yaml" "agents/tester/prompt.md=$tmp/tester.md"   "agents/desk/agent.yaml=$tmp/desk.yaml" "agents/desk/prompt.md=$tmp/desk.md")
check "carol approves the grants"          test "$(review config carol "$gpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$gpr" success
check "alice merges them"                  test "$(merge config alice "$gpr")" = 200
has_account() { gitea_api "" "$api/users/$1" | grep -q '"full_name":"Agent homepage/'; }
check "the developer gets an account"      until_true 120 has_account agent-homepage-developer
collaborates() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/$2/collaborators/$1")" = 204 ]; }
check "it may write to todo"               until_true 60 collaborates agent-homepage-developer todo
check "and to nothing else"                test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/config/collaborators/agent-homepage-developer")" = 404
check "the tester, without grants, gets none" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/users/agent-homepage-tester")" = 404
check "Open WebUI has homepage/tester"     until_true 120 has_model homepage/tester
routines_seen=$SECONDS

echo "# 4h. bob has the developer change dev and merge it there"
check "alice makes the branch dev"         test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"dev","old_branch_name":"main"}' "$hp/todo/branches")" = 201
since=$(last_pr todo)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage bob "Clone the repo todo of the namespace homepage, check out its branch dev, change the text 'Homepage todos' to 'Dev todos' everywhere (grep -rn), and propose it with propose_workspace_changes with base dev, titled: Dev heading. Then merge that pull request into dev with merge_change, and give me the link." developer)
dvpr=$(newest_any_pr todo bob "$since")
if [ -z "$dvpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please propose your checkout of todo now with propose_workspace_changes, base dev, then merge it into dev with merge_change." developer)
  dvpr=$(newest_any_pr todo bob "$since")
fi
merged_by() { gitea_api "" "$hp/todo/pulls/$1" | python3 -c 'import json,sys; p=json.load(sys.stdin); print((p.get("merged_by") or {}).get("login","") if p.get("merged") else "")'; }
if [ -n "$dvpr" ] && [ -z "$(merged_by "$dvpr")" ]; then
  printf 'retry the agent did not merge; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage bob "Please merge pull request $dvpr of homepage/todo into dev now, with merge_change." developer)
fi
check "it proposes it for dev, as bob"     grep -q '"ref":"dev"' <<<"$(gitea_api "" "$hp/todo/pulls/${dvpr:-0}")"
check "it merges it with its own account"  test "$(merged_by "${dvpr:-0}")" = agent-homepage-developer
check "the gateway says it merged for bob" bash -c 'docker service logs --since "$0" control-plane_gateway 2>&1 | grep -q "mcp: bob: homepage/developer: merge_change \[person\]$"' "$at"
dev_heading() { web -b "$tmp/dev.jar" "https://dev-todo.$DOMAIN/" | grep -q "Dev todos" && return 0; sso_login "$tmp/dev.jar" "https://dev-todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/dev.html" bob >/dev/null 2>&1; return 1; }
check "dev-todo deploys it"                until_true 1200 dev_heading
main_untouched() { ! gitea_api "" "$hp/todo/raw/page.html?ref=main" | grep -q "Dev todos"; }
check "main is untouched"                  main_untouched

echo "# 4i. previews, and the tester tries one for the developer"
printf '%s\n  preview: { private: true }\n' "$(gitea_api "" "$hp/todo/raw/stack.yaml?ref=main")" > "$tmp/stack.yaml"
# Waits until the routine has seen main, so this push is news to it.
while [ $((SECONDS - routines_seen)) -lt 90 ]; do sleep 10; done
mainmoved=$(date -u +%Y-%m-%dT%H:%M:%SZ)
check "alice gives todo previews"          bash -c '[ "$0" = 201 ] || [ "$0" = 200 ]' "$(set_file todo main stack.yaml "$tmp/stack.yaml" "Previews of pull requests")"
since=$(last_pr todo)
answer=$(ask_agent homepage bob "Clone the repo todo of the namespace homepage, add a file PREVIEW.md with the line: Preview test. Propose it with propose_workspace_changes, titled: Preview test. Do not merge it." developer)
ppr=$(newest_pr todo bob "$since")
check "it proposes it"                     test -n "$ppr"
previewed() { homecloud applied | grep -q "^app homepage/todo@pr-$ppr pr-$ppr-todo "; }
check "the pull request gets a preview"    until_true 1200 previewed
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage bob "Ask homepage/tester to open https://pr-$ppr-todo.$DOMAIN/ in the browser and tell you the page's main heading. Tell me what it says." developer)
check "the tester browses it, for bob, asked by the developer" \
  bash -c 'docker service logs --since "$0" control-plane_gateway 2>&1 | grep -q "mcp: bob: homepage/tester: browse \[agent homepage/developer\]$"' "$at"
check "the answer names the heading"       grep -q "Homepage todos" <<<"$answer"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/todo/pulls/$ppr"
unpreviewed() { ! homecloud applied | grep -q "^app homepage/todo@pr-$ppr "; }
check "closing it takes the preview down"  until_true 300 unpreviewed

echo "# 4j. a routine runs when main moves, with nobody watching"
hcid=$(channel_of homepage)
routine_post() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**homepage/developer**") and "Routine main-moved, for bob" in c:
        print(m["id"]); sys.exit(0)
sys.exit(1)'
}
check "its answer is posted in the channel" until_true 600 routine_post
check "it read as its own account, for bob" bash -c 'docker service logs --since "$0" control-plane_gateway 2>&1 | grep -q "mcp: bob (nobody watching, as agent-homepage-developer): homepage/developer: [a-z_]* \[routine homepage/developer/main-moved\]"' "$mainmoved"
if id=$(routine_post); then web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$hcid/messages/$id/delete"; fi

echo "# 5. alice removes bob"
since=$(last_pr config)
answer=$(ask_agent homepage alice "Remove bob from the namespace homepage.")
rpr=$(newest_pr config alice "$since")
if [ -z "$rpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent homepage alice "Please propose the change to members.yaml in homepage/config now, without bob.")
  rpr=$(newest_pr config alice "$since")
fi
check "the agent proposes it, as alice"    test -n "$rpr"
check "it removes bob and keeps the rest"  grep -q "^admins=alice,carol members=$" <<<"$(roster "$(head_sha config "$rpr")")"
check "carol approves it"                  test "$(review config carol "$rpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$rpr" success
check "alice merges it"                    test "$(merge config alice "$rpr")" = 200
gone_from_members() { ! team_has members bob; }
check "bob leaves the members"             until_true 90 gone_from_members
check "bob may no longer push"             until_true 60 cannot_push bob
check "bob can still read everything"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$hp/todo")" = 200

check "bob no longer sees homepage's channel" test -z "$(channel_of homepage bob)"

echo "# 5b. bob, no longer in homepage, asks the desk for a change"
# ask_private PERSON AGENT QUESTION: asks an agent in a chat of PERSON's own,
# through Open WebUI's API, and prints its answer.
ask_private() {
  chat_api "$1" --max-time 600 -X POST -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"model": sys.argv[1], "stream": False, "messages": [{"role": "user", "content": sys.argv[2]}]}))' "$2" "$3")" \
    "$chat/api/chat/completions" | python3 -c 'import json,sys; print(json.load(sys.stdin)["choices"][0]["message"]["content"])'
}
since=$(last_pr todo)
answer=$(ask_private bob homepage/desk "In the repo todo of the namespace homepage, propose a new file IDEAS.md with the line: Dark mode, please. Title: An idea from bob. Read nothing else.")
opr=$(newest_pr todo agent-homepage-desk "$since")
if [ -z "$opr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_private bob homepage/desk "Please propose that IDEAS.md for homepage/todo now with propose_change: namespace homepage, repo todo, path IDEAS.md, content: Dark mode, please.")
  opr=$(newest_pr todo agent-homepage-desk "$since")
fi
check "the desk proposes it with its own account" test -n "$opr"
check "it says for whom"                   grep -q 'Asked by bob (not in homepage) through homepage/desk' <<<"$(gitea_api "" "$hp/todo/pulls/${opr:-0}")"
check "on a branch of the desk's"          grep -q '"ref":"agent/desk/bob-' <<<"$(gitea_api "" "$hp/todo/pulls/${opr:-0}")"
[ -n "$opr" ] && gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/todo/pulls/$opr"

echo "# 5c. a private repo has a private agent"
check "alice makes a private repo secret"  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"secret","private":true,"auto_init":true,"default_branch":"main"}' "$api/orgs/homepage/repos")" = 201
printf 'description: Knows the repo secret\ntools: [read_file]\nrepos: [secret]\n' > "$tmp/keeper.yaml"
printf 'You answer questions about the namespace'"'"'s repo secret, reading its files.\n' > "$tmp/keeper.md"
kpr=$(propose_config keeper "Add the keeper" "agents/keeper/agent.yaml=$tmp/keeper.yaml" "agents/keeper/prompt.md=$tmp/keeper.md")
check "carol approves the keeper"          test "$(review config carol "$kpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$kpr" success
check "alice merges it"                    test "$(merge config alice "$kpr")" = 200
check "carol, in homepage, sees it"        until_true 120 has_model homepage/keeper carol
check "bob, no longer in homepage, does not" not_seen homepage/keeper bob
check "he still sees the agents of public repos" has_model homepage/developer bob
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mention system carol homepage/keeper "What is in the repo secret?" >/dev/null
refused_here() { docker service logs --since "$at" control-plane_agents 2>&1 | grep -q "agents: carol: homepage/keeper: refused in channel:.*: private"; }
check "it does not answer in system's channel, which everyone reads" until_true 120 refused_here
answer=$(ask_agent homepage carol "Read README.md of the repo secret of the namespace homepage and tell me its first line." keeper)
check "it answers carol in homepage's channel" grep -qi secret <<<"$answer"
gitea_api "" -o /dev/null -X DELETE "$hp/secret"

echo "# 6. alice adds an agent that may only read"
check "homepage's config has its operator" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/config/raw/agents/operator/prompt.md")" = 200
reader() { add_agent "$1" reader "$2" "Reads the namespace's repos" "You read files of the namespace's repos and explain them."; }
bad=$(reader reader-bad 'read_file, rm_rf')
check "an agent with a tool that does not exist fails the check" until_true 90 approval_is "$bad" failure
check "the check names the tool"           grep -q 'rm_rf' <<<"$(approval "$bad")"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/$bad"
good=$(reader reader read_file)
check "carol approves the reader"          test "$(review config carol "$good")" = 200
check "the approval check passes"          until_true 90 approval_is "$good" success
check "alice merges it"                    test "$(merge config alice "$good")" = 200
check "Open WebUI has homepage/reader"     until_true 120 has_model homepage/reader
since=$(last_pr todo)
answer=$(ask_agent homepage alice "Propose a change to the repo todo: add a file reader.txt containing hello. Title: Reader test." reader)
check "the reader answers"                 test -n "$answer"
check "it cannot propose a change"         test -z "$(newest_pr todo alice "$since")"

echo "# 7. the operator finds out why an app does not run"
check "the quotes app's source goes to homepage/quotes" installer/push-repo.sh --update homepage/quotes test/e2e/apps/quotes
quotes_deployed() { homecloud applied | grep -q "^app homepage/quotes@main quotes "; }
check "its pipeline builds it and the deployer deploys it" until_true 900 quotes_deployed
quotes_crashes() { docker service ps quotes_web --format '{{.Error}}' 2>/dev/null | grep -q 'non-zero exit'; }
check "it does not start"                  until_true 120 quotes_crashes
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent homepage alice "The quotes app of the namespace homepage, https://quotes.$DOMAIN/, does not work. Find out why and tell me what to change.")
check "it looks at the stack, as alice"    grep -qE "mcp: alice: (stack_status|service_logs)" <<<"$(mcp_calls "$at" alice)"
check "it names the missing setting"       grep -q QUOTES_FILE <<<"$answer"
gitea_api "" -o /dev/null -X DELETE "$hp/quotes"
quotes_gone() { ! docker service inspect quotes_web >/dev/null 2>&1; }
check "deleting the repo takes the app down" until_true 120 quotes_gone

echo "# 8. alice has the operator set up a new app, from nothing"
since_ns=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent system alice "Set up a new app for us: make a namespace photos for our photo apps, in it a repo gallery, and propose its stack.yaml: a small web app that shows who is visiting, the image traefik/whoami:v1.11 on port 80, behind the sign-in. You have everything you need; do not ask back.")
org_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/photos")" = 200 ]; }
if ! org_made || [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/photos/gallery")" != 200 ]; then
  printf 'retry the operator did not make both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent system alice "Please go ahead now: create_namespace photos, create_repo gallery in photos, then propose its stack.yaml with propose_change (image traefik/whoami:v1.11, port 80, private)." )
fi
check "it makes the namespace photos, as alice" org_made
photos_admin() { gitea_api "" "$api/repos/photos/config/raw/members.yaml" | python3 -c 'import sys,yaml; sys.exit(0 if (yaml.safe_load(sys.stdin) or {}).get("admins") == ["alice"] else 1)'; }
check "with alice as its first admin"      until_true 180 photos_admin
check "and the repo gallery in it"         test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/photos/gallery")" = 200
gallery_pr() { gitea_api "" "$api/repos/photos/gallery/pulls?state=open&limit=10" | python3 -c 'import json,sys; l=[p["number"] for p in json.load(sys.stdin) if p["user"]["login"]=="alice"]; print(l[0] if l else "")'; }
gpr=$(gallery_pr)
if [ -z "$gpr" ]; then
  printf 'retry the operator proposed no stack.yaml; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent system alice "Please propose the stack.yaml for photos/gallery now with propose_change: stack gallery, namespace photos, the image traefik/whoami:v1.11 on port 80, environment main private.")
  gpr=$(gallery_pr)
fi
check "it proposes a stack.yaml, as alice" test -n "$gpr"
gitea_api "" "$api/repos/photos/gallery/raw/stack.yaml?ref=$(gitea_api "" "$api/repos/photos/gallery/pulls/${gpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)" > "$tmp/gallery.yaml"
check "one the deployer takes"             homecloud validate "$tmp/gallery.yaml"
check "nothing went into system"           test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/system/gallery")" = 404
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$api/repos/photos/gallery/pulls/${gpr:-0}/merge")" = 200
gallery_deployed() { homecloud applied | grep -q "^app photos/gallery@main gallery "; }
check "the deployer deploys it"            until_true 300 gallery_deployed
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent system bob "Make me a namespace bobs for my own apps.")
check "bob cannot make a namespace"        test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/bobs")" = 404
refused_ns() { refused_only "$since" bob create_namespace "only root admins may"; }
check "the gateway refused every try"      refused_ns

while read -r c m; do
  for id in $(web -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$c/messages/$m/thread" | python3 -c 'import json,sys; [print(x["id"]) for x in json.load(sys.stdin)]' 2>/dev/null) $m; do
    web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$c/messages/$id/delete"
  done
done < "$tmp/posted"
}

# Usage: smoke.sh [SECTION...]; system, login and platform by default.
sections=${*:-system login platform}
for s in $sections; do
  t0=$SECONDS
  case $s in system|login|platform|agent|deploy|talk) "section_$s" ;; *) echo "unknown section: $s" >&2; exit 2 ;; esac
  echo "time  section $s took $(( (SECONDS - t0) / 60 ))m$(( (SECONDS - t0) % 60 ))s"
done
# Slow checks: each check's time, longest first.
echo
if [ $failed -eq 0 ]; then echo "all checks passed"; else echo "$failed check(s) failed"; fi
exit $((failed > 0))
