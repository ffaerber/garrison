# shellcheck shell=bash
# platform: the deployer, monitoring data, app stacks and private environments.
section_platform() {
echo "# deployer"

check "the deployer runs in the cluster"  test "$(kubectl --namespace garrison get deployment deployer -o jsonpath='{.status.readyReplicas}')" = 1
check "it runs an image built in Gitea"    bash -c "case '$(deployer_image)' in git.$DOMAIN/garrison/control-plane:bootstrap) exit 1 ;; git.$DOMAIN/garrison/control-plane:*) exit 0 ;; *) exit 1 ;; esac"
tag=$(deployer_image | sed 's/.*://')
check "the image is in Gitea's registry"   test "$(json -o /dev/null -w '%{http_code}' "$api/packages/garrison/container/control-plane/$tag")" = 200
check "its build succeeded in Gitea Actions" grep -q '"status":"success"' <<<"$(json "$api/repos/garrison/control-plane/actions/tasks?limit=1")"
check "the runner is up"                 test "$(kubectl --namespace garrison get deployment runner -o jsonpath='{.status.readyReplicas}')" = 1
check "bob cannot see the build's secrets" test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$api/orgs/garrison/actions/secrets")" = 403
check "the deployer no longer runs on the host" bash -c '! systemctl is-active --quiet garrison-controller'
check "the deployer deployed grafana, prometheus and loki" \
  bash -c 'a=$(garrison applied | cut -d" " -f1); for n in grafana prometheus loki; do grep -qx $n <<<"$a" || exit 1; done'

kubectl delete all --all-namespaces --selector garrison.io/stack=smoke >/dev/null 2>&1 || true
gitea_api "" -o /dev/null -X DELETE "$api/repos/garrison/smoke" 2>/dev/null || true
manifest=$(printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata: { name: smoke }\nspec:\n  selector: { matchLabels: { app: smoke } }\n  template:\n    metadata: { labels: { app: smoke } }\n    spec:\n      automountServiceAccountToken: false\n      containers: [{ name: web, image: "traefik/whoami:v1.11" }]\n' | base64 -w0)
check "bob cannot add a repo to the garrison organisation" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bobs-stack"}' "$api/orgs/garrison/repos")" = 403
check "alice adds a repo to the garrison organisation" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"smoke","auto_init":true,"default_branch":"main"}' "$api/orgs/garrison/repos")" = 201
check "alice gives it a kubernetes.yaml"   test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"message":"smoke test","branch":"main","content":"'$manifest'"}' "$api/repos/garrison/smoke/contents/kubernetes.yaml")" = 201
check "the deployer deploys it as the stack smoke" until_true 120 smoke_running
check "alice deletes the repo"             test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X DELETE "$api/repos/garrison/smoke")" = 204
check "the deployer takes the stack down" until_true 120 smoke_gone
check "the other system stacks are untouched" bash -c "$(declare -f stack_objects); for s in grafana gitea control-plane; do [ -n \"\$(stack_objects \$s)\" ] || exit 1; done"

echo "# monitoring data"
check "Prometheus scrapes the node"        poll 180 '"job":"node"[^]]*\},"value":\[[0-9.]+,"1"\]' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=up"
check "Prometheus scrapes the containers"  poll 180 '"job":"kubelet"[^]]*\},"value":\[[0-9.]+,"1"\]' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=up"
check "container metrics join their stack's service" poll 180 'gitea' "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=count%20by%20(service)%20(container_memory_usage_bytes%7Bcontainer!%3D%22%22%7D%20*%20on%20(namespace%2C%20pod)%20group_left%20(service)%20garrison_pod_info)"
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
check "the stack dashboard shows grafana's services" until_true 120 prom_has 'sum by (service) (rate(container_cpu_usage_seconds_total{container!=""}[5m]) * on (namespace, pod) group_left (service) garrison_pod_info{stack="grafana"})'
check "Prometheus scrapes Traefik"        until_true 180 prom_has 'up{job="traefik"} == 1'
check "the traffic dashboard has requests" until_true 120 prom_has 'sum(rate(traefik_entrypoint_requests_total[5m])) > 0'
check "Traefik's metrics are not reachable from outside" bash -c 'test "$(curl -s -o /dev/null -w "%{http_code}" "http://$(hostname -I | cut -d" " -f1):8082/metrics")" != 200'
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
check "hello got its dataset with a quota" test "$(zfs get -H -o value quota garrison/ns/demo/stacks/hello/hello.hello-data)" = 1G
check "hello's dataset is mounted in the container" \
  bash -c "$(declare -f app_claim); app_claim hello web data-hello-data hello.hello-data /data"
check "hello got a generated secret"       app_secret hello hello-token
check "the CLI deploys hello (dev)"       $CTL apply -domain "$DOMAIN" -env dev -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "dev has its own dataset"            zfs list garrison/ns/demo/stacks/dev-hello/dev-hello.hello-data
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
check "rejected stacks were not deployed"  bash -c '! kubectl get namespace app-git >/dev/null 2>&1'
check "hello still answers after the rejected deploys"   status_is 200 hello / 30

echo "# models"
# The model list in garrison's config: written by the setup, then kept
# current by the model sync. A fake provider, a static
# file served on this machine, stands in for OpenRouter and the like.
sys=$api/repos/garrison/config
llm_file() { gitea_api "" "$sys/raw/llm/$1?ref=${2:-main}"; }
seeded() { llm_file providers.yaml | grep -q '^default: ' && llm_file models.yaml | grep -q 'enabled: true'; }
check "garrison's config has the model list from the setup" until_true 180 seeded
install_model=$(llm_file providers.yaml | sed -n 's#^default: *"\{0,1\}[^/"]*/\([^"]*\)"\{0,1\}.*#\1#p')
check "its default is the setup's model, enabled" \
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
fake_listed() { grep -qF '"fake/fake-small": {enabled: false' <<<"$(llm_file models.yaml)"; }
check "the model sync lists the provider's models on main by itself" until_true 180 fake_listed
listed=$(llm_file models.yaml)
check "they come turned off, only those include names" \
  bash -c 'grep -qF "\"fake/fake-small\": {enabled: false" <<<"$0" && grep -qF "\"fake/fake-large\": {enabled: false" <<<"$0" && ! grep -q other-model <<<"$0"' "$listed"
check "the install's models are kept"      grep -qF "/$install_model\": {enabled: true" <<<"$listed"
check "without a pull request"             test -z "$(models_pr)"
approval_has() { sys_approval "$1" | grep -q "$2"; }
sys_review() { gitea_api "$1" -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$sys/pulls/$2/reviews"; }

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
pr=$(models_pr)
check "it waits for a root admin's approval" until_true 60 approval_has "$pr" "^pending"
sys_review bob "$pr"; sleep 25
check "bob's approval does not count"      approval_has "$pr" "^pending"
sys_review alice "$pr"
check "alice's does"                       until_true 60 approval_has "$pr" "^success"
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$sys/pulls/$pr/merge")" = 200
check "main marks fake-large retired"      grep -qE '"fake/fake-large": \{enabled: false, added: [0-9-]+, gone: ' <<<"$(llm_file models.yaml)"
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

echo "# the models agents use, chosen on the Models page"
page=$(feed alice /models)
check "the page offers every model, off ones marked" bash -c 'grep -qF "<option value=\"fake/fake-small\">fake-small (off)</option>" <<<"$0" && grep -qF "name=\"agent-coder\"" <<<"$0" && ! grep -qF "fake/fake-large\"" <<<"$0"' "$page"
res=$(models_post alice action=choose "default=$(llm_file providers.yaml | sed -n 's/^default: *"\{0,1\}\([^"]*\)"\{0,1\}.*/\1/p')" agent-coder=fake/fake-small)
check "alice puts the coder on fake-small; she lands on its change" landed
choose_pr=$(grep -oE '/c/garrison/config/[0-9]+$' <<<"$res" | grep -oE '[0-9]+$')
check "one change: the coder's agent.yaml, and the model turned on" test "$(gitea_api "" "$sys/pulls/${choose_pr:-0}/files" | python3 -c 'import json,sys; print(" ".join(sorted(f["filename"] for f in json.load(sys.stdin))))')" = "agents/coder/agent.yaml llm/models.yaml"
check "it waits for another root admin: alice wrote it" until_true 90 approval_has "${choose_pr:-0}" "^pending"
check "a model the providers no longer offer is refused" grep -q '^400' <<<"$(models_post alice action=choose agent-coder=fake/fake-large)"
check "bob may not choose"                 grep -q '^403' <<<"$(models_post bob action=choose agent-coder=fake/fake-small)"
gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$sys/pulls/${choose_pr:-0}"

echo "# an agent proposes a skill for the agent it asks; a root admin approves it"
# The coder leads the chain news-page, so it asks the scout. It calls
# propose_skill as the agent service does, in a run that alice owns.
service_key=$(python3 -c 'import hmac,hashlib,sys; print(hmac.new(sys.argv[1].encode(), b"garrison agent service v1", hashlib.sha256).hexdigest())' "$PASSWORD")
# as_coder TOOL ARGS_JSON: the coder's tool call; prints the tool's text.
as_coder() {
  kubectl --namespace garrison exec -i deployment/openwebui -- curl -sS --max-time 30 \
    -H "X-Garrison-Service: $service_key" -H "X-Garrison-Owner: alice" -H "X-Garrison-Started-By: routine garrison/coder/e2e" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":sys.argv[1],"arguments":json.loads(sys.argv[2])}}))' "$1" "$2")" \
    http://gateway:8080/mcp/garrison/coder | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r.get("result",{}).get("content",[{}])[0].get("text","") or r)'
}
skill=$'---\nname: e2e-sources\ndescription: Searching where the first search found nothing.\n---\nSearch in the language of the topic too, and use the sites own search.\n'
skill_args=$(python3 -c 'import json,sys; print(json.dumps({"agent":"scout","skill":"e2e-sources","content":sys.argv[1],"description":"It came back empty twice; searching in German helped."}))' "$skill")
out=$(as_coder propose_skill "$skill_args")
skill_pr=$(grep -oE 'garrison/config#[0-9]+' <<<"$out" | grep -oE '[0-9]+$')
check "the coder proposes a skill for the scout" test -n "$skill_pr" || echo "      it said: $out"
skill_pull=$(gitea_api "" "$sys/pulls/${skill_pr:-0}")
check "opened by the platform's account, on a skill branch, naming the agent and alice" python3 -c '
import json,sys
p=json.loads(sys.argv[1])
ok = p["user"]["login"] == sys.argv[2] and p["head"]["ref"].startswith("skill/scout/e2e-sources-") and "Proposed by the agent garrison/coder, working for alice" in p["body"]
sys.exit(0 if ok else 1)' "$skill_pull" "$ADMIN_USER"
check "it changes only the scout's SKILL.md" test "$(gitea_api "" "$sys/pulls/${skill_pr:-0}/files" | python3 -c 'import json,sys; print(" ".join(f["filename"] for f in json.load(sys.stdin)))')" = "agents/scout/skills/e2e-sources/SKILL.md"
check "the coder may not propose one for an agent it does not ask" grep -q "only for yourself and the agents you ask" <<<"$(as_coder propose_skill '{"agent":"operator","skill":"x","content":"---\nname: x\ndescription: y\n---\n","description":"d"}')"
check "it waits for a root admin's approval" until_true 90 approval_has "${skill_pr:-0}" "^pending"
sys_review alice "${skill_pr:-0}"
check "alice approves it: she did not write it" until_true 60 approval_has "${skill_pr:-0}" "^success"
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$sys/pulls/${skill_pr:-0}/merge")" = 200
check "the scout has the skill on main"    grep -q "name: e2e-sources" <<<"$(gitea_api "" "$sys/raw/agents/scout/skills/e2e-sources/SKILL.md")"
# The feed's Lessons: the proposal, done once merged, and that the scout
# learnt it.
check "the feed's lessons say it proposed"  until_true 60 in_feed alice view=lessons "e['kind'] == 'lesson' and e['agent'] == 'garrison/scout' and e['pr'] == ${skill_pr:-0} and 'may learn the skill e2e-sources' in e['title'] and e['status'] == 'ok'"
check "and that the scout learnt it"       until_true 60 in_feed alice view=lessons "e['kind'] == 'lesson' and e['title'] == 'scout learnt the skill e2e-sources: alice merged #${skill_pr:-0}'"
# Anything else on a skill branch is refused, whoever pushed it.
out=$(as_coder propose_skill "$(sed 's/e2e-sources/e2e-more/g' <<<"$skill_args")")
more_pr=$(grep -oE 'garrison/config#[0-9]+' <<<"$out" | grep -oE '[0-9]+$')
more_branch=$(gitea_api "" "$sys/pulls/${more_pr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["ref"])')
gitea_api "" -o /dev/null -X POST -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import base64,json,sys; print(json.dumps({"branch":sys.argv[1],"message":"more","content":base64.b64encode(b"tools: all\n").decode()}))' "$more_branch")" \
  "$sys/contents/agents/scout/skills/e2e-more/agent.yaml"
check "a skill branch with another file is refused" until_true 90 approval_has "${more_pr:-0}" "^failure.*a skill branch changes only"
gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$sys/pulls/${more_pr:-0}"

# A unit's channel link, which create_unit gives and the commander passes on
# (find_agents with in_unit): the agent service asks the gateway for it.
channel_url() {
  kubectl --namespace garrison exec deployment/openwebui -- curl -sS --max-time 30 -H "X-Garrison-Service: ${2:-$service_key}" \
    "http://gateway:8080/internal/units/$1/channel"
}
check "the gateway links garrison's channel" grep -qE "\"url\":\"https://chat\\.$DOMAIN/channels/[A-Za-z0-9-]+\"" <<<"$(channel_url garrison)"
check "only for the agent service"           grep -q "only for the agent service" <<<"$(channel_url garrison wrong)"

# The agents' web search: SearXNG answers the agent service as JSON (results
# depend on the search engines, so only the shape is checked), and no one
# else reaches it.
searx() {
  kubectl --namespace garrison exec deployment/agents -- wget -qO- -T 30 --header 'X-Real-IP: 127.0.0.1' 'http://searxng:8080/search?q=kubernetes&format=json' |
    python3 -c 'import json,sys; sys.exit(0 if "results" in json.load(sys.stdin) else 1)'
}
check "SearXNG answers the agent service"  until_true 120 searx
check "and no one else"                    bash -c '! kubectl --namespace garrison exec deployment/openwebui -- curl -sS --max-time 5 -o /dev/null http://searxng:8080/healthz 2>/dev/null'

# The scout browses the public web in its own browser, through the agents'
# internet proxy as itself: a public page opens, the home network is
# refused by the proxy, and Garrison's own pages not at all (a page could
# have steered it). Called as in a routine, with nobody watching. as_scout
# URL opens a page; as_scout JSON takes browse's arguments.
as_scout() {
  kubectl --namespace garrison exec -i deployment/openwebui -- curl -sS --max-time 90 \
    -H "X-Garrison-Service: $service_key" -H "X-Garrison-Owner: alice" -H "X-Garrison-Started-By: routine garrison/scout/e2e" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d "$(python3 -c 'import json,sys; a=sys.argv[1]; print(json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browse","arguments":json.loads(a) if a.startswith("{") else {"url":a}}}))' "$1")" \
    http://gateway:8080/mcp/garrison/scout | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r.get("result",{}).get("content",[{}])[0].get("text","") or r)'
}
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
check "the scout opens a public page"      grep -q "Example Domain" <<<"$(as_scout https://example.org/)"
as_scout http://10.43.0.10/ >/dev/null
refused_home() { logs_since "$since" agents | grep -q "internet: browser: garrison/scout: 10.43.0.10:80 refused"; }
check "the proxy refuses it the home network" until_true 30 refused_home
check "and it never opens Garrison's pages" grep -q "does not use Garrison's pages as the person" <<<"$(as_scout "https://$DOMAIN/")"

# A site login: alice adds one of garrison on the dashboard's Secrets page,
# logins.yaml lets the scout use it for admins in routines, and the deployer
# hands it to the gateway. The scout signs in to a fake shop (a pod, let
# through the proxy by INTERNET_ALLOW) without ever seeing the password,
# which goes only into the shop's password field.
cat > "$tmp/shop.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs
FORM = b'<!doctype html><title>Shop</title><h1>Shop</h1><form method="post" action="/login"><label>User <input name="user" type="text"></label> <label>Note <input name="note" type="text"></label> <label>Password <input name="pw" type="password"></label> <button>Sign in</button></form>'
class H(BaseHTTPRequestHandler):
    def do_GET(self): self.reply(FORM)
    def do_POST(self):
        f = parse_qs(self.rfile.read(int(self.headers.get("Content-Length", 0))).decode())
        ok = f.get("user") == ["scout-e2e"] and f.get("pw") == ["pa55-e2e"]
        self.reply(b'<!doctype html><title>Shop</title><p>Welcome, scout-e2e</p>' if ok else b'<!doctype html><title>Shop</title><p>Wrong login</p>')
    def reply(self, body):
        self.send_response(200); self.send_header("Content-Type", "text/html"); self.end_headers(); self.wfile.write(body)
HTTPServer(("", 80), H).serve_forever()
PY
kubectl create namespace e2e-shop >/dev/null 2>&1
kubectl --namespace e2e-shop create configmap shop --from-file=shop.py="$tmp/shop.py" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply --filename - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: shop, namespace: e2e-shop }
spec:
  selector: { matchLabels: { app: shop } }
  template:
    metadata: { labels: { app: shop } }
    spec:
      automountServiceAccountToken: false
      containers:
        - name: shop
          image: python:3.13-alpine
          command: [python3, /shop/shop.py]
          volumeMounts: [{ name: shop, mountPath: /shop }]
      volumes: [{ name: shop, configMap: { name: shop } }]
---
apiVersion: v1
kind: Service
metadata: { name: shop, namespace: e2e-shop }
spec:
  selector: { app: shop }
  ports: [{ port: 80 }]
YAML
kubectl --namespace e2e-shop rollout status deployment/shop --timeout 300s >/dev/null 2>&1
shop_ip=$(kubectl --namespace e2e-shop get service shop --output jsonpath='{.spec.clusterIP}')
sed -i '/^INTERNET_ALLOW=/d' "$ETC/install.env" && echo "INTERNET_ALLOW=$shop_ip/32" >> "$ETC/install.env"
kubectl --namespace garrison rollout restart deployment/agents >/dev/null && kubectl --namespace garrison rollout status deployment/agents --timeout 300s >/dev/null 2>&1
shop_allowed() { kubectl --namespace garrison logs deployment/agents --since=5m 2>&1 | grep -q "also lets through \[$shop_ip/32\]"; }
check "the agents' proxy lets agents reach the shop" until_true 120 shop_allowed
feed alice / >/dev/null
check "alice adds the shop's login"        test "$(web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code}' -H "Origin: https://$DOMAIN" \
  --data-urlencode action=login --data-urlencode login=shop --data-urlencode "site=http://$shop_ip" \
  --data-urlencode username=scout-e2e --data-urlencode password=pa55-e2e "https://$DOMAIN/secrets/garrison")" = 303
printf -- '- login: shop\n  agent: scout\n  for: admins\n  unattended: true\n' > "$tmp/logins.yaml"
check "logins.yaml lets the scout use it"  set_config garrison logins.yaml "$tmp/logins.yaml"
written() { kubectl --namespace garrison exec deployment/gateway -- test -s /etc/garrison/logins/garrison/shop.json; }
check "the deployer hands it to the gateway" until_true 120 written
page=$(as_scout "http://$shop_ip/")
field() { grep -oE "^\[[0-9]+\] input \($1\)" <<<"$page" | head -1 | tr -dc 0-9; }
user=$(field text) pw=$(field password) note=$(grep -ioE '^\[[0-9]+\] input \(text\) "note' <<<"$page" | tr -dc 0-9)
check "the scout sees the shop's form"     test -n "$user" -a -n "$pw" -a -n "$note"
check "a password goes into no other field" grep -q "not a password field" <<<"$(as_scout "{\"login\":\"shop\",\"part\":\"password\",\"type_into\":${note:-0}}")"
as_scout "{\"login\":\"shop\",\"part\":\"username\",\"type_into\":${user:-0}}" > "$tmp/typed"
signed=$(as_scout "{\"login\":\"shop\",\"part\":\"password\",\"type_into\":${pw:-0},\"submit\":true}")
check "the scout signs in with it"         grep -q "Welcome, scout-e2e" <<<"$signed"
unseen() { ! grep -q "pa55-e2e" "$tmp/typed" && ! grep -q "pa55-e2e" <<<"$signed"; }
check "and never sees the password"        unseen
as_scout https://example.org/ >/dev/null
check "the login goes only into its site"  grep -q "typed only on its own site" <<<"$(as_scout '{"login":"shop","type_into":1}')"
kubectl delete namespace e2e-shop --wait=false >/dev/null 2>&1
}
