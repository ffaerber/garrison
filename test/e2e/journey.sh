# journey: an app with code of its own goes from nothing to main, the way
# people do it: alice asks the agents in Open WebUI to make the namespace,
# the repo and the stack, merges in Gitea, has an agent change the page on
# dev, looks at dev, and merges dev into main. Talks to the real LLM; the
# checks look at what happened in Gitea, the registry and the cluster, and
# at the pages themselves. Sourced by smoke.sh, whose helpers it uses.

section_journey() {
echo "# journey: lab/example, from nothing to main, through the agents"
lab=$api/repos/lab
# page NAME: the page at https://NAME.<domain>.
page() { web --resolve "$1.$DOMAIN:443:127.0.0.1" "https://$1.$DOMAIN/"; }
# head_of_lab REPO BRANCH: the commit a branch of lab/REPO is at.
head_of_lab() { gitea_api "" "$lab/$1/branches/$2" | python3 -c 'import json,sys; print(json.load(sys.stdin)["commit"]["id"])'; }
# lab_pr REPO AUTHOR BASE FILE: the newest open pull request of lab/REPO by AUTHOR into BASE that changes FILE.
lab_pr() {
  local n
  for n in $(gitea_api "" "$lab/$1/pulls?state=open&limit=20" | python3 -c '
import json,sys
for p in sorted(json.load(sys.stdin), key=lambda p: -p["number"]):
    if p["user"]["login"] == sys.argv[1] and p["base"]["ref"] == sys.argv[2]: print(p["number"])' "$2" "$3" 2>/dev/null); do
    gitea_api "" "$lab/$1/pulls/$n/files" | grep -q "\"filename\":\"$4\"" && { echo "$n"; return 0; }
  done
  return 1
}
lab_merge() { gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$lab/$1/pulls/$2/merge"; }
check "alice signs in to the chat"         chat_login alice

echo "# 1. alice has the commander's operator make the namespace and the repo"
answer=$(ask_agent system alice "Make a namespace lab for our experiments, and in it the repo example from the template static-web: a small web page we will deploy. You have everything you need; do not ask back.")
lab_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/lab")" = 200 ]; }
example_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$lab/example/raw/index.html")" = 200 ]; }
if ! lab_made || ! example_made; then
  printf 'retry the operator did not do both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent system alice "Please go ahead now: create_namespace lab if it is missing, then create_repo example in lab with template static-web.")
fi
check "it makes the namespace lab, as alice" lab_made
check "and the repo lab/example from the template" example_made
check "with its pipeline"                  test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$lab/example/raw/.gitea/workflows/pipeline.yaml")" = 200
started_by() { gitea_api "" "$lab/example/commits?sha=main&limit=1" | python3 -c 'import json,sys; print((json.load(sys.stdin)[0].get("author") or {}).get("login", ""))'; }
check "written as alice"                   test "$(started_by)" = alice
lab_channel() { channel_of lab alice >/dev/null; }
check "lab gets its channel"               until_true 180 lab_channel
lab_operator() { chat_api alice "$chat/api/models" | grep -q '"id":"lab/operator"'; }
check "and its operator"                   until_true 180 lab_operator
main1=$(head_of_lab example main)
built() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/packages/lab/container/example/${1:0:12}")" = 200 ]; }
check "the pipeline builds main's image"   until_true 900 built "$main1"

echo "# 2. alice has lab's operator propose the stack; the admin approves; alice merges in Gitea"
answer=$(ask_agent lab alice "Deploy the app example of lab: propose stacks/example.yaml in lab/config as the README of lab/example says, with the environments main and dev, both public (no private). Do not ask back.")
spr=$(lab_pr config alice main stacks/example.yaml)
if [ -z "$spr" ]; then
  printf 'retry the operator proposed no stack; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent lab alice "Please propose stacks/example.yaml in lab/config now with propose_change: source example, entrypoint web, service web with image \"{{registry}}/lab/example:{{commit}}\" and port 8080, environments main: {} and dev: {}.")
  spr=$(lab_pr config alice main stacks/example.yaml)
fi
check "it proposes stacks/example.yaml, as alice" test -n "$spr"
gitea_api "" "$lab/config/raw/stacks/example.yaml?ref=$(gitea_api "" "$lab/config/pulls/${spr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)" > "$tmp/example.yaml"
check "one the deployer takes"             garrison validate --in lab "$tmp/example.yaml"
check "with main and dev, both public"     python3 -c 'import sys,yaml; e=yaml.safe_load(open(sys.argv[1]))["environments"]; sys.exit(0 if set(e) >= {"main","dev"} and not any((v or {}).get("private") for v in e.values()) else 1)' "$tmp/example.yaml"
check "a root admin who did not write it approves" test "$(gitea_api "" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$lab/config/pulls/${spr:-0}/reviews")" = 200
stack_ok() {
  gitea_api "" "$lab/config/commits/$(gitea_api "" "$lab/config/pulls/${spr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "the approval check passes"          until_true 120 stack_ok
check "alice merges it in Gitea"           test "$(lab_merge config "${spr:-0}")" = 200
deployed() { garrison applied | grep -q "^app lab/example@$1 $2 "; }
check "the deployer deploys main"          until_true 600 deployed main example
shows() { page "$1" | grep -q "$2"; }
check "https://example.$DOMAIN shows the page" until_true 300 shows example "<h1>example</h1>"
check "on white"                           shows example "background-color: #ffffff"

echo "# 3. alice makes the branch dev in Gitea; it deploys at dev-example"
# Nobody merges for this: the stack names dev as an environment, so the
# deployer deploys the branch once it exists, from the image of its commit,
# which main's build made already. alice is lab's only person, so she
# pushes and merges in its repos freely.
check "alice makes dev from main"          test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"dev","old_branch_name":"main"}' "$lab/example/branches")" = 201
check "the deployer deploys dev"           until_true 900 deployed dev dev-example
check "https://dev-example.$DOMAIN shows the page" until_true 300 shows dev-example "<h1>example</h1>"

echo "# 4. alice has lab's operator change the background on dev"
answer=$(ask_agent lab alice "Change the background colour of the page of lab/example to #2e7d32, on its branch dev: read index.html there and propose it with the new colour, with base dev. Change nothing else. Do not ask back.")
cpr=$(lab_pr example alice dev index.html)
if [ -z "$cpr" ]; then
  printf 'retry the operator proposed no change; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent lab alice "Please propose it now: index.html of lab/example on branch dev, with background-color: #2e7d32 instead of #ffffff, as a pull request with base dev.")
  cpr=$(lab_pr example alice dev index.html)
fi
check "it proposes it for dev, as alice"   test -n "$cpr"
changed() { gitea_api "" "$lab/example/pulls/${cpr:-0}/files" | python3 -c 'import json,sys; sys.exit(0 if [f["filename"] for f in json.load(sys.stdin)] == ["index.html"] else 1)'; }
check "changing index.html alone"          changed
check "alice merges it into dev in Gitea"  test "$(lab_merge example "${cpr:-0}")" = 200
check "dev-example turns green"            until_true 900 shows dev-example "#2e7d32"
check "example, on main, is still white"   shows example "background-color: #ffffff"

echo "# 5. alice proposes dev for main and merges it in Gitea"
mpr=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"dev","base":"main","title":"Green background"}' "$lab/example/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])')
check "alice opens the pull request from dev to main" test -n "$mpr"
check "alice merges it"                    test "$(lab_merge example "${mpr:-0}")" = 200
check "example, on main, turns green"      until_true 900 shows example "#2e7d32"
check "alice opens her dashboard"          until_true 60 home_login alice
check "its feed shows lab's deploys"       in_feed alice ns=lab "e['kind'] == 'deploy' and e['status'] == 'ok'"
}
