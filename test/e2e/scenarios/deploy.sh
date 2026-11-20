# shellcheck shell=bash
# deploy: a unit with an app of its own, built and deployed by its
# pipeline on the user runner. Leaves todo/app running.
section_deploy() {
echo "# deploying an app through its pipeline"
repo=$api/repos/todo/app
for u in alice bob; do sso_login "$tmp/git-$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/git-$u.html" $u >/dev/null 2>&1; done
check "bob cannot create a unit" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"bobs-unit"}' "$api/orgs")" = 403
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"todo","visibility":"limited","description":"Our todo list"}' "$api/orgs")
check "alice creates the unit todo" bash -c "[ $code = 201 ] || [ $code = 422 ]"
has_build_account() { gitea_api "" "$api/orgs/todo/actions/secrets" | grep -q '"name":"REGISTRY_TOKEN"'; }
check "the unit gets its build account" until_true 150 has_build_account
check "its build account is a member"     test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/todo/members/todo-builder")" = 204
# The app is the template todo, as create_repo writes it for todo/app, and
# its stack the one its README tells the agents to propose.
render_todo todo app "$tmp/todo-app"
check "the todo app's source goes to todo/app" installer/push-repo.sh --update todo/app "$tmp/todo-app"
# A config repo is there once its files are: members.yaml, on main.
has_config() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/${1:-todo}/config/raw/members.yaml?ref=main")" = 200 ]; }
check "todo has its config"            until_true 120 has_config
# The talk step's quotes app, which fails for a missing setting: pushed now,
# while alice is todo's only person. Once it has more, the members sync
# protects main of every repo, and a push right after making the repo would
# race it.
check "the quotes app's source goes to todo/quotes" installer/push-repo.sh --update todo/quotes test/e2e/apps/quotes
check "alice adds its stack to todo's config" set_stack todo todo "$tmp/todo-app.stack.yaml"
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
ns_channel() { web -H "Authorization: Bearer $chat_admin" "$chat/api/v1/channels/" | grep -q '"name":"todo"'; }
check "todo has its channel in Open WebUI, where the agents work in it" until_true 120 ns_channel

echo "# a dev environment"
check "alice creates the branch dev" \
  make_branch alice "$repo" dev
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
check "the reserved hostname"              in_feed alice view=problems "'reserved' in e['title'] and e['unit'] == 'todo'"
check "dev-todo taken down"                in_feed alice ns=todo "e['kind'] == 'undeploy' and 'dev-todo' in e['title']"
check "the system stacks' deploys, for everyone" in_feed alice "" "e['kind'] == 'deploy' and e['unit'] == '' and 'traefik' in e['title']"
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
check "the deployer tells Prometheus how each service does" grep -q 'garrison_service_replicas_running{unit="todo",app="todo",environment="main",stack="todo",service="todo_web"} 1' <<<"$(health)"
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
