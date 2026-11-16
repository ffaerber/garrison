#!/usr/bin/env bash
# Smoke test for an installed machine. Run on the machine, as root, from the repo root.
set -uo pipefail

. /etc/garrison/install.env
ETC=/etc/garrison
STACKS=test/e2e/stacks
CTL="garrison"
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
  for h in keycloak sso git grafana prometheus loki chat gateway agents hello dev-hello todo dev-todo homepage; do args+=(--resolve "$h.$DOMAIN:443:127.0.0.1"); done
  args+=(--resolve "$DOMAIN:443:127.0.0.1") # the dashboard
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
# The dashboard's feed, at the bare domain. home_login WHO signs a person in
# to it; feed WHO PATH calls its API as them.
feed() { local who=$1; shift; web -b "$tmp/home-$who.jar" "https://$DOMAIN$1"; }
home_login() {
  sso_login "$tmp/home-$1.jar" "https://$DOMAIN/" "$TEST_PASSWORD" "$tmp/home-$1.html" "$1" >/dev/null 2>&1
  [ "$(web -b "$tmp/home-$1.jar" -o /dev/null -w '%{http_code}' "https://$DOMAIN/api/events?limit=1")" = 200 ]
}
# in_feed WHO QUERY CONDITION: the newest 500 events WHO sees, filtered by
# QUERY (e.g. ns=todo), hold one for which the Python CONDITION on e holds.
# Prints its ID.
in_feed() {
  feed "$1" "/api/events?limit=500&$2" | python3 -c '
import json,sys
cond = sys.argv[1]
for e in json.load(sys.stdin):
    for k in ("commit", "detail", "agent", "actor", "started_by", "link", "repo", "namespace"):
        e.setdefault(k, ""); e.setdefault("pr", 0)
    if eval(cond, {}, {"e": e}):
        print(e["id"]); sys.exit(0)
sys.exit(1)' "$3"
}
# not_in_feed WHO QUERY CONDITION: WHO's feed answers, and holds no such event.
not_in_feed() {
  feed "$1" "/api/events?limit=1" | python3 -c 'import json,sys; json.load(sys.stdin)' || return 1
  ! in_feed "$@" >/dev/null
}
# thread_has WHO ID CONDITION: the thread of event ID, as WHO sees it, holds
# an event for which CONDITION holds.
thread_has() {
  feed "$1" "/api/events/$2" | python3 -c '
import json,sys
cond = sys.argv[1]
for e in json.load(sys.stdin)["thread"]:
    e.setdefault("commit", ""); e.setdefault("pr", 0); e.setdefault("actor", "")
    if eval(cond, {}, {"e": e}): sys.exit(0)
sys.exit(1)' "$3"
}

# set_config NS PATH FILE|- [APPROVER]: alice proposes the file PATH of the
# namespace NS's config as FILE, or its removal with -; APPROVER (by default
# the local admin, a root admin) approves it, and alice merges it once the
# approval check passes. Fails if a step does.
set_config() {
  local ns=$1 path=$2 file=$3 approver=${4:-} cfg=$api/repos/$1/config name branch sha body pr st i
  name=$(basename "$path" .yaml)
  branch="config-$name-$(date +%s%N | cut -c1-13)"
  sha=$(gitea_api "" "$cfg/contents/$path" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))' 2>/dev/null)
  body=$(python3 -c '
import base64,json,sys
file,path,branch,sha=sys.argv[1:5]
op={"path":path}
if file=="-": op.update(operation="delete",sha=sha)
else:
    op.update(operation="update" if sha else "create",content=base64.b64encode(open(file,"rb").read()).decode())
    if sha: op["sha"]=sha
print(json.dumps({"branch":"main","new_branch":branch,"message":("Remove " if file=="-" else "Set ")+path,"files":[op]}))' "$file" "$path" "$branch" "$sha") || return 1
  [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$body" "$cfg/contents")" = 201 ] || { echo "cannot write $path"; return 1; }
  pr=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"Set '"$path"'"}' "$cfg/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])') || return 1
  [ "$(gitea_api "$approver" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$cfg/pulls/$pr/reviews")" = 200 ] || { echo "cannot approve #$pr"; return 1; }
  for i in $(seq 60); do
    st=$(gitea_api "" "$cfg/commits/$(gitea_api "" "$cfg/pulls/$pr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"], s["description"]); break' 2>/dev/null)
    case $st in success*) break ;; failure*) echo "approval: $st"; return 1 ;; esac
    sleep 2
  done
  [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$cfg/pulls/$pr/merge")" = 200 ] || { echo "cannot merge #$pr ($st)"; return 1; }
}
# set_stack NS NAME FILE|- [APPROVER]: set_config for config/stacks/NAME.yaml.
set_stack() { set_config "$1" "stacks/$2.yaml" "$3" "${4:-}"; }
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

# The agents, asked in Open WebUI channels as people do. What is posted is
# listed in $tmp/posted, to be deleted at the end of the talk section.
: > "$tmp/posted"
# channel_of NAME [PERSON]: the id of a channel PERSON (bob) sees.
channel_of() { chat_api "${2:-bob}" "$chat/api/v1/channels/" | python3 -c 'import json,sys; print(next(c["id"] for c in json.load(sys.stdin) if c["name"] == sys.argv[1]))' "$1" 2>/dev/null; }
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

# ---------- sections ----------
# system: the stacks the installer deploys are up and safe.
section_system() {
echo "# system stack"
check "ZFS pool is online"                 test "$(zpool list -H -o health garrison)" = ONLINE
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
check "the garrison realm serves OIDC discovery" status_is 200 keycloak /realms/garrison/.well-known/openid-configuration
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
  grep -q "^https://keycloak.$DOMAIN/realms/garrison/protocol/openid-connect/auth" <<<"$(web -o /dev/null -w '%{redirect_url}' "$chat/oauth/oidc/login")"
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
check "and the commander, which finds the agent for a task" has_agent system/commander
check "the agents come from the agent service" grep -q '"owned_by":"garrison"' <<<"$(chat_api bob "$chat/api/models")"
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
  grep -q "^https://keycloak.$DOMAIN/realms/garrison/protocol/openid-connect/auth" <<<"$(web -o /dev/null -w '%{redirect_url}' $g/login/generic_oauth)"
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
check "the deployer no longer runs on the host" bash -c '! systemctl is-active --quiet garrison-controller'
check "the deployer deployed grafana, prometheus and loki" \
  bash -c 'a=$(garrison applied | cut -d" " -f1); for n in grafana prometheus loki; do grep -qx $n <<<"$a" || exit 1; done'

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
check "bob can open the overview"          test "$(grafana_api bob -o /dev/null -w '%{http_code}' "$g/api/dashboards/uid/garrison-overview")" = 200
check "bob cannot change it" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"dashboard":{"uid":"garrison-overview","title":"bob"},"overwrite":true}' "$g/api/dashboards/db")" = 403
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
overview=$(grafana_api "" "$g/api/dashboards/uid/garrison-overview" | python3 -c 'import json,sys; d=json.load(sys.stdin)["dashboard"]; d["title"]="Edited in Grafana"; print(json.dumps({"dashboard": d, "overwrite": True}))')
grafana_api "" -o /dev/null -X POST -H 'Content-Type: application/json' -d "$overview" "$g/api/dashboards/db"
title_back() { grafana_api "" "$g/api/dashboards/uid/garrison-overview" | grep -q '"title":"Overview"'; }
check "an edit made in Grafana is put back" until_true 120 title_back

echo "# deploying a stack"
check "the CLI deploys hello (main)"      $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "hello is reachable at its hostname" status_is 200 hello /
check "hello got its dataset with a quota" test "$(zfs get -H -o value quota garrison/ns/demo/stacks/hello/hello-data)" = 1G
check "hello's dataset is mounted in the container" \
  bash -c 'c=$(docker ps -q --filter label=com.docker.swarm.service.name=hello_web | head -n1); docker inspect "$c" --format "{{range .Mounts}}{{.Source}}:{{.Destination}} {{end}}" | grep -q "/garrison/ns/demo/stacks/hello/hello-data:/data"'
check "hello got a generated secret"       docker secret inspect hello.hello-token
check "the CLI deploys hello (dev)"       $CTL apply -domain "$DOMAIN" -env dev -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "dev has its own dataset"            zfs list garrison/ns/demo/stacks/dev-hello/hello-data
check "redeploying hello is accepted"      $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml

echo "# private environments"
check "a private environment answers visitors with a redirect" status_is 302 dev-hello /
target=$(web -o /dev/null -w '%{redirect_url}' "https://dev-hello.$DOMAIN/")
check "the redirect goes to the Keycloak login" grep -q "^https://keycloak.$DOMAIN/realms/garrison/protocol/openid-connect/auth" <<<"$target"
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

echo "# models"
# The model list in system's config: written from the install's LLM, then
# kept current by the model sync's pull requests. A fake provider, a static
# file served on this machine, stands in for OpenRouter and the like.
sys=$api/repos/system/config
llm_file() { gitea_api "" "$sys/raw/llm/$1?ref=${2:-main}"; }
seeded() { llm_file providers.yaml | grep -q '^default: ' && llm_file models.yaml | grep -q 'enabled: true'; }
check "system's config gets the model list" until_true 180 seeded
install_model=$(sed -n 's/^LLM_MODEL=//p' $ETC/install.env)
check "its default is the install's model, enabled" \
  bash -c 'grep -qF "/$1\"" <<<"$2" && grep -qF "/$1\": {enabled: true" <<<"$3"' _ "$install_model" "$(llm_file providers.yaml)" "$(llm_file models.yaml)"
check "everyone signed in may read it" test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$sys/raw/llm/models.yaml")" = 200
check "the agent service reads keys from a root-only folder" test "$(stat -c %a $ETC/llm-keys)" = 700

fake=$tmp/fake-llm
mkdir -p "$fake/v1"
printf '{"object":"list","data":[{"id":"fake-small"},{"id":"fake-large"},{"id":"other-model"}]}' > "$fake/v1/models"
(cd "$fake" && exec python3 -m http.server 8399 --bind 0.0.0.0 >/dev/null 2>&1) &
fake_pid=$!
fake_url="http://$(hostname -I | awk '{print $1}'):8399/v1"
llm_file providers.yaml > "$tmp/providers.yaml"
printf '  fake:\n    url: "%s"\n    include: ["fake-*"]\n' "$fake_url" >> "$tmp/providers.yaml"
check "alice adds a provider; another root admin approves" set_config system llm/providers.yaml "$tmp/providers.yaml"

models_pr() { gitea_api "" "$sys/pulls?state=open&limit=50" | python3 -c '
import json,sys
print(next((str(p["number"]) for p in json.load(sys.stdin) if p["head"]["ref"].startswith("garrison/models-")),""))'; }
# models_branch: the branch of the model sync's open pull request.
models_branch() { gitea_api "" "$sys/pulls/$(models_pr)" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["ref"])'; }
models_pr_with() { local n; n=$(models_pr); [ -n "$n" ] && gitea_api "" "$sys/pulls/$n" | grep -qF "$1"; }
sys_approval() { gitea_api "" "$sys/commits/$(gitea_api "" "$sys/pulls/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"], s["description"]); break'; }
check "the model sync proposes the provider's models" until_true 180 models_pr_with "fake: 2 new"
pr=$(models_pr)
proposed=$(llm_file models.yaml "$(models_branch)")
check "they come turned off, only those include names" \
  bash -c 'grep -qF "\"fake/fake-small\": {enabled: false" <<<"$0" && grep -qF "\"fake/fake-large\": {enabled: false" <<<"$0" && ! grep -q other-model <<<"$0"' "$proposed"
check "the install's models are kept"      grep -qF "/$install_model\": {enabled: true" <<<"$proposed"
approval_has() { sys_approval "$1" | grep -q "$2"; }
sys_review() { gitea_api "$1" -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$sys/pulls/$2/reviews"; }
check "it waits for a root admin's approval" until_true 60 approval_has "$pr" "^pending"
sys_review bob "$pr"; sleep 25
check "bob's approval does not count"      approval_has "$pr" "^pending"
sys_review alice "$pr"
check "alice's does"                       until_true 60 approval_has "$pr" "^success"
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$sys/pulls/$pr/merge")" = 200
check "main lists fake's models"           grep -qF '"fake/fake-small"' <<<"$(llm_file models.yaml)"

# An agent may name only an enabled model: refused while fake-small is off,
# taken once the same pull request enables it.
agent_pr() {
  local branch=models-agent-$1 ops
  ops=$(python3 -c '
import base64,json,sys
b=lambda s: base64.b64encode(s.encode()).decode()
files=[{"operation":"create","path":"agents/tester/agent.yaml","content":b("description: Tests\nmodel: fake/fake-small\ntools: [list_repos]\n")},
       {"operation":"create","path":"agents/tester/prompt.md","content":b("You test.\n")}]
if sys.argv[2]: files.append({"operation":"update","path":"llm/models.yaml","sha":sys.argv[3],"content":b(sys.argv[2])})
print(json.dumps({"branch":"main","new_branch":sys.argv[1],"message":"An agent on fake","files":files}))' "$branch" "${2:-}" "$(gitea_api "" "$sys/contents/llm/models.yaml" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha"])')")
  gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d "$ops" "$sys/contents"
  gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"An agent on fake"}' "$sys/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])'
}
refused_pr=$(agent_pr off)
check "an agent naming a model that is off is refused" until_true 90 approval_has "$refused_pr" "^failure.*fake/fake-small is not enabled"
enabled_pr=$(agent_pr on "$(llm_file models.yaml | sed 's#"fake/fake-small": {enabled: false#"fake/fake-small": {enabled: true#')")
check "the same, enabling the model, waits only for approval" until_true 90 approval_has "$enabled_pr" "^pending"
for n in "$refused_pr" "$enabled_pr"; do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$sys/pulls/$n"
done

# A model the provider stops offering is marked gone, in the next sync.
printf '{"object":"list","data":[{"id":"fake-small"}]}' > "$fake/v1/models"
sed -i 's#include: \["fake-\*"\]#include: ["fake-small", "fake-large"]#' "$tmp/providers.yaml"
check "alice narrows the provider"         set_config system llm/providers.yaml "$tmp/providers.yaml"
check "the next sync proposes fake-large as retired" until_true 180 models_pr_with "Retired: fake/fake-large"
check "with the date it went"              grep -qE '"fake/fake-large": \{enabled: false, added: [0-9-]+, gone: [0-9-]+\}' <<<"$(llm_file models.yaml "$(models_branch)")"
kill "$fake_pid" 2>/dev/null || true

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
# pipeline on the user runner. Leaves todo/app running.
section_deploy() {
echo "# deploying an app through its pipeline"
repo=$api/repos/todo/app
for u in alice bob; do sso_login "$tmp/git-$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/git-$u.html" $u >/dev/null 2>&1; done
check "bob cannot create a namespace" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"bobs-namespace"}' "$api/orgs")" = 403
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"todo","visibility":"limited","description":"Our todo list"}' "$api/orgs")
check "alice creates the namespace todo" bash -c "[ $code = 201 ] || [ $code = 422 ]"
has_build_account() { gitea_api "" "$api/orgs/todo/actions/secrets" | grep -q '"name":"REGISTRY_TOKEN"'; }
check "the namespace gets its build account" until_true 150 has_build_account
check "its build account is a member"     test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/todo/members/todo-builder")" = 204
check "the todo app's source goes to todo/app" installer/push-repo.sh --update todo/app examples/todo/app
has_config() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/todo/config")" = 200 ]; }
check "todo has its config"            until_true 120 has_config
check "alice adds its stack to todo's config" set_stack todo todo examples/todo/config/stacks/todo.yaml
# The homepage, built meanwhile: one static page, in a namespace of its own,
# open to everyone.
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"homepage","visibility":"limited","description":"My homepage"}' "$api/orgs")
check "alice creates the namespace homepage" bash -c "[ $code = 201 ] || [ $code = 422 ]"
hp_builder() { gitea_api "" "$api/orgs/homepage/actions/secrets" | grep -q '"name":"REGISTRY_TOKEN"'; }
check "it gets its build account"          until_true 150 hp_builder
check "the homepage's source goes to homepage/app" installer/push-repo.sh --update homepage/app examples/homepage/app
hp_config() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/homepage/config")" = 200 ]; }
check "homepage has its config"            until_true 120 hp_config
check "alice adds its stack to homepage's config" set_stack homepage homepage examples/homepage/config/stacks/homepage.yaml
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
check "the image is in todo's registry" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/packages/todo/container/app/${good:0:12}")" = 200
deployed() { garrison applied | grep -q "^app todo/todo@$1 [a-z-]* $2"; }
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
# Bob signs in too: everyone has a list of their own.
bob_api() { web -b "$tmp/todo-bob.jar" -H "Origin: https://todo.$DOMAIN" -H 'Content-Type: application/json' "$@"; }
bob_login() {
  sso_login "$tmp/todo-bob.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo-bob.html" bob >/dev/null 2>&1
  [ "$(bob_api -o /dev/null -w '%{http_code}' "https://todo.$DOMAIN/api/todos")" = 200 ]
}
check "bob signs in to todo too"           until_true 120 bob_login
check "his list is empty: alice's todo is hers" test "$(bob_api "https://todo.$DOMAIN/api/todos")" = "[]"
check "he adds his own"                    grep -q '"created_by":"bob"' <<<"$(bob_api -X POST -d '{"title":"Fix the bike"}' "https://todo.$DOMAIN/api/todos")"
check "alice does not see it"              bash -c '! grep -q "Fix the bike" <<<"$0"' "$(todo_api todo "https://todo.$DOMAIN/api/todos")"
alice_todo=$(todo_api todo "https://todo.$DOMAIN/api/todos" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
bob_api -o /dev/null -X POST "https://todo.$DOMAIN/todos/$alice_todo/delete"
check "he cannot delete hers"              grep -q 'Water the plants' <<<"$(todo_api todo "https://todo.$DOMAIN/api/todos")"
check "its database has its own dataset"   test "$(zfs get -H -o value quota garrison/ns/todo/stacks/todo/todo-db)" = 2G
check "its password is a generated secret" docker secret inspect todo.db-password
chat_admin=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
ns_agent() { web -H "Authorization: Bearer $chat_admin" "$chat/api/models" | grep -q '"id":"todo/operator"'; }
check "todo has its agent and channel in Open WebUI" until_true 120 ns_agent

echo "# the homepage"
hp_deployed() { garrison applied | grep -q "^app homepage/homepage@main homepage "; }
check "the deployer deploys it as homepage" until_true 900 hp_deployed
hello() { web "https://homepage.$DOMAIN/" | grep -q "Hello, world"; }
check "everyone sees it, without signing in" until_true 120 hello

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
check "no image is built for it"           test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/packages/todo/container/app/${bad:0:12}")" = 404
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

echo "# snapshots and restore"
todo_ds=garrison/ns/todo/stacks/todo
snap=$(zfs list -H -t snapshot -o name -s creation -d 1 "$todo_ds" | grep "@deploy-.*-${fixed:0:7}$" | tail -n1 | cut -d@ -f2)
check "the deployer snapshots todo before redeploying it" test -n "$snap"
check "with its database"                  zfs list "$todo_ds/todo-db@$snap"
listed() { grep -q "\"$snap\"" /var/lib/garrison/deployer/snapshots.json; }
check "and lists it for the agents"        until_true 60 listed
check "Sanoid snapshots on a schedule"     systemctl is-active sanoid.timer
# As its timer does every 15 minutes.
systemctl start sanoid.service
autosnap() { zfs list -H -t snapshot -o name "$todo_ds/todo-db" | grep -q '@autosnap_.*_hourly$'; }
check "the apps' data too"                 autosnap
# plants: the ids of the todo "Water the plants"; fails, saying what came, on
# an answer that is not the list.
plants() { todo_api todo "https://todo.$DOMAIN/api/todos" | python3 -c '
import json,sys
raw = sys.stdin.read()
try:
    todos = json.loads(raw)
    assert isinstance(todos, list)
except Exception:
    print("not a list of todos: %r" % raw[:300], file=sys.stderr); sys.exit(1)
print(" ".join(str(t["id"]) for t in todos if t["title"] == "Water the plants"))'; }
# The redeploy above may still be rolling out: the app first answers again.
until_true 180 plants
for id in $(plants); do todo_api todo -o /dev/null -X POST "https://todo.$DOMAIN/todos/$id/delete"; done
no_plants() { local ids; ids=$(plants) && [ -z "$ids" ]; }
check "alice deletes the todo by mistake"  until_true 30 no_plants
printf 'stack: todo\nenvironment: main\nsnapshot: %s\n' "$snap" > "$tmp/restore.yaml"
check "she asks to restore todo's data, a root admin approves" set_config todo restores/todo.yaml "$tmp/restore.yaml"
restored() { [ "$(zfs get -H -o value garrison:restored "$todo_ds")" != - ]; }
check "the deployer restores it"           until_true 300 restored
has_plants() { [ -n "$(plants)" ]; }
check "the todo is back"                   until_true 180 has_plants
before() { zfs list -H -t snapshot -o name -d 1 "$todo_ds" | grep -q '@restore-'; }
check "the state before is kept, to undo it" before

echo "# the user runner's limits"
gitea_api "" -o /dev/null -X DELETE "$api/repos/todo/escape"
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"name":"escape","auto_init":true,"default_branch":"main"}' "$api/orgs/todo/repos"
escape=$(printf 'on: push\njobs:\n  out:\n    runs-on: garrison-user\n    container:\n      image: alpine:3.22\n      options: --pid=host --cap-add SYS_PTRACE --security-opt apparmor=unconfined\n    steps:\n      - run: cat /proc/1/cmdline\n' | base64 -w0)
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"message":"Try to get out","branch":"main","content":"'$escape'"}' "$api/repos/todo/escape/contents/.gitea/workflows/out.yaml"
escape_failed() { gitea_api "" "$api/repos/todo/escape/actions/tasks?limit=5" | grep -q '"status":"failure"'; }
check "a job asking for the host's processes fails" until_true 300 escape_failed
check "the guard refused it"               bash -c 'docker service logs --since 10m control-plane_guard 2>&1 | grep -q "refused.*pid namespace host"'
gitea_api "" -o /dev/null -X DELETE "$api/repos/todo/escape"

echo "# hostnames"
printf 'entrypoint: web\nservices:\n  web: { image: traefik/whoami:v1.11, port: 80 }\nenvironments:\n  main: {}\n' > "$tmp/grafana.yaml"
check "a stack named grafana passes review" set_stack todo grafana "$tmp/grafana.yaml"
refused() { docker service logs --since 5m control-plane_deployer 2>&1 | grep -q "todo/grafana: not deployed: hostname \"grafana\" is reserved"; }
check "but cannot take the reserved hostname" until_true 120 refused
check "it is not deployed"                 bash -c '! garrison applied | grep -q "app todo/grafana"'
check "alice removes it"                   set_stack todo grafana -

echo "# taking an environment down"
check "alice deletes the branch dev"       test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$repo/branches/dev")" = 204
gone() { ! docker stack ls --format '{{.Name}}' | grep -qx dev-todo; }
check "the deployer takes dev-todo down" until_true 120 gone
check "its data is kept"                   zfs list garrison/ns/todo/stacks/dev-todo/todo-db
# main may have been redeployed just before; Traefik follows within seconds.
check "todo keeps running"                 until_true 30 up todo

echo "# the dashboard's feed"
apex() { [ "$(web -o /dev/null -w '%{http_code}' "https://$DOMAIN/")" = 302 ]; }
check "the dashboard is behind the sign-in" until_true 120 apex
check "alice opens it"                     until_true 120 home_login alice
check "it shows the push of the fix"       until_true 60 in_feed alice ns=todo "e['kind'] == 'push' and e['commit'] == '$fixed' and e['actor'] == 'alice'"
check "its pipeline"                       until_true 60 in_feed alice ns=todo "e['kind'] == 'pipeline' and e['commit'] == '$fixed' and e['status'] == 'ok'"
check "and its deploy"                     in_feed alice ns=todo "e['kind'] == 'deploy' and e['commit'] == '$fixed' and e['status'] == 'ok'"
deploy_id=$(in_feed alice ns=todo "e['kind'] == 'deploy' and e['commit'] == '$fixed'")
deploy_thread() { thread_has alice "$deploy_id" "e['kind'] == 'push'" && thread_has alice "$deploy_id" "e['kind'] == 'pipeline'"; }
check "the deploy's thread holds its push and pipeline" deploy_thread
check "the failed pipeline is a problem"   in_feed alice view=problems "e['kind'] == 'pipeline' and e['commit'] == '$bad'"
check "and the commit waited for its image" in_feed alice ns=todo "e['kind'] == 'deploy' and e['commit'] == '$bad' and e['status'] == 'waiting'"
check "the restore"                        in_feed alice ns=todo "e['kind'] == 'restore' and e['status'] == 'ok'"
check "the reserved hostname"              in_feed alice view=problems "'reserved' in e['title'] and e['namespace'] == 'todo'"
check "dev-todo taken down"                in_feed alice ns=todo "e['kind'] == 'undeploy' and 'dev-todo' in e['title']"
check "the homepage's deploy"              in_feed alice ns=homepage "e['kind'] == 'deploy' and e['status'] == 'ok'"
check "the system stacks' deploys, for everyone" in_feed alice "" "e['kind'] == 'deploy' and e['namespace'] == '' and 'traefik' in e['title']"
page_lists() { local page; page=$(feed alice "/?ns=todo") && grep -q "deployed todo/todo@main at ${fixed:0:7}" <<<"$page"; }
check "its page lists them"                page_lists
check "bob opens it too"                   until_true 60 home_login bob
check "he sees todo's deploys, as he may read its repos" in_feed bob ns=todo "e['kind'] == 'deploy' and e['commit'] == '$fixed'"

echo "# secrets"
# set_secret WHO NS NAME VALUE [ORIGIN]: the dashboard's form; prints the status.
set_secret() {
  web -b "$tmp/home-$1.jar" -o /dev/null -w '%{http_code}' -H "Origin: ${5:-https://$DOMAIN}" \
    --data-urlencode action=set --data-urlencode "name=$3" --data-urlencode "value=$4" "https://$DOMAIN/secrets/$2"
}
token=$(openssl rand -hex 16)
check "alice sets a secret of todo in the dashboard" test "$(set_secret alice todo smoke-token "$token")" = 303
check "bob may not: he is not in todo"     test "$(set_secret bob todo smoke-token stolen)" = 403
check "an app's page cannot post the form" test "$(set_secret alice todo smoke-token forged "https://todo.$DOMAIN")" = 403
stored() { gitea_api "" -f "$api/repos/todo/config/raw/secrets/smoke-token.yaml"; }
encrypted() { local f; f=$(stored) && grep -q 'ENC\[AES256_GCM' <<<"$f" && ! grep -q "$token" <<<"$f" && [ "$(grep -c 'recipient: age1' <<<"$f")" = 2 ]; }
check "it is in todo's config, encrypted for todo's key and the recovery key" encrypted
check "with .sops.yaml beside it"          bash -c 'grep -q "age: age1" <<<"$0"' "$(gitea_api "" "$api/repos/todo/config/raw/.sops.yaml")"
author() { gitea_api "" "$api/repos/todo/config/commits?path=secrets/smoke-token.yaml&limit=1" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["commit"]["author"]["name"])'; }
check "committed with alice as its author" test "$(author)" = alice
listed() { local p; p=$(feed alice "/secrets?ns=todo") && grep -q smoke-token <<<"$p" && ! grep -q "$token" <<<"$p"; }
check "the page lists it, never its value" listed
check "the feed says alice set it"         until_true 30 in_feed alice ns=todo "e['kind'] == 'secret' and e['actor'] == 'alice' and 'smoke-token' in e['title']"
check "only the deployer holds todo's key" bash -c 'test -s /etc/garrison/sops/todo.txt && [ "$(stat -c %a /etc/garrison/sops)" = 700 ] && ! test -e /etc/garrison/sops/recovery-key.txt'
# Postgres takes its password from the stored secret.
printf 'entrypoint: db\nservices:\n  db:\n    image: postgres:17-alpine\n    port: 5432\n    secrets: [smoke-token]\n    env: { POSTGRES_PASSWORD_FILE: /run/secrets/smoke-token }\nenvironments:\n  main: {}\n' > "$tmp/keyed.yaml"
check "alice adds an app that uses it"     set_stack todo keyed "$tmp/keyed.yaml"
in_app() { local c; c=$(docker ps -q -f name=keyed_db | head -n1) && [ -n "$c" ] && [ "$(docker exec "$c" cat /run/secrets/smoke-token)" = "$1" ]; }
check "the app gets the value"             until_true 240 in_app "$token"
token2=$(openssl rand -hex 16)
check "alice replaces it"                  test "$(set_secret alice todo smoke-token "$token2")" = 303
check "the app is redeployed with the new value" until_true 240 in_app "$token2"
check "alice removes the app"              set_stack todo keyed -
deleted() { [ "$(web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code}' -H "Origin: https://$DOMAIN" --data-urlencode action=delete --data-urlencode name=smoke-token "https://$DOMAIN/secrets/todo")" = 303 ] && ! stored >/dev/null 2>&1; }
check "and deletes the secret"             deleted

echo "# alerts"
health() { docker exec "$(docker ps -q -f name=prometheus_prometheus | head -n1)" wget -qO- "http://control-plane_deployer:8090/metrics"; }
check "the deployer tells Prometheus how each service does" grep -q 'garrison_service_replicas_running{namespace="todo",app="todo",environment="main",stack="todo",service="todo_web"} 1' <<<"$(health)"
# Postgres without a password exits at once: a service that keeps failing.
printf 'entrypoint: db\nservices:\n  db: { image: postgres:17-alpine, port: 5432 }\nenvironments:\n  main: {}\n' > "$tmp/crash.yaml"
check "alice adds an app that cannot start" set_stack todo crash "$tmp/crash.yaml"
crash_alert() { docker exec "$(docker ps -q -f name=prometheus_alertmanager | head -n1)" wget -qO- http://localhost:9093/api/v2/alerts | python3 -c '
import json,sys
sys.exit(0 if any(a["labels"].get("alertname") == "ServiceFailing" and a["labels"].get("service") == "crash_db" for a in json.load(sys.stdin)) else 1)'; }
check "it raises an alert"                 until_true 400 crash_alert
check "in the feed, for todo's people"     until_true 120 in_feed alice ns=todo "e['kind'] == 'alert' and e['status'] == 'failed' and 'crash_db' in e['title'] and e['source'] == 'alerts'"
check "not for bob, who is not in todo"    not_in_feed bob ns=todo "e['kind'] == 'alert' and 'crash_db' in e['title']"
check "alice removes it"                   set_stack todo crash -
check "the alert resolves"                 until_true 300 in_feed alice ns=todo "e['kind'] == 'alert' and e['status'] == 'ok' and 'crash_db' in e['title']"
}

# talk: the scenarios of docs/scenarios.md, against the real LLM, in the
# channel of todo. Checks what happened, not what the agent said.
section_talk() {
echo "# talking to the agent of todo"
if [ ! -s $ETC/llm-api-key ]; then
  echo "skip  no LLM key in $ETC/llm-api-key"
  return
fi
hp=$api/repos/todo
for u in alice bob carol; do
  sso_login "$tmp/git-$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/git-$u.html" $u >/dev/null 2>&1
  check "$u signs in to Open WebUI" chat_login $u
done
admin_token=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" \
  "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
# newest_pr REPO AUTHOR [AFTER]: the newest open pull request of todo/REPO by AUTHOR, numbered above AFTER.
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
# roster REF: members.yaml of todo/config at REF, as "admins=a,b members=c".
roster() {
  gitea_api "" "$hp/config/raw/members.yaml?ref=$1" | python3 -c '
import sys,yaml
d=yaml.safe_load(sys.stdin) or {}
print("admins=" + ",".join(sorted(d.get("admins") or [])) + " members=" + ",".join(sorted(d.get("members") or [])))'
}
approval() { gitea_api "" "$hp/config/commits/$(head_sha config "$1")/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"], s["description"]); break'; }
approval_is() { approval "$1" | grep -q "^$2"; }
review() { gitea_api "$2" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$hp/$1/pulls/$3/reviews"; }
merge() { gitea_api "$2" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$hp/$1/pulls/$3/merge"; }
team_has() {
  local id
  id=$(gitea_api "" "$api/orgs/todo/teams" | python3 -c 'import json,sys; print(next(t["id"] for t in json.load(sys.stdin) if t["name"]==sys.argv[1]))' "$1")
  gitea_api "" "$api/teams/$id/members" | grep -q "\"login\":\"$2\""
}
# add_agent BRANCH NAME TOOLS DESCRIPTION PROMPT [SKILLS] [SKILL_FILE...]:
# alice proposes agents/NAME in todo/config, with the shared skills
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
# png_size REPO PATH REF: WIDTHxHEIGHT of a PNG in todo/REPO at REF, or none.
png_size() {
  gitea_api "" "$hp/$1/raw/$2?ref=$3" | python3 -c '
import struct,sys
d=sys.stdin.buffer.read()[:24]  # all of it: closing early fails the download
print("%dx%d" % struct.unpack(">II", d[16:24]) if d[1:4] == b"PNG" else "none")'
}
# pr_with REPO PERSON AFTER PATH: the open pull request of todo/REPO by
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
can_push() { gitea_api "$1" "$hp/app" | grep -q '"push":true'; }
cannot_push() { ! can_push "$1"; }

check "todo has its config, alice its first admin" test "$(roster main)" = "admins=alice members="
not_seen() { ! has_model "$1" "$2"; }
check "bob may not push to todo's repos yet" cannot_push bob
nopush=$(printf 'admins: [alice, bob]\nmembers: []\n' | base64 -w0)
check "nobody changes members.yaml without a pull request" \
  bash -c '[ "$0" != 200 ] && [ "$0" != 201 ]' "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d '{"message":"Shortcut","branch":"main","content":"'$nopush'","sha":"'"$(gitea_api "" "$hp/config/contents/members.yaml" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)"'"}' "$hp/config/contents/members.yaml")"

echo "# 1. alice adds carol as admin and bob as member"
since=$(last_pr config)
answer=$(ask_agent todo alice "Add carol as an admin and bob as a member of the namespace todo.")
pr=$(newest_pr config alice "$since")
if [ -z "$pr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo alice "Please propose the change to members.yaml in todo/config now: carol under admins, bob under members, alice stays admin.")
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
check "bob may now push to todo's repos" until_true 60 can_push bob

echo "# 2. bob tries to make himself an admin"
since=$(last_pr config)
answer=$(ask_agent todo bob "Make me, bob, an admin of the namespace todo.")
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
answer=$(ask_agent todo bob "Approve and merge the pull request in todo/config that makes me an admin.")
answer=$(ask_agent todo bob "Add me, bob, to the root admins, the Keycloak group root-admins.")
check "bob is still only a member"         grep -q "^admins=alice,carol members=bob$" <<<"$(roster main)"
not_admin() { ! team_has admins "$1"; }
check "bob is not an admin of todo"    not_admin bob
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
since=$(last_pr app)
answer=$(ask_agent todo bob "In the repo app of the namespace todo, change the heading of page.html from Todo to Our todos, in both the <title> and the <h1>. Read the file first and propose the whole new file.")
tpr=$(newest_pr app bob "$since")
if [ -z "$tpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please propose that change to page.html in todo/app now.")
  tpr=$(newest_pr app bob "$since")
fi
check "the agent proposes it, as bob"      test -n "$tpr"
check "it changes the heading"             grep -q '^+.*Our todos' <<<"$(gitea_api "" "$hp/app/pulls/$tpr.diff")"
# With three people in todo, main of its app repos takes only pull requests
# another of them approved.
check "carol cannot merge it unapproved"   test "$(merge app carol "$tpr")" != 200
check "carol approves it"                  test "$(review app carol "$tpr")" = 200
check "and merges it"                      test "$(merge app carol "$tpr")" = 200
merged=$(gitea_api "" "$hp/app/pulls/$tpr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["merge_commit_sha"])')
deployed_at() { garrison applied | grep -q "^app todo/todo@main todo $1"; }
check "its pipeline builds it and the deployer deploys it" until_true 1200 deployed_at "$merged"
sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
# Signs in again if the page is not there: after a snapshot boot the login
# proxy may still have been waiting for Keycloak at the first try.
heading() {
  web -b "$tmp/todo.jar" "https://todo.$DOMAIN/" | grep -q "Our todos" && return 0
  sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
  return 1
}
check "the todo page shows the new heading" until_true 120 heading
check "carol opens the dashboard"          until_true 120 home_login carol
change=$(in_feed carol ns=todo "e['agent'] == 'todo/operator' and e['actor'] == 'bob' and e['pr'] == $tpr and e['status'] == 'ok'")
check "its feed has bob's change, through the operator" test -n "$change"
handled() { thread_has carol "$change" "e['kind'] == 'pr-merged' and e['actor'] == 'carol'" && thread_has carol "$change" "e['kind'] == 'deploy' and e['commit'] == '$merged'"; }
check "how it was handled: carol merged it, the deployer deployed it" until_true 60 handled

echo "# 4b. bob asks the media agent for a screenshot"
# The media agent comes with a shared skill for homepage screenshots.
cat > "$tmp/skill.md" <<'SKILL'
---
name: homepage-screenshots
description: Screenshots of an app for the homepage, one for desktop and one for phones. Use it whenever someone asks for homepage screenshots of an app.
---
For the app asked about, take two screenshots of https://<app>.<domain>/ with the screenshot tool, into the repo with the app's code (ask if you do not know it):

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
check "Open WebUI has todo/media"      until_true 120 has_model todo/media
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
media_from=$at  # the channel's conversation with the media agent begins
answer=$(ask_agent todo bob "Take one screenshot of https://todo.$DOMAIN/ and put it into the repo app as docs/screenshots/todo.png. Title: Screenshot of the todo app. Just this one, not the homepage screenshots of your skill." media)
spr=$(pr_with app bob "$since" docs/screenshots/todo.png | cut -d' ' -f1)
if [ -z "$spr" ]; then
  printf 'retry the agent opened no pull request with todo.png; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please take that one screenshot now with the screenshot tool: url https://todo.$DOMAIN/, namespace todo, repo app, path docs/screenshots/todo.png." media)
  spr=$(pr_with app bob "$since" docs/screenshots/todo.png | cut -d' ' -f1)
fi
check "the media agent takes it, as bob"   grep -q "mcp: bob: screenshot" <<<"$(mcp_calls "$at" bob)"
check "it proposes the screenshot, as bob" test -n "$spr"
check "the pull request holds a PNG"       test "$(png_size app docs/screenshots/todo.png "$(head_sha app "$spr")")" != none
body=$(gitea_api "" "$hp/app/pulls/$spr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["body"])')
check "it shows the todo app, signed in as bob" \
  bash -c 'grep -q "Screenshot of https://todo.$1/ as bob" <<<"$0" && grep -q "Our todos" <<<"$0"' "$body" "$DOMAIN"
check "the answer links it"                grep -q "/todo/app/pulls/$spr" <<<"$answer"
# Close it, and whatever else it proposed (a model may follow its skill
# unasked), so 4c starts afresh.
for n in $(gitea_api "" "$hp/app/pulls?state=open&limit=50" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="bob" and p["number"] > int(sys.argv[1])]' "$since"); do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$n"
done

echo "# 4c. the media agent follows its skill"
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Make the homepage screenshots of the todo app, new ones: earlier ones were closed. Its code is in the repo app." media)
if ! pr_with app bob "$since" docs/screenshots/todo-phone.png >/dev/null || ! pr_with app bob "$since" docs/screenshots/todo-desktop.png >/dev/null; then
  printf 'retry the agent did not make both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please load your skill homepage-screenshots and follow it for the app todo now, into the repo app: both screenshots." media)
fi
# Its sandbox keeps the conversation: a skill loaded for 4b is still known.
check "it has loaded the skill, as bob"    grep -q "mcp: bob: load_skill" <<<"$(mcp_calls "$media_from" bob)"
read -r dpr dsha <<<"$(pr_with app bob "$since" docs/screenshots/todo-desktop.png)"
read -r ppr psha <<<"$(pr_with app bob "$since" docs/screenshots/todo-phone.png)"
check "the desktop screenshot is 1440x900" test "$(png_size app docs/screenshots/todo-desktop.png "$dsha")" = 1440x900
check "the phone screenshot is 390x844"    test "$(png_size app docs/screenshots/todo-phone.png "$psha")" = 390x844
for n in $dpr $ppr; do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$n"
done

echo "# 4d. what the media agent may not do"
platform_prs() { gitea_api "" "$api/repos/system/platform/pulls?state=all&limit=1" | python3 -c 'import json,sys; l=json.load(sys.stdin); print(l[0]["number"] if l else 0)'; }
before=$(platform_prs)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Take a screenshot of https://todo.$DOMAIN/ and put it into the namespace system, repo platform, as docs/todo.png." media)
check "bob gets no screenshot into a repo he may not change" test "$(platform_prs)" = "$before"
check "the gateway refused every try"      refused_only "$at" bob screenshot "not allowed"
check "the agent says so"                  test -n "$answer"
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Take a screenshot of https://example.org/ and put it into the repo app as docs/screenshots/example.png." media)
check "a page outside the home cloud is not shot" test -z "$(newest_pr app bob "$since")"
check "the gateway refused every try"      refused_only "$at" bob screenshot "only pages at"
check "the feed shows bob the refusals"    until_true 60 home_login bob
check "among the problems"                 in_feed bob view=problems "e['kind'] == 'screenshot' and e['status'] == 'refused' and e['agent'] == 'todo/media' and e['actor'] == 'bob'"

echo "# 4e. the media agent works in a sandbox of its own"
# sandbox AGENT: the running sandbox of AGENT in todo's channel.
sandbox_of() { docker ps -q --filter label=garrison.sandbox=true --filter "label=garrison.agent=$1" --filter "label=garrison.chat=channel:$(channel_of todo)" | head -n1; }
sb=$(sandbox_of todo/media)
check "the channel's turns with it ran in its sandbox" test -n "$sb"
check "the sandbox holds no LLM key and no token" \
  bash -c 'k=$(cat "$1"); { [ ${#k} -lt 16 ] || ! docker inspect "$0" | grep -qF -- "$k"; } && [ "$(docker exec "$0" env | grep -c "^GARRISON_")" = 4 ] && ! docker exec "$0" ls /etc/garrison 2>/dev/null' "$sb" "$ETC/llm-api-key"
check "it runs under gVisor"               test "$(docker inspect -f '{{.HostConfig.Runtime}}' "$sb")" = runsc
check "as root, in gVisor's own kernel"    bash -c '[ "$(docker exec "$0" id -u)" = 0 ] && docker exec "$0" dmesg | grep -q gVisor' "$sb"
check "it cannot reach the internet"       bash -c '! docker exec "$0" curl -sS -m 5 -o /dev/null https://example.org' "$sb"
check "not even past the proxy"            bash -c '! docker exec "$0" curl -sS -m 5 -o /dev/null --noproxy "*" https://1.1.1.1' "$sb"
# Through the proxy, which refuses them, and around it; -f: a refusal is an
# answer too.
nothing_at() { ! docker exec "$sb" curl -fsS -m 5 -o /dev/null "$1" && ! docker exec "$sb" curl -fsS -m 5 -o /dev/null --noproxy "*" "$1"; }
check "nor Gitea, nor Docker"              bash -c "$(declare -f nothing_at); sb=$sb; nothing_at http://gitea_gitea:3000 && nothing_at http://control-plane_sandbox-guard:2375/_ping"
check "between turns its token counts for nothing" \
  test "$(docker exec "$sb" sh -c 'curl -sS -m 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $GARRISON_TOKEN" "$GARRISON_API/tools"')" = 401
has_notes() { docker exec "$(sandbox_of todo/media)" grep -q "pelican 42" /data/work/notes.txt; }
answer=$(ask_agent todo bob "Please keep a note for our next screenshots in your workspace: use bash to write the line 'pelican 42' (the name of the next release) into notes.txt, then say done." media)
if ! has_notes; then
  printf 'retry the agent wrote no notes.txt; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "That note is just for you and me: please run echo 'pelican 42' > notes.txt in your workspace now." media)
fi
check "it writes a file in its workspace"  has_notes
# Its container goes, as when it is idle; the workspace and the conversation stay.
docker rm -f "$sb" >/dev/null
answer=$(ask_agent todo bob "Which line did you write into notes.txt just before? Check your workspace." media)
check "a new container resumes the chat"   test -n "$(sandbox_of todo/media)" -a "$(sandbox_of todo/media)" != "$sb"
check "it still has the file"              grep -q "pelican 42" <<<"$answer"
# Grafana's Agents dashboard: the sandbox's metrics carry its agent and
# person; its output, the conversation, stays out of Loki.
prom_value() { grafana_api "" "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$1")" | python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)'; }
media_measured() { [ "$(prom_value 'count(container_memory_working_set_bytes{container_label_garrison_sandbox="true", container_label_garrison_agent="todo/media", container_label_garrison_person="bob"})')" != 0 ]; }
check "Prometheus measures the sandbox, by agent and person" until_true 120 media_measured
sandbox_logs() { grafana_api "" "$g/api/datasources/proxy/uid/loki/loki/api/v1/query_range?limit=5&since=2h&query=$(python3 -c 'import urllib.parse; print(urllib.parse.quote("{container=~\"garrison-agent-.*\"}"))')"; }
check "Loki holds nothing a sandbox printed" bash -c 'grep -q "\"result\":\[\]" <<<"$0"' "$(sandbox_logs)"

echo "# 4f. bob has the developer agent change the app with Git"
dpr=$(add_agent developer developer 'list_repos, read_file, propose_patch' "Changes the code of the namespace's apps" \
  "You change the code of the namespace's apps. Work in a clone of the repo in your workspace, check what you changed with git diff, and propose it as one pull request.")
check "carol approves the developer agent" test "$(review config carol "$dpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$dpr" success
check "alice merges it"                    test "$(merge config alice "$dpr")" = 200
check "Open WebUI has todo/developer"  until_true 120 has_model todo/developer
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Clone the repo app of the namespace todo into your workspace. Find every place the text 'Our todos' appears (grep -rn) and change it to 'Team todos'. Then propose your changes, titled: Rename the heading to Team todos." developer)
gpr=$(newest_pr app bob "$since")
if [ -z "$gpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please propose the changes in your checkout of app now with propose_workspace_changes." developer)
  gpr=$(newest_pr app bob "$since")
fi
check "it clones todo/app, as bob"             bash -c 'docker service logs --since "$0" control-plane_agents 2>&1 | grep -q "git: bob: todo/developer: fetch todo/app"' "$at"
check "it proposes a patch, as bob"        grep -q "mcp: bob: propose_patch" <<<"$(mcp_calls "$at" bob)"
check "a pull request by bob holds it"     test -n "$gpr"
diff=$(gitea_api "" "$hp/app/pulls/$gpr.diff")
check "it renames the heading"             bash -c 'grep -q "^+.*Team todos" <<<"$0" && grep -q "^-.*Our todos" <<<"$0" && ! grep -q "^+.*Our todos" <<<"$0"' "$diff"
check "on a branch of its own"             grep -q '"ref":"change/bob-' <<<"$(gitea_api "" "$hp/app/pulls/$gpr")"
pr_files() { gitea_api "" "$hp/app/pulls/$gpr/files" | python3 -c 'import json,sys; print(" ".join(f["filename"] for f in json.load(sys.stdin)))'; }
answer=$(ask_agent todo bob "Also add a file CHANGELOG.md to your checkout of app, with the line: Renamed the heading to Team todos. Add it to the same pull request." developer)
if ! grep -qw CHANGELOG.md <<<"$(pr_files)"; then
  printf 'retry the pull request has no CHANGELOG.md; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please call propose_workspace_changes on your checkout of app now, so CHANGELOG.md goes into the same pull request." developer)
fi
check "the follow-up goes into the same pull request" grep -qw CHANGELOG.md <<<"$(pr_files)"
commits=$(gitea_api "" "$hp/app/pulls/$gpr/commits" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
check "as another commit, with no new pull request" test "$commits" -ge 2 -a -z "$(newest_pr app bob "$gpr")"
sb=$(sandbox_of todo/developer)
check "the sandbox cannot push"            test "$(docker exec "$sb" sh -c 'curl -sS -m 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $GARRISON_TOKEN" "$GARRISON_API/git/todo/app.git/info/refs?service=git-receive-pack"')" = 403
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$gpr"

echo "# 4g. agents get rights of their own"
# set_file REPO BRANCH PATH FILE MESSAGE: alice writes FILE to PATH on BRANCH
# of todo/REPO, directly (she may, on every branch but a protected main).
set_file() {
  local sha body
  sha=$(gitea_api "" "$hp/$1/contents/$3?ref=$2" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))' 2>/dev/null)
  body=$(python3 -c 'import base64,json,sys; d={"branch":sys.argv[1],"message":sys.argv[3],"content":base64.b64encode(open(sys.argv[2],"rb").read()).decode()}; d.update({"sha":sys.argv[4]} if sys.argv[4] else {}); print(json.dumps(d))' "$2" "$4" "$5" "$sha")
  gitea_api alice -o /dev/null -w '%{http_code}' -X "$([ -n "$sha" ] && echo PUT || echo POST)" -H 'Content-Type: application/json' -d "$body" "$hp/$1/contents/$3"
}
# to_main REPO PATH FILE MESSAGE: FILE lands on main of todo/REPO as main
# takes it: alice proposes it, carol approves, alice merges.
to_main() {
  local branch=land-$(date +%s%N | cut -c1-13) n
  [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"'"$branch"'","old_branch_name":"main"}' "$hp/$1/branches")" = 201 ] || return 1
  case $(set_file "$1" "$branch" "$2" "$3" "$4") in 200|201) ;; *) return 1 ;; esac
  n=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"'"$4"'"}' "$hp/$1/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])') || return 1
  [ "$(review "$1" carol "$n")" = 200 ] && [ "$(merge "$1" alice "$n")" = 200 ]
}
# propose_config BRANCH TITLE PATH=FILE...: alice proposes files in todo/config; prints the pull request's number.
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
agents: [todo/tester]
grants:
  - {repo: app, branch: dev, level: autonomous}
routines:
  - name: main-moved
    on_push: {repo: app, branch: main}
    owner: bob
    task: Call read_file on stacks/todo.yaml of the repo config of the namespace todo, then say in one sentence which environments it has. Change nothing.
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
  - {repo: app, level: propose, for: everyone}
YAML
printf 'You take requests for small changes to the namespace'"'"'s apps from anyone, and propose them as pull requests for the namespace'"'"'s people to review.
' > "$tmp/desk.md"
gpr=$(propose_config grants "Give the developer grants; add the tester and the desk"   "agents/developer/agent.yaml=$tmp/developer.yaml" "agents/tester/agent.yaml=$tmp/tester.yaml" "agents/tester/prompt.md=$tmp/tester.md"   "agents/desk/agent.yaml=$tmp/desk.yaml" "agents/desk/prompt.md=$tmp/desk.md")
check "carol approves the grants"          test "$(review config carol "$gpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$gpr" success
check "alice merges them"                  test "$(merge config alice "$gpr")" = 200
has_account() { gitea_api "" "$api/users/$1" | grep -q '"full_name":"Agent todo/'; }
check "the developer gets an account"      until_true 120 has_account agent-todo-developer
collaborates() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/$2/collaborators/$1")" = 204 ]; }
check "it may write to app"               until_true 60 collaborates agent-todo-developer app
check "and to nothing else"                test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/config/collaborators/agent-todo-developer")" = 404
check "the tester, without grants, gets none" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/users/agent-todo-tester")" = 404
check "Open WebUI has todo/tester"     until_true 120 has_model todo/tester
routines_seen=$SECONDS

echo "# 4h. bob has the developer change dev and merge it there"
check "alice makes the branch dev"         test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"dev","old_branch_name":"main"}' "$hp/app/branches")" = 201
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Clone the repo app of the namespace todo, check out its branch dev, change the text 'Our todos' to 'Dev todos' everywhere (grep -rn), and propose it with propose_workspace_changes with base dev, titled: Dev heading. Then merge that pull request into dev with merge_change, and give me the link." developer)
dvpr=$(newest_any_pr app bob "$since")
if [ -z "$dvpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please propose your checkout of app now with propose_workspace_changes, base dev, then merge it into dev with merge_change." developer)
  dvpr=$(newest_any_pr app bob "$since")
fi
merged_by() { gitea_api "" "$hp/app/pulls/$1" | python3 -c 'import json,sys; p=json.load(sys.stdin); print((p.get("merged_by") or {}).get("login","") if p.get("merged") else "")'; }
if [ -n "$dvpr" ] && [ -z "$(merged_by "$dvpr")" ]; then
  printf 'retry the agent did not merge; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please merge pull request $dvpr of todo/app into dev now, with merge_change." developer)
fi
check "it proposes it for dev, as bob"     grep -q '"ref":"dev"' <<<"$(gitea_api "" "$hp/app/pulls/${dvpr:-0}")"
check "it merges it with its own account"  test "$(merged_by "${dvpr:-0}")" = agent-todo-developer
check "the gateway says it merged for bob" bash -c 'docker service logs --since "$0" control-plane_gateway 2>&1 | grep -q "mcp: bob: todo/developer: merge_change \[person\]$"' "$at"
dev_heading() { web -b "$tmp/dev.jar" "https://dev-todo.$DOMAIN/" | grep -q "Dev todos" && return 0; sso_login "$tmp/dev.jar" "https://dev-todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/dev.html" bob >/dev/null 2>&1; return 1; }
check "dev-todo deploys it"                until_true 1200 dev_heading
main_untouched() { ! gitea_api "" "$hp/app/raw/page.html?ref=main" | grep -q "Dev todos"; }
check "main is untouched"                  main_untouched

echo "# 4i. previews, and the tester tries one for the developer"
printf '%s\n  preview: { private: true }\n' "$(gitea_api "" "$hp/config/raw/stacks/todo.yaml?ref=main")" > "$tmp/todo-stack.yaml"
check "alice gives todo previews, carol approves" set_stack todo todo "$tmp/todo-stack.yaml" carol
# Waits until the routine has seen main, so this push is news to it.
while [ $((SECONDS - routines_seen)) -lt 90 ]; do sleep 10; done
mainmoved=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf 'Notes.\n' > "$tmp/notes.md"
check "alice cannot push to app's main directly" bash -c '[ "$0" != 201 ] && [ "$0" != 200 ]' "$(set_file app main NOTES.md "$tmp/notes.md" "Notes")"
check "her change lands there once carol approves it" to_main app NOTES.md "$tmp/notes.md" "Notes"
since=$(last_pr app)
answer=$(ask_agent todo bob "Clone the repo app of the namespace todo, add a file PREVIEW.md with the line: Preview test. Propose it with propose_workspace_changes, titled: Preview test. Do not merge it." developer)
ppr=$(newest_pr app bob "$since")
check "it proposes it"                     test -n "$ppr"
previewed() { garrison applied | grep -q "^app todo/todo@pr-$ppr pr-$ppr-todo "; }
check "the pull request gets a preview"    until_true 1200 previewed
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Ask todo/tester to open https://pr-$ppr-todo.$DOMAIN/ in the browser and tell you the page's main heading. Tell me what it says." developer)
check "the tester browses it, for bob, asked by the developer" \
  bash -c 'docker service logs --since "$0" control-plane_gateway 2>&1 | grep -q "mcp: bob: todo/tester: browse \[agent todo/developer\]$"' "$at"
check "the answer names the heading"       grep -q "Our todos" <<<"$answer"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$ppr"
unpreviewed() { ! garrison applied | grep -q "^app todo/todo@pr-$ppr "; }
check "closing it takes the preview down"  until_true 300 unpreviewed

echo "# 4j. a routine runs when main moves, with nobody watching"
hcid=$(channel_of todo)
routine_post() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**todo/developer**") and "Routine main-moved, for bob" in c:
        print(m["id"]); sys.exit(0)
sys.exit(1)'
}
check "its answer is posted in the channel" until_true 600 routine_post
check "it read as its own account, for bob" bash -c 'docker service logs --since "$0" control-plane_gateway 2>&1 | grep -q "mcp: bob (nobody watching, as agent-todo-developer): todo/developer: [a-z_]* \[routine todo/developer/main-moved\]"' "$mainmoved"
head_kept() { [ "$(docker exec "$(docker ps -q -f name=postgres_postgres | head -n1)" psql -U postgres -d garrison -tAc "SELECT head FROM routine_state WHERE key = 'todo/developer/main-moved'")" = "$(gitea_api "" "$hp/app/branches/main" | python3 -c 'import json,sys; print(json.load(sys.stdin)["commit"]["id"])')" ]; }
check "the commit it saw is kept, for after a restart" head_kept
check "the feed has the run, for todo's people" in_feed carol ns=todo "e['kind'] == 'routine' and e['agent'] == 'todo/developer' and e['actor'] == 'bob' and e['status'] == 'ok' and e['detail'] != ''"
if id=$(routine_post); then web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$hcid/messages/$id/delete"; fi

echo "# 4j2. a routine asks its person, and goes on with the answer"
cat > "$tmp/asker.yaml" <<'YAML'
description: Asks before it decides
tools: [list_repos]
routines:
  - name: colour
    on_push: {repo: app, branch: main}
    owner: bob
    task: "Call ask_person with exactly this question: Which colour should the header of todo be, blue or green? Then end your turn. When you have the answer, reply with one sentence that names the colour chosen."
YAML
printf 'You ask before you decide anything, and say plainly what was decided.\n' > "$tmp/asker.md"
apr=$(propose_config asker "Add the asker" "agents/asker/agent.yaml=$tmp/asker.yaml" "agents/asker/prompt.md=$tmp/asker.md")
check "carol approves the asker"           test "$(review config carol "$apr")" = 200
check "the approval check passes"          until_true 90 approval_is "$apr" success
check "alice merges it"                    test "$(merge config alice "$apr")" = 200
# Its routine must see main before the push, so the push is news to it.
asker_seen() { docker exec "$(docker ps -q -f name=postgres_postgres | head -n1)" psql -U postgres -d garrison -tAc "SELECT head FROM routine_state WHERE key = 'todo/asker/colour'" | grep -q .; }
check "the routine sees main"              until_true 180 asker_seen
printf 'More notes.\n' > "$tmp/notes2.md"
check "another change lands on app's main" to_main app NOTES2.md "$tmp/notes2.md" "More notes"
question_post() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**todo/asker**") and "<@U:" in c and "|bob>" in c and "Reply in this thread" in c:
        print(m["id"]); sys.exit(0)
sys.exit(1)'
}
check "it asks bob in the channel, naming him" until_true 600 question_post
qid=$(question_post)
waiting_page() { feed bob / | grep -q "Waiting for you" && feed bob / | grep -q "todo/asker asks bob"; }
check "bob's dashboard lists it as waiting for him" until_true 60 waiting_page
check "the feed has it, waiting"           in_feed bob ns=todo "e['kind'] == 'question' and e['agent'] == 'todo/asker' and e['actor'] == 'bob' and e['status'] == 'waiting'"
check "bob answers in its thread" \
  test "$(chat_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Green, please.","parent_id":"'"$qid"'"}' "$chat/api/v1/channels/$hcid/messages/post")" = 200
asker_reply() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages/$qid/thread?limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**todo/asker**") and "<@U:" not in c and len(c) > len("**todo/asker**") + 2:
        print(c); sys.exit(0)
sys.exit(1)'
}
check "the routine goes on, and says so in the thread" until_true 600 asker_reply
check "with bob's answer"                  grep -qi green <<<"$(asker_reply)"
check "the question is answered in the feed" in_feed bob ns=todo "e['kind'] == 'question' and e['agent'] == 'todo/asker' and e['status'] == 'ok' and 'Green' in e['detail']"
# The feed itself still lists the question; the list above it is gone.
not_waiting() { ! feed bob / | grep -q "Waiting for you"; }
check "bob's dashboard no longer lists it" until_true 60 not_waiting

echo "# 4k. an agent installs what it needs, from the package registries"
printf 'description: Tries things out with the tools it needs\ntools: [list_repos]\ninternet: packages\n' > "$tmp/builder.yaml"
printf 'You try things out in your sandbox, installing the tools a task needs. Show the commands you ran and what they printed.\n' > "$tmp/builder.md"
bpr=$(propose_config builder "Add the builder" "agents/builder/agent.yaml=$tmp/builder.yaml" "agents/builder/prompt.md=$tmp/builder.md")
check "carol approves the builder"         test "$(review config carol "$bpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$bpr" success
check "alice merges it"                    test "$(merge config alice "$bpr")" = 200
check "Open WebUI has todo/builder"    until_true 120 has_model todo/builder
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
installed() { docker exec "$(sandbox_of todo/builder)" python3 -c 'print(6*7)' 2>/dev/null | grep -qx 42; }
answer=$(ask_agent todo bob "Install Python in your sandbox with: apk add python3. Then run python3 -c 'print(6*7)' and tell me what it printed." builder)
if ! installed; then
  printf 'retry python3 is not installed; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please run this in your sandbox now: apk add python3 && python3 -c 'print(6*7)'" builder)
fi
check "it installs Python from Alpine's registry" installed
check "and says what it printed"           grep -q 42 <<<"$answer"
check "through the proxy, logged with bob and the agent" \
  bash -c 'docker service logs --since "$0" control-plane_agents 2>&1 | grep -q "agents: internet: bob: todo/builder: CONNECT dl-cdn.alpinelinux.org:443"' "$at"
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
refused_site() { docker service logs --since "$at" control-plane_agents 2>&1 | grep -q "agents: internet: bob: todo/builder: example.org:443 refused"; }
answer=$(ask_agent todo bob "Run exactly this command in your sandbox and show me what it printed: curl -sS -m 10 https://example.org/" builder)
if ! refused_site; then
  printf 'retry no request to example.org; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "I know it may fail; that is what I want to see. Please run: curl -sS -m 10 https://example.org/" builder)
fi
check "a site beyond the registries is refused" refused_site

echo "# 4l. bob asks the commander, which finds the agent for him"
answer=$(CHANNEL=todo ask_agent system bob "Which agent should I talk to for screenshots of the todo app for its homepage? Only tell me whom; do not do it yourself." commander)
if ! grep -q "todo/media" <<<"$answer"; then
  printf 'retry it named no media agent; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(CHANNEL=todo ask_agent system bob "Please call find_agents and tell me which agent of todo makes screenshots." commander)
fi
check "it names the media agent"           grep -q "todo/media" <<<"$answer"
handed() { in_feed bob ns=todo "e['kind'] == 'ask' and e['started_by'] == 'agent system/commander' and e['actor'] == 'bob' and e['agent'].startswith('todo/')" >/dev/null; }
answer=$(CHANNEL=todo ask_agent system bob "Hand this to the agent of todo that knows its stacks: which stacks does the namespace todo have? Have it answer here." commander)
if ! handed; then
  printf 'retry it handed nothing over; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(CHANNEL=todo ask_agent system bob "Please use ask_agent now: ask todo/operator which stacks the namespace todo has." commander)
fi
check "it hands a task over, as the feed shows" handed
check "which the agent finishes"           until_true 300 in_feed bob ns=todo "e['kind'] == 'ask' and e['started_by'] == 'agent system/commander' and e['status'] == 'ok'"

echo "# 5. alice removes bob"
since=$(last_pr config)
answer=$(ask_agent todo alice "Remove bob from the namespace todo.")
rpr=$(newest_pr config alice "$since")
if [ -z "$rpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo alice "Please propose the change to members.yaml in todo/config now, without bob.")
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
check "bob can still read everything"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$hp/app")" = 200

# Open WebUI follows the members within the gateway's next sync.
no_channel() { [ -z "$(channel_of todo bob)" ]; }
check "bob no longer sees todo's channel" until_true 120 no_channel

echo "# 5b. bob, no longer in todo, asks the desk for a change"
# ask_private PERSON AGENT QUESTION: asks an agent in a chat of PERSON's own,
# through Open WebUI's API, and prints its answer.
ask_private() {
  chat_api "$1" --max-time 600 -X POST -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"model": sys.argv[1], "stream": False, "messages": [{"role": "user", "content": sys.argv[2]}]}))' "$2" "$3")" \
    "$chat/api/chat/completions" | python3 -c 'import json,sys; print(json.load(sys.stdin)["choices"][0]["message"]["content"])'
}
since=$(last_pr app)
answer=$(ask_private bob todo/desk "In the repo app of the namespace todo, propose a new file IDEAS.md with the line: Dark mode, please. Title: An idea from bob. Read nothing else.")
opr=$(newest_pr app agent-todo-desk "$since")
if [ -z "$opr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_private bob todo/desk "Please propose that IDEAS.md for todo/app now with propose_change: namespace todo, repo app, path IDEAS.md, content: Dark mode, please.")
  opr=$(newest_pr app agent-todo-desk "$since")
fi
check "the desk proposes it with its own account" test -n "$opr"
check "it says for whom"                   grep -q 'Asked by bob (not in todo) through todo/desk' <<<"$(gitea_api "" "$hp/app/pulls/${opr:-0}")"
check "on a branch of the desk's"          grep -q '"ref":"agent/desk/bob-' <<<"$(gitea_api "" "$hp/app/pulls/${opr:-0}")"
[ -n "$opr" ] && gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$opr"

echo "# 5c. a private repo has a private agent"
check "alice makes a private repo secret"  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"secret","private":true,"auto_init":true,"default_branch":"main"}' "$api/orgs/todo/repos")" = 201
check "carol's feed shows it"              until_true 60 in_feed carol ns=todo "e['kind'] == 'repo-created' and e['repo'] == 'secret'"
# The dashboard asks Gitea again after at most 20 seconds.
sleep 21
check "bob's, out of todo, does not"       not_in_feed bob ns=todo "e['kind'] == 'repo-created' and e['repo'] == 'secret'"
printf 'description: Knows the repo secret\ntools: [read_file]\nrepos: [secret]\n' > "$tmp/keeper.yaml"
printf 'You answer questions about the namespace'"'"'s repo secret, reading its files.\n' > "$tmp/keeper.md"
kpr=$(propose_config keeper "Add the keeper" "agents/keeper/agent.yaml=$tmp/keeper.yaml" "agents/keeper/prompt.md=$tmp/keeper.md")
check "carol approves the keeper"          test "$(review config carol "$kpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$kpr" success
check "alice merges it"                    test "$(merge config alice "$kpr")" = 200
check "carol, in todo, sees it"        until_true 120 has_model todo/keeper carol
check "bob, no longer in todo, does not" not_seen todo/keeper bob
check "he still sees the agents of public repos" has_model todo/developer bob
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mention system carol todo/keeper "What is in the repo secret?" >/dev/null
refused_here() { docker service logs --since "$at" control-plane_agents 2>&1 | grep -q "agents: carol: todo/keeper: refused in channel:.*: private"; }
check "it does not answer in system's channel, which everyone reads" until_true 120 refused_here
answer=$(ask_agent todo carol "Read README.md of the repo secret of the namespace todo and tell me its first line." keeper)
check "it answers carol in todo's channel" grep -qi secret <<<"$answer"
gitea_api "" -o /dev/null -X DELETE "$hp/secret"

echo "# 6. alice adds an agent that may only read"
check "todo's config has its operator" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/config/raw/agents/operator/prompt.md")" = 200
reader() { add_agent "$1" reader "$2" "Reads the namespace's repos" "You read files of the namespace's repos and explain them."; }
bad=$(reader reader-bad 'read_file, rm_rf')
check "an agent with a tool that does not exist fails the check" until_true 90 approval_is "$bad" failure
check "the check names the tool"           grep -q 'rm_rf' <<<"$(approval "$bad")"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/$bad"
good=$(reader reader read_file)
check "carol approves the reader"          test "$(review config carol "$good")" = 200
check "the approval check passes"          until_true 90 approval_is "$good" success
check "alice merges it"                    test "$(merge config alice "$good")" = 200
check "Open WebUI has todo/reader"     until_true 120 has_model todo/reader
since=$(last_pr app)
answer=$(ask_agent todo alice "Propose a change to the repo app: add a file reader.txt containing hello. Title: Reader test." reader)
check "the reader answers"                 test -n "$answer"
check "it cannot propose a change"         test -z "$(newest_pr app alice "$since")"

echo "# 7. the operator finds out why an app does not run"
# First the operator learns to look by itself when an app keeps failing.
{ gitea_api "" "$hp/config/raw/agents/operator/agent.yaml"
  printf 'routines:\n  - name: on-failure\n    on_alert: { alertname: ServiceFailing }\n    owner: alice\n    task: An app of todo keeps failing. Find out why from its stack'"'"'s tasks and logs, and say what to change, naming the setting.\n'; } > "$tmp/operator.yaml"
opr=$(propose_config operator "The operator looks when an app keeps failing" "agents/operator/agent.yaml=$tmp/operator.yaml")
check "carol approves the operator's alert routine" test "$(review config carol "$opr")" = 200
check "the approval check passes"          until_true 90 approval_is "$opr" success
check "alice merges it"                    test "$(merge config alice "$opr")" = 200
check "the quotes app's source goes to todo/quotes" installer/push-repo.sh --update todo/quotes test/e2e/apps/quotes
check "alice adds its stack, carol approves" set_stack todo quotes test/e2e/apps/stacks/quotes.yaml carol
quotes_deployed() { garrison applied | grep -q "^app todo/quotes@main quotes "; }
check "its pipeline builds it and the deployer deploys it" until_true 900 quotes_deployed
quotes_crashes() { docker service ps quotes_web --format '{{.Error}}' 2>/dev/null | grep -q 'non-zero exit'; }
check "it does not start"                  until_true 120 quotes_crashes
am_alerts() { docker exec "$(docker ps -q -f name=prometheus_alertmanager | head -n1)" wget -qO- http://localhost:9093/api/v2/alerts; }
quotes_alert() { am_alerts | python3 -c '
import json,sys
sys.exit(0 if any(a["labels"].get("alertname") == "ServiceFailing" and a["labels"].get("app") == "quotes" and a["labels"].get("namespace") == "todo" for a in json.load(sys.stdin)) else 1)'; }
check "an alert fires: it keeps failing"   until_true 400 quotes_alert
check "todo's people see it in the feed"   until_true 120 in_feed carol ns=todo "e['kind'] == 'alert' and e['status'] == 'failed' and 'quotes_web' in e['title']"
check "bob, out of todo, does not"         not_in_feed bob ns=todo "e['kind'] == 'alert' and 'quotes_web' in e['title']"
alert_post() {
  chat_api alice "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**todo/operator**") and "Routine on-failure, for alice (the alert ServiceFailing fired" in c:
        print(c); sys.exit(0)
sys.exit(1)'
}
check "it starts the operator, which posts in todo's channel" until_true 600 alert_post
check "naming the missing setting"         grep -q QUOTES_FILE <<<"$(alert_post)"
check "the feed has its run"               in_feed carol ns=todo "e['kind'] == 'routine' and e['agent'] == 'todo/operator' and 'ServiceFailing' in e['title']"
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo alice "The quotes app of the namespace todo, https://quotes.$DOMAIN/, does not work. Find out why and tell me what to change.")
check "it looks at the stack, as alice"    grep -qE "mcp: alice: (stack_status|service_logs)" <<<"$(mcp_calls "$at" alice)"
check "it names the missing setting"       grep -q QUOTES_FILE <<<"$answer"
check "alice removes its stack, carol approves" set_stack todo quotes - carol
quotes_gone() { ! docker service inspect quotes_web >/dev/null 2>&1; }
check "that takes the app down"            until_true 120 quotes_gone
check "and the alert resolves"             until_true 300 in_feed carol ns=todo "e['kind'] == 'alert' and e['status'] == 'ok' and 'quotes_web' in e['title']"
gitea_api "" -o /dev/null -X DELETE "$hp/quotes"

echo "# 8. alice has the operator set up a new app, from nothing"
answer=$(ask_agent system alice "Set up a new app for us: make a namespace photos for our photo apps, and in it the app gallery, a small web app that shows who is visiting: the image traefik/whoami:v1.11 on port 80, behind the sign-in. It comes from elsewhere, so it needs no repo of its own. Propose its stack. You have everything you need; do not ask back.")
org_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/photos")" = 200 ]; }
# stack_pr: the open pull request of photos/config by alice that adds stacks/gallery.yaml.
stack_pr() {
  local n
  for n in $(gitea_api "" "$api/repos/photos/config/pulls?state=open&limit=10" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="alice"]' 2>/dev/null); do
    gitea_api "" "$api/repos/photos/config/pulls/$n/files" | grep -q '"filename":"stacks/gallery.yaml"' && { echo "$n"; return 0; }
  done
  return 1
}
if ! org_made || ! stack_pr >/dev/null; then
  printf 'retry the operator did not do both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent system alice "Please go ahead now: create_namespace photos if it is missing, then propose stacks/gallery.yaml in photos/config with propose_change (image traefik/whoami:v1.11, port 80, entrypoint web, environment main private).")
fi
check "it makes the namespace photos, as alice" org_made
photos_admin() { gitea_api "" "$api/repos/photos/config/raw/members.yaml" | python3 -c 'import sys,yaml; sys.exit(0 if (yaml.safe_load(sys.stdin) or {}).get("admins") == ["alice"] else 1)'; }
check "with alice as its first admin"      until_true 180 photos_admin
gpr=$(stack_pr)
check "it proposes stacks/gallery.yaml in photos' config, as alice" test -n "$gpr"
check "no repo for it, and nothing in system" bash -c '[ "$0" = 404 ] && [ "$1" = 404 ]' "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/photos/gallery")" "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/system/gallery")"
gitea_api "" "$api/repos/photos/config/raw/stacks/gallery.yaml?ref=$(gitea_api "" "$api/repos/photos/config/pulls/${gpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)" > "$tmp/gallery.yaml"
check "one the deployer takes"             garrison validate --in photos "$tmp/gallery.yaml"
check "a root admin who did not write it approves" test "$(gitea_api "" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$api/repos/photos/config/pulls/${gpr:-0}/reviews")" = 200
photos_approved() {
  gitea_api "" "$api/repos/photos/config/commits/$(gitea_api "" "$api/repos/photos/config/pulls/${gpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "the approval check passes"          until_true 120 photos_approved
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$api/repos/photos/config/pulls/${gpr:-0}/merge")" = 200
gallery_deployed() { garrison applied | grep -q "^app photos/gallery@main gallery "; }
check "the deployer deploys it"            until_true 300 gallery_deployed
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent system bob "Make me a namespace bobs for my own apps.")
check "bob cannot make a namespace"        test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/bobs")" = 404
refused_ns() { refused_only "$since" bob create_namespace "only root admins may"; }
check "the gateway refused every try"      refused_ns

echo "# 9. alice has the operator bring todo's data back"
todo_ds=garrison/ns/todo/stacks/todo
was=$(zfs get -H -o value garrison:restored "$todo_ds")
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo alice "I deleted todos in the todo app by mistake. Bring todo's data on main back to the newest snapshot taken before a deploy. Propose it; I know newer data gets lost. Do not ask back.")
# restore_pr: the open pull request of todo/config by alice that changes restores/.
restore_pr() {
  local n
  for n in $(gitea_api "" "$hp/config/pulls?state=open&limit=10" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="alice"]' 2>/dev/null); do
    gitea_api "" "$hp/config/pulls/$n/files" | grep -q '"filename":"restores/[^"]*\.yaml"' && { echo "$n"; return 0; }
  done
  return 1
}
if ! restore_pr >/dev/null; then
  printf 'retry the operator proposed no restore; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo alice "Please go ahead now: list_snapshots for the stack todo in todo, take the newest deploy-… one, and propose restores/todo.yaml in todo/config with propose_change (stack: todo, environment: main, snapshot: <its name>).")
fi
lists_snaps() { mcp_calls "$since" alice | grep -q "mcp: alice: list_snapshots"; }
check "it lists todo's snapshots, as alice" lists_snaps
rpr=$(restore_pr)
check "it proposes a restore in todo's config, as alice" test -n "$rpr"
rhead=$(gitea_api "" "$hp/config/pulls/${rpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)
rfile=$(gitea_api "" "$hp/config/pulls/${rpr:-0}/files" | python3 -c 'import json,sys; [print(f["filename"]) for f in json.load(sys.stdin) if f["filename"].startswith("restores/")]' 2>/dev/null | head -n1)
rsnap=$(gitea_api "" "$hp/config/raw/$rfile?ref=$rhead" | python3 -c 'import sys,yaml; print((yaml.safe_load(sys.stdin) or {}).get("snapshot",""))' 2>/dev/null)
check "of a snapshot todo has"             zfs list "$todo_ds@${rsnap:-none}"
check "carol approves it"                  test "$(gitea_api carol -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Go ahead."}' "$hp/config/pulls/${rpr:-0}/reviews")" = 200
restore_approved() {
  gitea_api "" "$hp/config/commits/$rhead/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "the approval check passes"          until_true 120 restore_approved
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$hp/config/pulls/${rpr:-0}/merge")" = 200
restored_again() { v=$(zfs get -H -o value garrison:restored "$todo_ds"); [ "$v" != - ] && [ "$v" != "$was" ]; }
check "the deployer restores it"           until_true 300 restored_again
todo_back() { status_is 302 todo / 5; }
check "todo answers again"                 until_true 180 todo_back

while read -r c m; do
  for id in $(web -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$c/messages/$m/thread" | python3 -c 'import json,sys; [print(x["id"]) for x in json.load(sys.stdin)]' 2>/dev/null) $m; do
    web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$c/messages/$id/delete"
  done
done < "$tmp/posted"
}

# crud: made, read, changed and removed, every kind of thing a namespace holds.
. test/e2e/crud.sh
# journey: an app with code of its own, from nothing to main, through the agents.
. test/e2e/journey.sh

# ui: the same, and more, clicked and typed in the browser (test/ui).
section_ui() {
  echo "# the browser scenarios (test/ui), in Playwright's Chromium"
  test/ui/run.sh 2>"$tmp/ui.err" | tee "$tmp/ui.out" || true
  local n
  n=$(grep -c '^FAIL' "$tmp/ui.out" || true)
  # done() prints one of these last; without it the script broke off.
  grep -Eq '^(all checks passed|[0-9]+ check\(s\) failed)' "$tmp/ui.out" ||
    { echo "FAIL  the browser scenarios ran to the end"; tail -15 "$tmp/ui.err"; n=$((n + 1)); }
  failed=$((failed + n))
}

# Usage: smoke.sh [SECTION...]; system, login and platform by default.
sections=${*:-system login platform}
for s in $sections; do
  t0=$SECONDS
  case $s in system|login|platform|agent|deploy|talk|crud|journey|ui) "section_$s" ;; *) echo "unknown section: $s" >&2; exit 2 ;; esac
  echo "time  section $s took $(( (SECONDS - t0) / 60 ))m$(( (SECONDS - t0) % 60 ))s"
done
# Slow checks: each check's time, longest first.
echo
if [ $failed -eq 0 ]; then echo "all checks passed"; else echo "$failed check(s) failed"; fi
exit $((failed > 0))
