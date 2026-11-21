# shellcheck shell=bash
# platform: the deployer, monitoring data, app stacks and private environments.
section_platform() {
echo "# deployer"

check "the deployer runs in Swarm"       test "$(docker service ls --filter name=deployer --format '{{.Replicas}}')" = 1/1
check "it runs an image built in Gitea"    bash -c "case '$(deployer_image)' in git.$DOMAIN/garrison/control-plane:bootstrap) exit 1 ;; git.$DOMAIN/garrison/control-plane:*) exit 0 ;; *) exit 1 ;; esac"
tag=$(deployer_image | sed 's/.*://')
check "the image is in Gitea's registry"   test "$(json -o /dev/null -w '%{http_code}' "$api/packages/garrison/container/control-plane/$tag")" = 200
check "its build succeeded in Gitea Actions" grep -q '"status":"success"' <<<"$(json "$api/repos/garrison/control-plane/actions/tasks?limit=1")"
check "the runner is up"                 test "$(docker service ls --filter name=runner --format '{{.Replicas}}')" = 1/1
check "bob cannot see the build's secrets" test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/orgs/garrison/actions/secrets")" = 403
check "the deployer no longer runs on the host" bash -c '! systemctl is-active --quiet garrison-controller'
check "the deployer deployed grafana, prometheus and loki" \
  bash -c 'a=$(garrison applied | cut -d" " -f1); for n in grafana prometheus loki; do grep -qx $n <<<"$a" || exit 1; done'

docker stack rm smoke >/dev/null 2>&1 || true
gitea_api "" -o /dev/null -X DELETE "$api/repos/garrison/smoke" 2>/dev/null || true
compose=$(printf 'services:\n  web:\n    image: traefik/whoami:v1.11\n' | base64 -w0)
check "bob cannot add a repo to the garrison organisation" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bobs-stack"}' "$api/orgs/garrison/repos")" = 403
check "alice adds a repo to the garrison organisation" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"smoke","auto_init":true,"default_branch":"main"}' "$api/orgs/garrison/repos")" = 201
check "alice gives it a compose.yaml"      test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"message":"smoke test","branch":"main","content":"'$compose'"}' "$api/repos/garrison/smoke/contents/compose.yaml")" = 201
check "the deployer deploys it as the stack smoke" until_true 120 smoke_running
check "alice deletes the repo"             test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$api/repos/garrison/smoke")" = 204
check "the deployer takes the stack down" until_true 120 smoke_gone
check "the other system stacks are untouched" bash -c 'l=$(docker stack ls --format "{{.Name}}"); grep -qx grafana <<<"$l" && grep -qx gitea <<<"$l" && grep -qx control-plane <<<"$l"'

echo "# monitoring data"
check "Prometheus scrapes the node"        poll 180 '"job":"node"[^]]*\},"value":\[[0-9.]+,"1"\]' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=up"
check "Prometheus scrapes the containers"  poll 180 '"job":"cadvisor"[^]]*\},"value":\[[0-9.]+,"1"\]' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=up"
check "container metrics name their Swarm service" poll 180 'gitea' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=count%20by%20(container_label_com_docker_swarm_service_name)(container_memory_usage_bytes)"
check "Loki has the logs of the Gitea stack" poll 180 '"values":\[\[' "$g/api/datasources/proxy/uid/loki/loki/api/v1/query_range?query=%7Bstack%3D%22gitea%22%7D&limit=1"
check "Prometheus is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' https://prometheus.$DOMAIN/)" = 404
check "Loki is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' https://loki.$DOMAIN/)" = 404
check "Prometheus and Loki publish no port on the host" bash -c '! (ss -ltn | grep -Eq ":(9090|3100) ")'

echo "# dashboards from Git"
folder_count() { grafana_api "" "$g/api/search?type=dash-db&folderUIDs=ns-garrison&tag=git" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'; }
six_dashboards() { [ "$(folder_count)" -ge 6 ]; }
check "the garrison folder holds the dashboards from Git" until_true 180 six_dashboards
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
for f in sorted(glob.glob("garrison/grafana/dashboards/*.json")):
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
gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"message":"Add a dashboard","branch":"main","content":"'$smoke_dash'"}' "$api/repos/garrison/platform/contents/dashboards/smoke.json"
dash_there() { test "$(grafana_api "" -o /dev/null -w '%{http_code}' "$g/api/dashboards/uid/smoke-test")" = 200; }
dash_gone()  { ! dash_there; }
check "a dashboard committed by alice appears" until_true 120 dash_there
sha=$(gitea_api alice "$api/repos/garrison/platform/contents/dashboards/smoke.json" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)
gitea_api alice -o /dev/null -X DELETE -H 'Content-Type: application/json' -d '{"message":"Remove a dashboard","branch":"main","sha":"'$sha'"}' "$api/repos/garrison/platform/contents/dashboards/smoke.json"
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
# The model list in garrison's config: written from the install's LLM, then
# kept current by the model sync's pull requests. A fake provider, a static
# file served on this machine, stands in for OpenRouter and the like.
sys=$api/repos/garrison/config
llm_file() { gitea_api "" "$sys/raw/llm/$1?ref=${2:-main}"; }
seeded() { llm_file providers.yaml | grep -q '^default: ' && llm_file models.yaml | grep -q 'enabled: true'; }
check "garrison's config gets the model list" until_true 180 seeded
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
check "alice adds a provider; another root admin approves" set_config garrison llm/providers.yaml "$tmp/providers.yaml"

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
check "alice narrows the provider"         set_config garrison llm/providers.yaml "$tmp/providers.yaml"
check "the next sync proposes fake-large as retired" until_true 180 models_pr_with "Retired: fake/fake-large"
check "with the date it went"              grep -qE '"fake/fake-large": \{enabled: false, added: [0-9-]+, gone: [0-9-]+\}' <<<"$(llm_file models.yaml "$(models_branch)")"
kill "$fake_pid" 2>/dev/null || true

echo "# an LLM provider and its key, on the dashboard's Models page"
# models_post WHO FIELD=VALUE...: the page's form, as WHO; prints the status and where it leads.
models_post() {
  local who=$1 a=(); shift
  for f in "$@"; do a+=(--data-urlencode "$f"); done
  web -b "$tmp/home-$who.jar" -o /dev/null -w '%{http_code} %{redirect_url}' -H "Origin: https://$DOMAIN" -X POST "${a[@]}" "https://$DOMAIN/models"
}
check "alice signs in to her dashboard"    until_true 60 home_login alice
check "bob signs in to his"                until_true 60 home_login bob
check "the Models page is a root admin's"  test "$(web -b "$tmp/home-bob.jar" -o /dev/null -w '%{http_code}' "https://$DOMAIN/models")" = 403
check "bob may not add a provider"         grep -q '^403' <<<"$(models_post bob action=add name=smoke url=http://127.0.0.1:9/v1 key=tok-bob)"
check "nor an app's page, for alice"       test "$(web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code}' -H "Origin: https://hello.$DOMAIN" -X POST -d action=add -d name=smoke -d url=http://127.0.0.1:9/v1 -d key=tok "https://$DOMAIN/models")" = 403
res=$(models_post alice action=add name=smoke url=http://127.0.0.1:9/v1 key=tok-smoke)
landed() { grep -qE "^303 https://$DOMAIN/c/garrison/config/[0-9]+$" <<<"$res" || { echo "got: $res"; return 1; }; }
check "alice adds the provider smoke with its key; she lands on its change" landed
smoke_pr=$(grep -oE '/c/garrison/config/[0-9]+$' <<<"$res" | grep -oE '[0-9]+$')
check "the change adds it to providers.yaml, as alice" python3 -c '
import json,sys
p=json.loads(sys.argv[1]); sys.exit(0 if p["user"]["login"] == "alice" else 1)' "$(gitea_api "" "$api/repos/garrison/config/pulls/${smoke_pr:-0}")"
check "with its URL, without its key"      bash -c 'grep -q "smoke:" <<<"$0" && grep -q "http://127.0.0.1:9/v1" <<<"$0" && ! grep -q tok-smoke <<<"$0"' "$(gitea_api "" "$api/repos/garrison/config/pulls/${smoke_pr:-0}.diff")"
stored=$(gitea_api "" "$api/repos/garrison/config/raw/secrets/llm-smoke.yaml")
check "its key is in garrison's config, encrypted" bash -c 'grep -q "ENC\[" <<<"$0" && ! grep -q tok-smoke <<<"$0"' "$stored"
key_is() { [ "$(cat $ETC/llm-keys/smoke 2>/dev/null)" = "$1" ] && [ "$(stat -c %a $ETC/llm-keys/smoke)" = 600 ]; }
check "the deployer writes it where the agent service reads it, root only" until_true 90 key_is tok-smoke
check "the page lists it, waiting for its change, never its key" bash -c 'grep -q "<code>smoke</code>" <<<"$0" && grep -q "waiting for its change" <<<"$0" && ! grep -q tok-smoke <<<"$0"' "$(feed alice /models)"
gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$api/repos/garrison/config/pulls/${smoke_pr:-0}"
check "alice deletes the key"              grep -q '^303' <<<"$(models_post alice action=delete name=smoke)"
key_gone() { [ ! -e $ETC/llm-keys/smoke ]; }
check "and the deployer removes its file"  until_true 90 key_gone

}
