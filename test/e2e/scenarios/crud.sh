# crud: every kind of thing a unit holds is made, read, changed and
# removed the way people do it (Gitea's API as alice, pull requests to
# config), and the test checks what the platform does about each: Gitea's
# teams and accounts, Open WebUI's channels, groups and agents (garrison's,
# with crud's grants and routines), the
# deployer's stacks, datasets, secrets and hostnames. No LLM. Sourced by
# smoke.sh, whose helpers it uses; the unit is crud.

# change_config NS APPROVER PATH=FILE|PATH=- ...: alice proposes these files of
# the unit's config in one pull request (- removes the file), APPROVER
# ("" for the local admin, a root admin) approves, and alice merges it once
# the approval check passes. cfg_pr is its number. Fails if a step does,
# leaving the pull request open.
change_config() {
  local ns=$1 approver=$2 cfg=$api/repos/$1/config branch body st i spec path specs=()
  shift 2
  for spec in "$@"; do
    path=${spec%%=*}
    specs+=("$spec=$(gitea_api "" "$cfg/contents/$path" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))' 2>/dev/null)")
  done
  branch="crud-$(date +%s%N | cut -c1-13)"
  body=$(python3 -c '
import base64,json,sys
branch=sys.argv[1]; ops=[]
for spec in sys.argv[2:]:
    path,file,sha=spec.split("=",2)
    if file=="-": ops.append({"operation":"delete","path":path,"sha":sha})
    else:
        op={"operation":"update" if sha else "create","path":path,"content":base64.b64encode(open(file,"rb").read()).decode()}
        if sha: op["sha"]=sha
        ops.append(op)
print(json.dumps({"branch":"main","new_branch":branch,"message":"Change "+", ".join(o["path"] for o in ops),"files":ops}))' "$branch" "${specs[@]}") || return 1
  [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$body" "$cfg/contents")" = 201 ] || { echo "cannot write $*"; return 1; }
  cfg_pr=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"'"$branch"'","base":"main","title":"Change config"}' "$cfg/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])') || return 1
  [ "$(gitea_api "$approver" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$cfg/pulls/$cfg_pr/reviews")" = 200 ] || { echo "cannot approve #$cfg_pr"; return 1; }
  for i in $(seq 60); do
    st=$(gitea_api "" "$cfg/commits/$(gitea_api "" "$cfg/pulls/$cfg_pr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"], s["description"]); break' 2>/dev/null)
    case $st in success*) break ;; failure*) echo "approval: $st"; return 1 ;; esac
    sleep 2
  done
  [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$cfg/pulls/$cfg_pr/merge")" = 200 ] || { echo "cannot merge #$cfg_pr ($st)"; return 1; }
}
# refused_by_check NS PATH=FILE...: the change fails the approval check; its
# pull request is closed again.
refused_by_check() {
  cfg_pr=
  change_config "$@" > "$tmp/refused.out" 2>&1 && { echo "passed the check"; return 1; }
  [ -n "$cfg_pr" ] && gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$api/repos/$1/config/pulls/$cfg_pr"
  grep -q "^approval: failure" "$tmp/refused.out" || { cat "$tmp/refused.out"; return 1; }
}
# put_file NAME CONTENT: writes CONTENT (printf format) to $tmp/NAME and prints the path.
put_file() { printf -- "$2" > "$tmp/$1"; echo "$tmp/$1"; }

not() { ! "$@"; }
# Gitea
team_has() {   # team_has NS TEAM PERSON
  local id
  id=$(gitea_api "" "$api/orgs/$1/teams" | python3 -c 'import json,sys; print(next(t["id"] for t in json.load(sys.stdin) if t["name"]==sys.argv[1]))' "$2") || return 1
  gitea_api "" "$api/teams/$id/members" | python3 -c 'import json,sys; sys.exit(0 if any(u["login"]==sys.argv[1] for u in json.load(sys.stdin)) else 1)' "$3"
}
can_push_to() { gitea_api "$1" "$api/repos/$2" | grep -q '"push":true'; }   # can_push_to PERSON NS/REPO
user_status() { gitea_api "" -o /dev/null -w '%{http_code}' "$api/users/$1"; }
locked() { gitea_api "" "$api/users/$1" | grep -q '"prohibit_login":true'; }
unlocked() { gitea_api "" "$api/users/$1" | grep -q '"prohibit_login":false'; }
collaborator() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/$1/collaborators/$2")" = 204 ]; }   # NS/REPO ACCOUNT
# Open WebUI
sees_model() { chat_api "$1" "$chat/api/models" | grep -q "\"id\":\"$2\""; }          # PERSON ID
sees_channel() { chat_api "$1" "$chat/api/v1/channels/" | python3 -c 'import json,sys; sys.exit(0 if any(c["name"]==sys.argv[1] for c in json.load(sys.stdin)) else 1)' "$2"; }
has_group() { chat_api alice "$chat/api/v1/groups/" | grep -q "\"name\":\"garrison $1\""; }
model_says() { chat_api alice "$chat/api/v1/models/model?id=$1" | grep -q "$2"; }   # ID TEXT
# the deployer
applied_app() { garrison applied | grep -q "^app $1 "; }                          # NS/NAME@ENV
stack_up() { app_up "$1"; }
claimed() { python3 -c 'import json,sys; sys.exit(0 if any(c["Hostname"]==sys.argv[2] for c in json.load(open(sys.argv[1]))) else 1)' /var/lib/garrison/deployer/claims.json "$1"; }
says() { req "$1" "https://$1.$DOMAIN/" | grep -q "$2"; }                            # HOST TEXT

section_crud() {
for u in alice bob; do chat_login $u; done
ns=crud
o=$api/orgs/$ns

echo "# a unit"
check "alice creates the unit crud"   test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"crud","visibility":"limited","description":"Made by the crud test"}' "$api/orgs")" = 201
first_admin() { gitea_api "" "$api/repos/$ns/config/raw/members.yaml" | python3 -c 'import sys,yaml; sys.exit(0 if (yaml.safe_load(sys.stdin) or {}).get("admins") == ["alice"] else 1)'; }
# Gitea tells nobody of a new organisation; the platform notices within
# seconds and sets it up at once, not at its loops' next tick.
check "within seconds it has its config, alice its first admin" until_true 15 first_admin
check "and alice joins its admins"         until_true 15 team_has $ns admins alice
builder() { [ "$(user_status $ns-builder)" = 200 ] && gitea_api "" "$o/actions/secrets" | grep -q REGISTRY_TOKEN; }
check "a build account, with its token"    until_true 20 builder
check "a group in Open WebUI"              until_true 20 has_group $ns
check "a channel alice sees"               until_true 20 sees_channel alice $ns
check "but bob, not in it, does not"       not sees_channel bob crud
check "and garrison's operator, which bob may use too" until_true 20 sees_model bob garrison/operator
check "alice opens her dashboard"          until_true 60 home_login alice
check "whose feed says crud is set up"     until_true 20 in_feed alice ns=$ns "e['kind'] == 'unit' and e['actor'] == 'alice'"
check "bob sees the unit"             grep -q '"username":"crud"' <<<"$(gitea_api bob "$api/orgs?limit=50")"
check "and reads its config"               test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/repos/$ns/config/raw/members.yaml")" = 200
check "alice changes its description"      test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"description":"Changed by the crud test"}' "$o")" = 200
check "it reads back changed"              grep -q '"description":"Changed by the crud test"' <<<"$(gitea_api bob "$o")"
check "bob cannot change it"               test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"description":"bob"}' "$o")" = 403

echo "# its people"
msha=$(gitea_api "" "$api/repos/$ns/config/contents/members.yaml" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha"])')
check "nobody changes members.yaml without a pull request" test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d '{"content":"YWRtaW5zOiBbYWxpY2VdCm1lbWJlcnM6IFtib2JdCg==","message":"direct","sha":"'"$msha"'"}' "$api/repos/$ns/config/contents/members.yaml")" = 403
check "bob may not push to its repos yet"  not can_push_to bob crud/config
check "alice adds bob as a member"         change_config $ns "" members.yaml="$(put_file m1 'admins: [alice]\nmembers: [bob]\n')"
check "he joins its members"               until_true 120 team_has $ns members bob
pushes() { can_push_to bob crud/config; }
check "and may push to its repos"          until_true 60 pushes
check "he sees its channel now"            until_true 120 sees_channel bob $ns
check "alice makes him an admin"           change_config $ns "" members.yaml="$(put_file m2 'admins: [alice, bob]\nmembers: []\n')"
check "he joins its admins"                until_true 120 team_has $ns admins bob
not_member() { ! team_has $ns members bob; }
check "and leaves its members"             until_true 60 not_member
check "members.yaml naming nobody as admin fails the check" refused_by_check $ns "" members.yaml="$(put_file m3 'admins: []\nmembers: [bob]\n')"
check "alice removes him"                  change_config $ns "" members.yaml="$(put_file m4 'admins: [alice]\nmembers: []\n')"
not_admin() { ! team_has $ns admins bob; }
check "he leaves its admins"               until_true 120 not_admin
no_push() { ! can_push_to bob crud/config; }
check "and may no longer push"             until_true 60 no_push
no_channel() { ! sees_channel bob $ns; }
check "nor see its channel"                until_true 120 no_channel
check "alice is still its admin"           team_has $ns admins alice

echo "# a repo"
check "alice makes the repo notes"         test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"notes","auto_init":true,"default_branch":"main"}' "$o/repos")" = 201
check "bob reads it"                       test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/repos/$ns/notes")" = 200
check "but cannot push to it"              not can_push_to bob crud/notes
check "alice makes it private"             test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"private":true}' "$api/repos/$ns/notes")" = 200
check "bob no longer sees it"              test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/repos/$ns/notes")" = 404
check "alice makes it public again"        test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"private":false}' "$api/repos/$ns/notes")" = 200
check "bob sees it again"                  test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/repos/$ns/notes")" = 200
check "alice makes the repo scratch"       test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"scratch","auto_init":true,"default_branch":"main"}' "$o/repos")" = 201
check "bob cannot delete it"               test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X DELETE "$api/repos/$ns/scratch")" = 403
check "alice deletes it"                   test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$api/repos/$ns/scratch")" = 204
check "it is gone"                         test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/$ns/scratch")" = 404

echo "# main of a repo, alone and with others"
notes=$api/repos/$ns/notes
# put_main NAME: alice writes the file NAME onto notes' main directly; prints the status.
put_main() { gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"branch":"main","message":"'"$1"'","content":"bm90ZQo="}' "$notes/contents/$1.txt"; }
check "alone in crud, alice pushes to notes' main" test "$(put_main alone)" = 201
check "alice adds bob as a member"         change_config $ns "" members.yaml="$(put_file m5 'admins: [alice]\nmembers: [bob]\n')"
refused_main() { [ "$(put_main "shared-$(date +%s%N)")" != 201 ]; }
check "with two people, main takes no direct push" until_true 60 refused_main
check "alice proposes a change instead"    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"branch":"main","new_branch":"tidy","message":"Tidy","content":"bm90ZQo="}' "$notes/contents/tidy.txt")" = 201
npr=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"tidy","base":"main","title":"Tidy"}' "$notes/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])')
nmerge() { gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$notes/pulls/$npr/merge"; }
check "she cannot merge it unapproved"     test "$(nmerge)" != 200
check "nor approve it herself"             test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED"}' "$notes/pulls/$npr/reviews")" != 200 -o "$(nmerge)" != 200
check "bob approves it"                    test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Fine."}' "$notes/pulls/$npr/reviews")" = 200
check "then alice merges it"               test "$(nmerge)" = 200
check "the merge deletes its branch"        test "$(gitea_api alice -o /dev/null -w '%{http_code}' "$notes/branches/tidy")" = 404
check "other branches stay free"           test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"branch":"main","new_branch":"more","message":"more","content":"bm90ZQo="}' "$notes/contents/more.txt")" = 201
check "alice removes bob again"            change_config $ns "" members.yaml="$(put_file m6 'admins: [alice]\nmembers: []\n')"
free_main() { [ "$(put_main "alone-$(date +%s%N)")" = 201 ]; }
check "alone again, she pushes to main freely" until_true 60 free_main

echo "# an agent"
# The agents are garrison's: alice, a root admin, adds one there, and crud
# gives it rights and a routine in its own config.
agent_yaml() { put_file "$1" "description: $2\ntools: [list_stacks, read_file]\n${3:-}"; }
prompt=$(put_file prompt.md 'You help with the notes of a unit.\n')
check "an agent without its prompt fails the check" refused_by_check garrison "" agents/helper/agent.yaml="$(agent_yaml a0 'Helps with notes')"
check "alice adds the agent helper to garrison" change_config garrison "" agents/helper/agent.yaml="$(agent_yaml a1 'Helps with notes')" agents/helper/prompt.md="$prompt"
check "Open WebUI has it, as helper, for bob too" until_true 120 sees_model bob garrison/helper
check "alice changes its description"      change_config garrison "" agents/helper/agent.yaml="$(agent_yaml a2 'Helps with the notes, now')"
check "Open WebUI shows the new one"       until_true 120 model_says garrison/helper 'Helps with the notes, now'
check "an agent folder in crud fails the check" refused_by_check $ns "" agents/helper/agent.yaml="$(agent_yaml a3 'Mine')" agents/helper/prompt.md="$prompt"
check "it says agents are garrison's"      grep -q "agents are defined in garrison/config only" "$tmp/refused.out"
check "a grant on config fails the check"  refused_by_check $ns "" grants.yaml="$(put_file g3 '- { agent: helper, repo: config, level: propose }\n')"
check "a grant for an agent garrison lacks fails" refused_by_check $ns "" grants.yaml="$(put_file g0 '- { agent: nobody, repo: notes, level: propose }\n')"
check "alice grants it notes in crud"      change_config $ns "" grants.yaml="$(put_file g4 '- { agent: helper, repo: notes, level: propose }\n')"
account() { [ "$(user_status agent-$ns-helper)" = 200 ] && unlocked agent-$ns-helper; }
check "it gets an account of its own in crud" until_true 120 account
check "which may write to notes"           until_true 60 collaborator $ns/notes agent-$ns-helper
check "and to nothing else"                not collaborator crud/config agent-crud-helper
check "alice makes notes private"          test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"private":true}' "$api/repos/$ns/notes")" = 200
# In crud the helper now works on a private repo: it may reach the
# registries, no more.
check "alice lets the helper reach the whole internet" change_config garrison "" agents/helper/agent.yaml="$(agent_yaml a6 'Helps with the notes, now' 'internet: all\n')"
check "then crud's grant on the private repo fails the check" \
  refused_by_check $ns "" grants.yaml="$(put_file g6 '# notes\n- { agent: helper, repo: notes, level: propose }\n')"
check "it says the registries are the most" grep -q "internet: packages), no more" "$tmp/refused.out"
check "the package registries it may reach" change_config garrison "" agents/helper/agent.yaml="$(agent_yaml a7 'Helps with the notes, now' 'internet: packages\n')"
check "a routine every five minutes fails the check" \
  refused_by_check $ns "" routines.yaml="$(put_file r8 '- { agent: helper, name: often, schedule: "*/5 * * * *", owner: alice, task: Look. }\n')"
check "it says how often it may run"       grep -q "runs more often than every 10m" "$tmp/refused.out"
check "a routine at 6:30 every morning passes" change_config $ns "" routines.yaml="$(put_file r9 '- { agent: helper, name: morning, schedule: "30 6 * * *", owner: alice, task: Sum up the notes. }\n')"
# The agent service keeps when it saw it in Postgres, so a restart does not
# forget a run that is due.
kept() { kubectl --namespace garrison exec -i deployment/postgres -- psql -U postgres -d garrison -tAc "SELECT last IS NOT NULL FROM routine_state WHERE key = '$ns/helper/morning'" | grep -qx t; }
check "the agent service keeps its state"  until_true 150 kept
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"private":false}' "$api/repos/$ns/notes"
check "alice takes its grant and routine away" change_config $ns "" grants.yaml=- routines.yaml=-
no_collab() { ! collaborator $ns/notes agent-$ns-helper; }
check "its account may no longer write"    until_true 120 no_collab
check "and is locked"                      until_true 60 locked agent-$ns-helper
check "alice removes the agent from garrison" change_config garrison "" agents/helper/agent.yaml=- agents/helper/prompt.md=-
gone_model() { ! sees_model alice garrison/helper; }
check "Open WebUI no longer has it"        until_true 120 gone_model
check "the operator stays"                 sees_model bob garrison/operator

echo "# an app"
stack_yaml() { put_file "$1" "entrypoint: web\nservices:\n  web:\n    image: traefik/whoami:v1.11\n    port: 80\n    env: { WHOAMI_NAME: $2 }\n    secrets: [token]\n    volumes: { data: { dataset: data, size: $3, path: /data } }\nenvironments:\n  main: {}\n"; }
ds=garrison/ns/$ns/stacks/crud
check "a stack that does not parse fails the check" refused_by_check $ns "" stacks/crud.yaml="$(put_file s0 'entrypoint: web\n')"
check "alice adds the app crud"            change_config $ns "" stacks/crud.yaml="$(stack_yaml s1 first 1Gi)"
check "the deployer deploys it"            until_true 300 applied_app $ns/crud@main
check "it answers at crud.<domain>"        until_true 120 says crud 'Name: first'
check "it claims its hostname"             claimed crud
check "with a dataset of its own"          test "$(zfs get -H -o value quota $ds/data)" = 1G
check "and a generated secret"             app_secret crud token
check "alice changes its setting and size" change_config $ns "" stacks/crud.yaml="$(stack_yaml s2 second 2Gi)"
check "it is deployed again"               until_true 300 says crud 'Name: second'
check "its dataset grows"                  until_true 60 bash -c '[ "$(zfs get -H -o value quota '$ds'/data)" = 2G ]'
check "its data was snapshotted first"     bash -c "zfs list -H -t snapshot -o name -d 1 $ds | grep -q '@deploy-'"
check "a restore of an app that does not exist fails the check" refused_by_check $ns "" restores/nothing.yaml="$(put_file r0 'stack: nothing\nsnapshot: deploy-1\n')"
check "alice removes the app"              change_config $ns "" stacks/crud.yaml=-
down() { ! stack_up crud; }
check "the deployer takes it down"         until_true 180 down
check "it is no longer recorded"           not applied_app crud/crud@main
released() { ! claimed crud; }
check "its hostname is free again"         until_true 60 released
check "its data is kept"                   zfs list $ds/data
check "and its secret"                     app_secret crud token

echo "# taking the unit away"
check "alice adds the app keep"            change_config $ns "" stacks/keep.yaml="$(stack_yaml k1 kept 1Gi)"
check "it is deployed"                     until_true 300 says keep 'Name: kept'
check "and an agent with a grant there"    change_config $ns "" grants.yaml="$(put_file k2 '- { agent: operator, repo: notes, level: propose }\n')"
keeper() { [ "$(user_status agent-$ns-operator)" = 200 ] && unlocked agent-$ns-operator; }
check "with its account"                   until_true 120 keeper
check "bob cannot delete the unit"    test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X DELETE "$o")" = 403
for r in notes config; do gitea_api alice -o /dev/null -X DELETE "$api/repos/$ns/$r"; done
check "alice deletes its repos and the unit" test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$o")" = 204
check "it is gone"                         test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$o")" = 404
keep_down() { ! stack_up keep; }
check "the deployer takes its app down"    until_true 180 keep_down
keep_free() { ! claimed keep; }
check "and frees its hostname"             until_true 60 keep_free
check "its data is kept"                   zfs list garrison/ns/$ns/stacks/keep/data
no_ch() { ! sees_channel alice $ns; }
check "its channel is removed"             until_true 120 no_ch
no_group() { ! has_group $ns; }
check "and its group"                      until_true 120 no_group
no_builder() { [ "$(user_status $ns-builder)" = 404 ]; }
check "its build account is removed"       until_true 120 no_builder
check "the agents' accounts there are locked" until_true 120 locked agent-$ns-operator

echo "# the unit made again"
check "alice makes crud again"             test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"crud","visibility":"limited"}' "$api/orgs")" = 201
check "it gets a fresh config"             until_true 180 first_admin
check "without the old grants"             test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/$ns/config/contents/grants.yaml")" = 404
check "and a new build account"            until_true 120 builder
check "the old agent's account stays locked" locked agent-$ns-operator
check "the old app is not back"            not stack_up keep
}
