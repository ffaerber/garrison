# Helpers the scenarios share (test/e2e/scenarios/), sourced by smoke.sh.

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
  for h in keycloak sso git grafana prometheus loki chat gateway agents hello dev-hello todo dev-todo example dev-example files; do args+=(--resolve "$h.$DOMAIN:443:127.0.0.1"); done
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

# After a reboot, give every pod of the platform time to come back before checking.
if [ -n "${WAIT_READY:-}" ]; then
  test/e2e/ready.sh "$WAIT_READY" || true
fi

# logs_since SINCE NAME: the logs of the platform's Deployment NAME since
# SINCE, a duration (15m) or a time (2026-11-21T10:00:00Z).
logs_since() {
  local flag=--since
  case $1 in *T*) flag=--since-time ;; esac
  kubectl --namespace garrison logs "deployment/$2" --all-containers "$flag=$1" 2>&1
}


# ---------- shared helpers ----------
TEST_PASSWORD=${TEST_PASSWORD:-12345678}   # alice and bob, see test/e2e/add-test-users.sh
# gitea_api USER_TO_ACT_AS ARGS...: call the Gitea API as the local admin, or,
# with a name, with exactly that person's rights (Gitea's Sudo header).
gitea_api() {
  local as=$1; shift
  web -u "$ADMIN_USER:$PASSWORD" ${as:+-H "Sudo: $as"} "$@"
}
api=https://git.$DOMAIN/api/v1
# Every system stack is a repo of the garrison organisation; traefik stands in for them.
repo=$api/repos/garrison/traefik
# until_true SECONDS COMMAND...: retry until the command succeeds.
until_true() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do [ $SECONDS -ge $deadline ] && return 1; sleep 3; done
}
# The dashboard's feed, at the bare domain. home_login WHO signs a person in
# to it; feed WHO PATH calls its API as them, signing them in first if this
# run has not: each scenario starts with no one signed in.
feed() {
  local who=$1; shift
  [ -s "$tmp/home-$who.jar" ] || until_true 60 home_login "$who" || true
  web -b "$tmp/home-$who.jar" "https://$DOMAIN$1"
}
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
    for k in ("commit", "detail", "agent", "actor", "started_by", "link", "repo", "unit"):
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
# unit NS's config as FILE, or its removal with -; APPROVER (by default
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
  # Gitea checks a pull request before it merges it, and says to try again
  # later until it has.
  for i in $(seq 15); do
    [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$cfg/pulls/$pr/merge")" = 200 ] && return 0
    sleep 2
  done
  echo "cannot merge #$pr ($st)"; return 1
}
# set_stack NS NAME FILE|- [APPROVER]: set_config for config/stacks/NAME.yaml.
set_stack() { set_config "$1" "stacks/$2.yaml" "$3" "${4:-}"; }
# ---------- apps: each environment a namespace, app-<stack> ----------
# app_up STACK: whether the environment's namespace has its Deployments.
app_up() { [ -n "$(kubectl --namespace "app-$1" get deployments --output name 2>/dev/null)" ]; }
# app_gone STACK: whether the environment's Deployments are gone. Its
# namespace stays, with its Secrets.
app_gone() { ! app_up "$1"; }
# app_secret STACK NAME: whether the environment has the Secret NAME.
app_secret() { kubectl --namespace "app-$1" get secret "$2" >/dev/null 2>&1; }
# in_app STACK SERVICE COMMAND...: runs COMMAND in the service's pod.
in_app() { local st=$1 svc=$2; shift 2; kubectl --namespace "app-$st" exec -i "deployment/$svc" -- "$@"; }
# app_crashes STACK SERVICE: whether the service's container ended in an error.
app_crashes() {
  kubectl --namespace "app-$1" get pods --selector "garrison.io/service=$2" --output json 2>/dev/null | python3 -c '
import json, sys
for p in json.load(sys.stdin)["items"]:
    for c in p["status"].get("containerStatuses") or []:
        for t in ((c.get("lastState") or {}).get("terminated"), (c.get("state") or {}).get("terminated")):
            if t and t.get("exitCode", 0) != 0:
                sys.exit(0)
sys.exit(1)'
}
# app_claim STACK SERVICE CLAIM VOLUME PATH: whether the service mounts the
# claim at PATH, bound to the OpenEBS volume VOLUME (its ZFSVolume).
app_claim() {
  local pv
  pv=$(kubectl --namespace "app-$1" get pvc "$3" -o jsonpath='{.status.phase} {.spec.volumeName}') || return 1
  [ "$pv" = "Bound $4" ] || { echo "claim $3: $pv"; return 1; }
  [ "$(kubectl get pv "$4" -o jsonpath='{.spec.csi.volumeHandle}')" = "$4" ] || return 1
  kubectl --namespace "app-$1" get deployment "$2" --output json | python3 -c '
import json, sys
spec = json.load(sys.stdin)["spec"]["template"]["spec"]
claims = {v["name"]: v["persistentVolumeClaim"]["claimName"] for v in spec.get("volumes", []) if "persistentVolumeClaim" in v}
sys.exit(0 if any(claims.get(m["name"]) == sys.argv[1] and m["mountPath"] == sys.argv[2] for m in spec["containers"][0].get("volumeMounts", [])) else 1)' "$3" "$5"
}

# stack_objects NAME: what the platform's stack NAME has in the cluster.
stack_objects() { kubectl --namespace garrison get all --selector "garrison.io/stack=$1" --output name 2>/dev/null; }
smoke_running() { [ "$(kubectl --namespace garrison get deployment smoke -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = 1 ]; }
smoke_gone()    { [ -z "$(stack_objects smoke)" ]; }
deployer_image() { kubectl --namespace garrison get deployment deployer -o jsonpath='{.spec.template.spec.containers[0].image}'; }
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
# system_channel [PERSON]: the id of the channel of the unit garrison.
system_channel() { chat_api "${1:-bob}" "$chat/api/v1/channels/" | python3 -c 'import json,sys; print(next(c["id"] for c in json.load(sys.stdin) if c["name"] == "garrison"))' 2>/dev/null; }
# mcp_calls SINCE PERSON: the tools the gateway ran for PERSON, as "mcp: PERSON: TOOL".
mcp_calls() { logs_since "$1" gateway | grep -oE "mcp: $2: [a-z0-9._-]+/[a-z0-9-]+: [a-z_]+" | sed -E 's#: [^ ]+/[^ ]+: #: #' | sort -u; }
# mcp ARGS...: the gateway's MCP endpoint, which only Open WebUI reaches.
mcp() {
  kubectl --namespace garrison exec -i deployment/openwebui -- \
    curl -sS --max-time 20 "$@" http://gateway:8080/mcp
}

# agents_api ARGS... PATH: the agent service, which only the system network reaches.
agents_api() {
  local path=${!#}
  kubectl --namespace garrison exec -i deployment/openwebui -- \
    curl -sS --max-time 20 "${@:1:$#-1}" "http://agents:8070$path"
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
# ask_agent UNIT PERSON QUESTION [AGENT]: asks an agent, the operator by
# default, in the unit's channel (or $CHANNEL), where it works in that
# unit, and prints its answer from the thread. The agents are
# garrison's: their model is garrison/<agent>. It waits ASK_WAIT seconds
# (360) for the answer; a turn may take up to the agent service's 10 minutes.
ask_agent() {
  local ns=$1 who=$2 question=$3 id=garrison/${4:-operator} cid mid i
  read -r cid mid <<<"$(mention "${CHANNEL:-$ns}" "$who" "$id" "$question")"
  [ -n "$mid" ] || return 1
  for i in $(seq $(( ${ASK_WAIT:-360} / 5 ))); do
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

# render_todo NS REPO DIR: the template todo as create_repo writes it into
# NS/REPO, in DIR (NAME.tmpl as NAME, the placeholders filled in), and the
# stack its README says to propose, in DIR.stack.yaml.
render_todo() {
  rm -rf "$3" && cp -r templates/app/todo "$3" || return 1
  find "$3" -name '*.tmpl' -exec sh -c 'mv "$1" "${1%.tmpl}"' _ {} \;
  find "$3" -type f -exec sed -i "s/{{unit}}/$1/g; s/{{repo}}/$2/g" {} +
  python3 -c '
import re,sys
print(re.search(r"```yaml\n(.*?)```", open(sys.argv[1]).read(), re.S).group(1), end="")' "$3/README.md" > "$3.stack.yaml"
}


# ---------- talking to the agents of todo ----------
hp=$api/repos/todo
# talk_setup: what the scenarios that talk to the agents of todo share:
# everyone signed in, and grants.yaml and routines.yaml of todo/config as
# they are on main, for a scenario to add to. Fails without an LLM key.
talk_setup() {
echo "# talking to the agent of todo"
if [ ! -s $ETC/llm-api-key ]; then
  echo "skip  no LLM key in $ETC/llm-api-key"
  return 1
fi
hp=$api/repos/todo
for u in alice bob carol; do
  sso_login "$tmp/git-$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/git-$u.html" $u >/dev/null 2>&1
  check "$u signs in to Open WebUI" chat_login $u
done
admin_token=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" \
  "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
gitea_api "" -f "$hp/config/raw/grants.yaml" > "$tmp/grants.yaml" 2>/dev/null || : > "$tmp/grants.yaml"
gitea_api "" -f "$hp/config/raw/routines.yaml" > "$tmp/routines.yaml" 2>/dev/null || : > "$tmp/routines.yaml"
}
# make_branch AS REPO_URL NAME [FROM]: AS makes the branch NAME of the repo at
# REPO_URL from FROM (main), and it exists. It looks at the branch, not at the
# answer: Gitea can take longer than web's 20 s on a fresh clone and make it anyway.
make_branch() {
  local as=$1 repo=$2 name=$3 from=${4:-main}
  gitea_api "$as" -o /dev/null -X POST -H 'Content-Type: application/json' \
    -d "{\"new_branch_name\":\"$name\",\"old_branch_name\":\"$from\"}" "$repo/branches" || true
  until_true 60 gitea_api "$as" -f -o /dev/null "$repo/branches/$name"
}
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
# merges REPO USER PR [REVIEWER]: USER merges, retrying while Gitea still
# checks the pull request ("try again later"); an approval given meanwhile
# can go uncounted, so REVIEWER gives it again now and then.
merges() {
  local code i
  for i in $(seq 15); do
    code=$(merge "$1" "$2" "$3")
    [ "$code" = 200 ] && return 0
    [ "$code" = 405 ] && [ -n "${4:-}" ] && [ $((i % 5)) = 0 ] && review "$1" "$4" "$3" >/dev/null
    sleep 2
  done
  echo "      $2's merge of #$3: $code: $(gitea_api "$2" -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$hp/$1/pulls/$3/merge" | head -c 300)"
  return 1
}
team_has() {
  local id
  id=$(gitea_api "" "$api/orgs/todo/teams" | python3 -c 'import json,sys; print(next(t["id"] for t in json.load(sys.stdin) if t["name"]==sys.argv[1]))' "$1")
  gitea_api "" "$api/teams/$id/members" | grep -q "\"login\":\"$2\""
}
# The agents are garrison's: alice, a root admin, proposes them in
# garrison/config, and another root admin approves them. todo gives them
# rights and routines in its own config (grants.yaml, routines.yaml).
gc=$api/repos/garrison/config
gc_approval() { gitea_api "" "$gc/commits/$(gitea_api "" "$gc/pulls/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"], s["description"]); break'; }
gc_approval_is() { gc_approval "$1" | grep -q "^$2"; }
# lands PR: the local admin, a root admin, approves garrison/config's pull
# request PR, and alice merges it once the approval check passes.
lands() {
  [ "$(gitea_api "" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$gc/pulls/$1/reviews")" = 200 ] &&
    until_true 90 gc_approval_is "$1" success &&
    [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$gc/pulls/$1/merge")" = 200 ]
}
# add_agent BRANCH NAME TOOLS DESCRIPTION PROMPT [SKILLS] [SKILL_FILE...]:
# alice proposes agents/NAME in garrison/config, with the shared skills
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
  gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d "$files" "$gc/contents"
  gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$1"'","base":"main","title":"Add the '"$2"' agent"}' "$gc/pulls" |
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
  logs_since "$1" gateway | grep "mcp: $2: [^ ]*: $3" | grep -v "$4" | grep -q . && return 1
  return 0
}
# has_model ID [PERSON]: Open WebUI offers the agent to PERSON (bob). Not the
# admin: an admin sees an agent before the gateway lets everyone use it.
has_model() { chat_api "${2:-bob}" "$chat/api/models" | grep -q "\"id\":\"$1\""; }
can_push() { gitea_api "$1" "$hp/app" | grep -q '"push":true'; }
cannot_push() { ! can_push "$1"; }

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
  local branch=land-$(date +%s%N | cut -c1-13) n code i
  code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"'"$branch"'","old_branch_name":"main"}' "$hp/$1/branches")
  [ "$code" = 201 ] || { echo "      the branch: $code"; return 1; }
  code=$(set_file "$1" "$branch" "$2" "$3" "$4")
  case $code in 200|201) ;; *) echo "      the file: $code"; return 1 ;; esac
  n=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"'"$4"'"}' "$hp/$1/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])') || { echo "      no pull request"; return 1; }
  code=$(review "$1" carol "$n")
  [ "$code" = 200 ] || { echo "      carol's review of #$n: $code"; return 1; }
  # Gitea checks a new pull request before it merges it, and says to try
  # again later until it has.
  for i in $(seq 15); do
    code=$(merge "$1" alice "$n")
    [ "$code" = 200 ] && return 0
    # An approval given while Gitea still checked the new pull request can
    # go uncounted ("Does not have enough approvals"); carol gives it again.
    [ "$code" = 405 ] && [ $((i % 5)) = 0 ] && review "$1" carol "$n" >/dev/null
    sleep 2
  done
  echo "      alice's merge of #$n: $code: $(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$hp/$1/pulls/$n/merge" | head -c 300)"
  return 1
}
# propose_config BRANCH TITLE PATH=FILE...: alice proposes files in todo/config
# (or $REPO, e.g. $gc); prints the pull request's number.
propose_config() {
  local branch=$1 title=$2 repo=${REPO:-$hp/config} spec specs=() ops; shift 2
  for spec in "$@"; do
    # Each with the blob it replaces, if any.
    specs+=("$spec=$(gitea_api "" "$repo/contents/${spec%%=*}" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))' 2>/dev/null)")
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
  gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d "$ops" "$repo/contents"
  gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"'"$title"'"}' "$repo/pulls" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])'
}
has_account() { gitea_api "" "$api/users/$1" | grep -q '"full_name":"Agent todo/'; }
# label_value V: V as a label value, as the agent service writes it on a
# sandbox's pod (sandbox.labelValue): other characters become _.
label_value() { printf '%s' "$1" | sed -E 's/[^A-Za-z0-9._-]+/_/g; s/^[_.-]+//; s/[_.-]+$//' | cut -c1-63; }
# sandbox_of AGENT: the running sandbox pod of AGENT for todo's channel.
sandbox_of() {
  kubectl --namespace garrison-sandboxes get pods --field-selector status.phase=Running --output name \
    --selector "garrison.sandbox=true,garrison.agent=$(label_value "$1"),garrison.chat=$(label_value "channel:$(channel_of todo)")" | head -n1 | sed 's#^pod/##'
}
# in_sandbox POD COMMAND...: runs COMMAND in a sandbox, as the agent would.
in_sandbox() { local pod=$1; shift; kubectl --namespace garrison-sandboxes exec -i "$pod" -- "$@"; }
# sandbox_image: the sandbox image the agent service runs.
sandbox_image() {
  kubectl --namespace garrison get deployment agents -o jsonpath='{.spec.template.spec.containers[0].args}' |
    grep -oE "git\.$DOMAIN/garrison/sandbox:[A-Za-z0-9._-]+" | head -n1
}
# check_namespace: garrison-check, for the tests' own pods, with the pull
# secret of the platform's images.
check_namespace() {
  kubectl get namespace garrison-check >/dev/null 2>&1 || kubectl create namespace garrison-check >/dev/null
  kubectl --namespace garrison-check get secret registry-pull >/dev/null 2>&1 ||
    kubectl --namespace garrison get secret registry-pull -o json |
      python3 -c 'import json,sys; s=json.load(sys.stdin); s["metadata"]={"name":"registry-pull"}; print(json.dumps(s))' |
      kubectl --namespace garrison-check apply -f - >/dev/null
}
# image_pod NAME [-g] [-d] [-v HOST:PATH] -- COMMAND...: runs COMMAND in a pod
# of the sandbox image in garrison-check, under gVisor with -g, with the host
# folder HOST read-only at PATH with -v. It prints the output and is gone
# after, or with -d is left running in the background.
image_pod() {
  local name=$1 runtime= detach= vol= overrides; shift
  while [ "$1" != -- ]; do
    case $1 in -g) runtime=gvisor ;; -d) detach=1 ;; -v) vol=$2; shift ;; esac
    shift
  done
  shift
  check_namespace
  overrides=$(python3 - "$name" "$(sandbox_image)" "$runtime" "$vol" "$@" <<'PY'
import json, sys
name, image, runtime, vol, cmd = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5:]
c = {"name": name, "image": image, "command": cmd}
spec = {"containers": [c], "imagePullSecrets": [{"name": "registry-pull"}], "automountServiceAccountToken": False, "restartPolicy": "Never"}
if runtime:
    spec["runtimeClassName"] = runtime
    c["securityContext"] = {"runAsUser": 0}
if vol:
    host, path = vol.split(":", 1)
    c["volumeMounts"] = [{"name": "v", "mountPath": path, "readOnly": True}]
    spec["volumes"] = [{"name": "v", "hostPath": {"path": host}}]
print(json.dumps({"spec": spec}))
PY
)
  if [ -n "$detach" ]; then
    kubectl --namespace garrison-check run "$name" --image "$(sandbox_image)" --restart Never --overrides "$overrides" >/dev/null &&
      kubectl --namespace garrison-check wait --for=condition=Ready "pod/$name" --timeout 600s >/dev/null
  else
    # Not kubectl run -i: it attaches once the container runs, and loses
    # what a short command printed before. The pod's log keeps it all.
    local rc=1 phase
    kubectl --namespace garrison-check delete pod "$name" --ignore-not-found --wait >/dev/null
    kubectl --namespace garrison-check run "$name" --image "$(sandbox_image)" --restart Never --overrides "$overrides" >/dev/null
    for _ in $(seq 300); do
      phase=$(kubectl --namespace garrison-check get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
      case $phase in Succeeded) rc=0; break ;; Failed) break ;; esac
      sleep 2
    done
    kubectl --namespace garrison-check logs "pod/$name" 2>&1
    kubectl --namespace garrison-check delete pod "$name" --wait=false >/dev/null 2>&1
    return $rc
  fi
}

# ui_run NAME: the browser scenario test/ui/NAME.py, its checks counted with
# the others.
ui_run() {
  echo "# the browser scenario $1 (test/ui/$1.py), in Playwright's Chromium"
  test/ui/run.sh "$1" 2>"$tmp/ui.err" | tee "$tmp/ui.out" || true
  local n
  n=$(grep -c '^FAIL' "$tmp/ui.out" || true)
  # done() prints one of these last; without it the script broke off.
  grep -Eq '^(all checks passed|[0-9]+ check\(s\) failed)' "$tmp/ui.out" ||
    { echo "FAIL  the browser scenario $1 ran to the end"; tail -15 "$tmp/ui.err"; n=$((n + 1)); }
  failed=$((failed + n))
}
