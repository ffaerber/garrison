# shellcheck shell=bash
# nodes: a second machine, joined by test/vm/steps.sh with `install.sh
# --join` and a larger pool than the main one's, runs an app with data: its
# dataset on its own pool (OpenEBS ZFS LocalPV), snapshots and a restore
# there, its route through Traefik on the main machine. The platform stays on
# the main one (docs/nodes.md). Runs on the main machine.
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
}
