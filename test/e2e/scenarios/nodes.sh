# shellcheck shell=bash
# nodes: a second machine, joined by test/vm/steps.sh with `install.sh
# --join` and a larger pool than the main one's, runs an app with data: its
# dataset on its own pool (OpenEBS ZFS LocalPV), snapshots and a restore
# there, its route through Traefik on the main machine, and a move to the
# main machine and back, with its data. The platform stays on the main one
# (docs/nodes.md). Runs on the main machine.
section_nodes() {
echo "# a second machine"
local node main
main=$(kubectl get nodes --selector node-role.kubernetes.io/control-plane --output jsonpath='{.items[0].metadata.name}')
node=$(kubectl get nodes --selector '!node-role.kubernetes.io/control-plane' --output jsonpath='{.items[0].metadata.name}')
check "a second machine joined"            test -n "$node"
ready() { kubectl wait --for=condition=Ready "node/$node" --timeout 5s >/dev/null 2>&1; }
check "it is ready"                        until_true 180 ready
# on_node COMMAND...: runs COMMAND on the second machine, in the zfs pod
# there (garrison/openebs), in the host's namespaces.
on_node() {
  local pod
  pod=$(kubectl --namespace garrison get pods --selector app=zfs --field-selector "spec.nodeName=$node" --output name) || return 1
  kubectl --namespace garrison exec -i "$pod" -- nsenter -t 1 -m -u -i -n -- "$@"
}
# pods_on NODE: the namespace/name of each pod there and what made it.
pods_on() {
  kubectl get pods --all-namespaces --field-selector "spec.nodeName=$1,status.phase=Running" \
    --output jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.metadata.ownerReferences[0].kind}{"\n"}{end}'
}
collectors() {
  local p
  for p in zfs openebs-zfs-localpv-node node-exporter alloy sandbox-image; do
    pods_on "$node" | grep -q "^garrison/$p-" || { echo "$p does not run there"; return 1; }
  done
}
check "what runs on every machine runs there" until_true 300 collectors
only_daemons() {
  local bad
  bad=$(pods_on "$node" | grep '^garrison/' | grep -v ' DaemonSet$') || true
  [ -z "$bad" ] || { echo "$bad"; return 1; }
}
check "the platform does not run there"    only_daemons
check "Sanoid snapshots its pool"          on_node systemctl is-enabled sanoid.timer

echo "# an app with data goes to the machine with the most free space"
ds=garrison/ns/todo/stacks/notes
put_notes() { printf 'entrypoint: web\nservices:\n  web:\n    image: nginx:1.29-alpine\n    port: 80\n    env: { NOTE: "%s" }\n    volumes: { html: { dataset: html, size: 1Gi, path: /usr/share/nginx/html } }\nenvironments:\n  main: {}\n' "$1" > "$tmp/notes.yaml"; }
put_notes first
check "alice adds the app notes"           set_stack todo notes "$tmp/notes.yaml"
applied_app() { garrison applied | grep -q "^app $1 "; }
check "the deployer deploys it"            until_true 300 applied_app todo/notes@main
web_node() { kubectl --namespace app-notes get pods --selector garrison.io/service=web --field-selector status.phase=Running --output jsonpath='{.items[0].spec.nodeName}'; }
on_second() { [ "$(web_node)" = "$node" ]; }
check "it runs on the second machine"      until_true 240 on_second
check "its dataset is on that machine's pool" on_node zfs list "$ds/notes.html"
check "with its size as quota"             test "$(on_node zfs get -H -o value quota "$ds/notes.html")" = 1G
check "not on the main machine's"          bash -c "! zfs list $ds/notes.html >/dev/null 2>&1"
write() { printf '%s\n' "$1" | in_app notes web sh -c 'cat > /usr/share/nginx/html/index.html'; }
check "alice's page goes into its data"    write 'kept on the second machine'
says_notes() { [ "$(req notes "https://notes.$DOMAIN/" 2>/dev/null)" = "$1" ]; }
check "Traefik on the main machine reaches it" until_true 60 says_notes 'kept on the second machine'
# Another app's pod, on the main machine, does not get past its policy.
ip=$(kubectl --namespace app-notes get pods --selector garrison.io/service=web --output jsonpath='{.items[0].status.podIP}')
cat > "$tmp/probe.yaml" <<YAML
apiVersion: v1
kind: Pod
metadata: { name: probe, namespace: app-todo }
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  nodeSelector: { node-role.kubernetes.io/control-plane: "true" }
  containers: [{ name: probe, image: busybox:1.37, command: [sh, -c, "wget -q -T 5 -O - http://$ip/ && echo REACHED || echo BLOCKED"] }]
YAML
kubectl apply -f "$tmp/probe.yaml" >/dev/null
probe_done() { [ "$(kubectl --namespace app-todo get pod probe -o jsonpath='{.status.phase}')" = Succeeded ]; }
until_true 120 probe_done
check "another app cannot reach it"        bash -c "kubectl --namespace app-todo logs probe | grep -qx BLOCKED"
kubectl --namespace app-todo delete pod probe --wait=false >/dev/null 2>&1

echo "# its snapshots and a restore, on the second machine"
put_notes second
check "alice changes its setting"          set_stack todo notes "$tmp/notes.yaml"
snapped() { on_node zfs list -H -t snapshot -o name -d 1 "$ds" | grep -q '@deploy-'; }
check "the deployer snapshots its data there first" until_true 300 snapped
redeployed() { [ "$(kubectl --namespace app-notes get deployment web -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="NOTE")].value}')" = second ] && on_second; }
check "and deploys it again, on the same machine" until_true 300 redeployed
snap=$(on_node zfs list -H -t snapshot -o name -s creation -d 1 "$ds" | grep '@deploy-' | tail -n1 | cut -d@ -f2)
listed() { python3 -c 'import json,sys; sys.exit(0 if any(s["name"] == sys.argv[1] for s in json.load(open("/var/lib/garrison/deployer/snapshots.json"))["notes"]["snapshots"]) else 1)' "$snap" 2>/dev/null; }
check "the deployer lists it, for list_snapshots" until_true 180 listed
until_true 120 says_notes 'kept on the second machine'
check "alice overwrites her page by mistake" write 'oops'
printf 'stack: notes\nenvironment: main\nsnapshot: %s\n' "$snap" > "$tmp/restore.yaml"
check "she asks to restore it, a root admin approves" set_config todo restores/notes.yaml "$tmp/restore.yaml"
restored() { [ "$(on_node zfs get -H -o value garrison:restored "$ds")" != - ]; }
check "the deployer restores it there"     until_true 300 restored
check "her page is back"                   until_true 180 says_notes 'kept on the second machine'

echo "# the second machine restarts"
on_node systemctl reboot >/dev/null 2>&1 || true
down() { ! ready; }
until_true 120 down
check "it comes back"                      until_true 300 ready
check "and so does the app, with its data" until_true 300 says_notes 'kept on the second machine'
check "still on that machine"              on_second

echo "# it moves to the main machine, with its data, and back"
# A move request in todo's config: the deployer stops notes, sends its
# datasets to the other machine (zfs send), points its volumes there and
# starts it there; the old copy stays, renamed.
on_main() { [ "$(web_node)" = "$main" ]; }
# moved_ok STACK MACHINE: the feed has STACK's move to MACHINE, done.
moved_ok() { in_feed alice ns=todo "e['kind'] == 'move' and e['status'] == 'ok' and e['title'].startswith('moved $1 ') and 'to $2' in e['title']"; }
printf 'stack: notes\nenvironment: main\nto: %s\n' "$main" > "$tmp/move.yaml"
check "alice asks to move notes to the main machine" set_config todo moves/notes.yaml "$tmp/move.yaml"
check "the feed says it moved"             until_true 300 moved_ok notes "$main"
check "it runs on the main machine"        until_true 240 on_main
check "its dataset is on the main machine's pool" zfs list "$ds/notes.html"
check "with its size as quota"             test "$(zfs get -H -o value quota "$ds/notes.html")" = 1G
check "and its snapshots"                  bash -c "zfs list -H -t snapshot -o name -d 1 $ds | grep -q '@deploy-'"
check "her page came along"                until_true 180 says_notes 'kept on the second machine'
old_kept() { on_node zfs list -H -o name | grep -q "^$ds\.moved-[0-9-]*/notes.html$"; }
check "the old copy stays there, renamed"  old_kept
check "alice writes to it there"           write 'moved to the main machine'
check "and reads it back"                  until_true 60 says_notes 'moved to the main machine'
printf 'stack: notes\nenvironment: main\nto: %s\n' "$node" > "$tmp/move.yaml"
check "she moves it back"                  set_config todo moves/notes.yaml "$tmp/move.yaml"
check "the feed says it moved back"        until_true 300 moved_ok notes "$node"
check "it runs on the second machine again" until_true 240 on_second
check "with what she wrote on the main one" until_true 180 says_notes 'moved to the main machine'

echo "# the todo app moves to the second machine, with its Postgres database"
# todo runs since deploy on the main machine, its database (Postgres) on
# the dataset todo-db, with alice's todo of then. It must stop cleanly and
# start from the copy, with every row.
todo_api() { web -b "$tmp/todo.jar" -H "Origin: https://todo.$DOMAIN" -H 'Content-Type: application/json' "$@"; }
todo_login() {
  sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
  [ "$(todo_api -o /dev/null -w '%{http_code}' "https://todo.$DOMAIN/api/todos")" = 200 ]
}
check "alice opens todo"                   until_true 120 todo_login
add_todo() { todo_api -X POST -d "{\"title\":\"$1\"}" "https://todo.$DOMAIN/api/todos" | grep -q created_by; }
has_todo() { todo_api "https://todo.$DOMAIN/api/todos" | grep -qF "\"title\":\"$1\""; }
check "her todo of before is there"        has_todo 'Water the plants'
check "she adds one just before the move"  add_todo 'Move the database'
db_node() { kubectl --namespace app-todo get pods --selector garrison.io/service=db --field-selector status.phase=Running --output jsonpath='{.items[0].spec.nodeName}'; }
check "its database runs on the main machine" test "$(db_node)" = "$main"
tds=garrison/ns/todo/stacks/todo
printf 'stack: todo\nenvironment: main\nto: %s\n' "$node" > "$tmp/move-todo.yaml"
check "alice asks to move todo to the second machine" set_config todo moves/todo.yaml "$tmp/move-todo.yaml"
check "the feed says it moved"             until_true 600 moved_ok todo "$node"
db_there() { [ "$(db_node)" = "$node" ]; }
check "its database runs there"            until_true 300 db_there
check "its dataset is on that machine's pool" on_node zfs list "$tds/todo.todo-db"
check "not on the main machine's any more" bash -c "! zfs list $tds/todo.todo-db >/dev/null 2>&1"
check "every todo came along"              until_true 180 has_todo 'Water the plants'
check "the last one too"                   has_todo 'Move the database'
check "she adds one there"                 add_todo 'Settled in'
check "and it is kept"                     has_todo 'Settled in'
check "Postgres started cleanly from the copy" bash -c "! kubectl --namespace app-todo logs deployment/db --tail=200 2>&1 | grep -qiE 'PANIC|corrupt|invalid record length|could not locate a valid checkpoint'"

echo "# the Storage dashboard: every machine's pool and data"
# The zfs pod on each machine writes its pools' and datasets' sizes for
# node-exporter (garrison/openebs/zfs-metrics.sh); Grafana's Storage
# dashboard shows them.
prom() { grafana_api "" "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$1")"; }
# prom_has QUERY: Prometheus answers QUERY with at least one series.
prom_has() { prom "$1" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["data"]["result"] else 1)'; }
check "Prometheus has the main machine's pool size" until_true 240 prom_has "garrison_zfs_pool_size_bytes{machine=\"$main\",pool=\"garrison\"} > 0"
check "and the second machine's"           until_true 240 prom_has "garrison_zfs_pool_size_bytes{machine=\"$node\",pool=\"garrison\"} > 0"
check "todo's database, on the second machine, with its quota" until_true 240 prom_has "garrison_zfs_dataset_quota_bytes{machine=\"$node\",dataset=\"$tds/todo.todo-db\",unit=\"todo\",kind=\"app\"} > 0"
check "the old copy from the move, on the main machine" until_true 240 prom_has "garrison_zfs_dataset_used_bytes{machine=\"$main\",kind=\"moved\",unit=\"todo\"}"
check "the pools are healthy"              prom_has "min(garrison_zfs_pool_healthy) == 1"
check "Grafana has the Storage dashboard"  test "$(grafana_api "" -o /dev/null -w '%{http_code}' "$g/api/dashboards/uid/garrison-storage")" = 200
}
