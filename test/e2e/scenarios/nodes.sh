# shellcheck shell=bash
# nodes: a second machine, joined by test/vm/steps.sh with
# `install.sh --join`, runs apps without data; the platform and every service
# with data stay on the main one (docs/nodes.md). Runs on the main machine.
section_nodes() {
echo "# a second machine in the swarm"
local main node
# By ID: the test machines are clones and may share a hostname.
main=$(docker node inspect self --format '{{.ID}}')
node=$(docker node ls --filter role=worker -q | head -n1)
check "a second machine is in the swarm" test -n "$node"
ready() { [ "$(docker node inspect "$node" --format '{{.Status.State}} {{.Spec.Availability}}')" = "ready active" ]; }
check "it is ready for tasks" until_true 120 ready

# on_node SERVICE: the ID of the machine its running task is on.
on_node() {
  local t
  for t in $(docker service ps -q --filter desired-state=running "$1"); do
    docker inspect "$t" --format '{{.NodeID}} {{.Status.State}}'
  done | awk '$2 == "running" { print $1; exit }'
}
runs_on() { [ "$(on_node "$1")" = "$2" ]; }
# only_global: the machine runs nothing but what runs on every machine.
only_global() {
  local t s
  for t in $(docker node ps "$node" --filter desired-state=running --format '{{.Name}}'); do
    s=${t%%.*}
    [ "$(docker service inspect "$s" --format '{{if .Spec.Mode.Global}}global{{end}}')" = global ] || { echo "$s runs on $node"; return 1; }
  done
}
check "the platform does not run there, only its collectors" only_global
has_collectors() { [ "$(docker node ps "$node" --filter desired-state=running --format '{{.Name}}' | grep -cE '^(prometheus_node-exporter|prometheus_cadvisor|loki_alloy)\.')" = 3 ]; }
check "its metrics and logs collectors run there" until_true 180 has_collectors

echo "# an app without data moves there"
# Swarm does not move running tasks; a new task goes where the fewest run,
# which is the new machine.
docker service update --quiet --detach --force todo_web >/dev/null
check "todo's web part runs on the second machine" until_true 240 runs_on todo_web "$node"
check "its image came from the installation's registry" \
  bash -c 'docker service inspect todo_web --format "{{.Spec.TaskTemplate.ContainerSpec.Image}}" | grep -q "^git.$DOMAIN/todo/app:"'
check "todo's database stays on the main machine" runs_on todo_db "$main"
docker service update --quiet --detach --force todo_db >/dev/null
check "and stays there when it restarts" until_true 240 runs_on todo_db "$main"
login_todo() {
  sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
  web -b "$tmp/todo.jar" -H "Origin: https://todo.$DOMAIN" "https://todo.$DOMAIN/api/todos" | grep -q 'Water the plants'
}
check "alice's todos come from the main machine's database through it" until_true 180 login_todo

echo "# the machine leaves"
docker node update --availability drain "$node" >/dev/null
check "its app moves back to the main machine when it is drained" until_true 240 runs_on todo_web "$main"
check "and alice still sees her todos" until_true 120 login_todo
}
