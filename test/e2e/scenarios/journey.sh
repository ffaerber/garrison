# journey: an app with code of its own goes from nothing to main, the way
# people do it: alice asks the agents in Open WebUI to make the namespace,
# the repo and the stack, merges in Gitea, has an agent change the page on
# dev, looks at dev, and merges dev into main. Talks to the real LLM; the
# checks look at what happened in Gitea, the registry and the cluster, and
# at the pages themselves. Sourced by smoke.sh, whose helpers it uses.

section_journey() {
echo "# journey: lab/example, from nothing to main, through the agents"
lab=$api/repos/lab
# page NAME: the page at https://NAME.<domain>, as alice: the todo app is
# behind the sign-in, so she signs in when it asks her to.
page() {
  local out
  out=$(web -b "$tmp/lab-$1.jar" "https://$1.$DOMAIN/")
  if ! grep -q '<h1>' <<<"$out"; then
    sso_login "$tmp/lab-$1.jar" "https://$1.$DOMAIN/" "$TEST_PASSWORD" "$tmp/lab-$1.html" alice >/dev/null 2>&1
    out=$(web -b "$tmp/lab-$1.jar" "https://$1.$DOMAIN/")
  fi
  printf '%s' "$out"
}
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

echo "# 1. alice has the operator make the namespace and the repo"
answer=$(ask_agent garrison alice "Make a namespace lab for our experiments, and in it the repo example from the template todo: a todo list we will deploy. You have everything you need; do not ask back.")
lab_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/lab")" = 200 ]; }
example_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$lab/example/raw/page.html")" = 200 ]; }
if ! lab_made || ! example_made; then
  printf 'retry the operator did not do both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent garrison alice "Please go ahead now: create_namespace lab if it is missing, then create_repo example in lab with template todo.")
fi
check "it makes the namespace lab, as alice" lab_made
check "and the repo lab/example from the template" example_made
check "with its pipeline"                  test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$lab/example/raw/.gitea/workflows/pipeline.yaml")" = 200
started_by() { gitea_api "" "$lab/example/commits?sha=main&limit=1" | python3 -c 'import json,sys; print((json.load(sys.stdin)[0].get("author") or {}).get("login", ""))'; }
check "written as alice"                   test "$(started_by)" = alice
lab_channel() { channel_of lab alice >/dev/null; }
# The operator answers once lab is set up, so its channel is there as soon
# as it has; there the agents, garrison's, work in lab.
check "lab has its channel when the operator answers" until_true 5 lab_channel
# lab_said TEXT...: a message in lab's channel holds every TEXT.
lab_said() {
  chat_api alice "$chat/api/v1/channels/$(channel_of lab alice)/messages?skip=0&limit=50" | python3 -c '
import json,sys
sys.exit(0 if any(all(t in (m.get("content") or "") for t in sys.argv[1:]) for m in json.load(sys.stdin)) else 1)' "$@"
}
check "the operator welcomes alice there, with what to ask" until_true 120 lab_said "Welcome to lab" "@operator" "https://$DOMAIN"
main1=$(head_of_lab example main)
built() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/packages/lab/container/example/${1:0:12}")" = 200 ]; }
check "the pipeline builds main's image"   until_true 900 built "$main1"

echo "# 2. alice has the operator, in lab's channel, propose the stack; the admin approves; alice merges in Gitea"
answer=$(ask_agent lab alice "Deploy the app example of lab: propose stacks/example.yaml in lab/config as the README of lab/example says, with the environments main and dev. Do not ask back.")
spr=$(lab_pr config alice main stacks/example.yaml)
if [ -z "$spr" ]; then
  printf 'retry the operator proposed no stack; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent lab alice "Please propose stacks/example.yaml in lab/config now with propose_change: the yaml block of the README.md of lab/example, as it is.")
  spr=$(lab_pr config alice main stacks/example.yaml)
fi
check "it proposes stacks/example.yaml, as alice" test -n "$spr"
check "the answer links its card on the dashboard" grep -q "https://$DOMAIN/c/lab/config/${spr:-0}" <<<"$answer"
gitea_api "" "$lab/config/raw/stacks/example.yaml?ref=$(gitea_api "" "$lab/config/pulls/${spr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)" > "$tmp/example.yaml"
check "one the deployer takes"             garrison validate --in lab "$tmp/example.yaml"
check "with main and dev, both behind the sign-in" python3 -c 'import sys,yaml; e=yaml.safe_load(open(sys.argv[1]))["environments"]; sys.exit(0 if set(e) >= {"main","dev"} and all((v or {}).get("private") for v in e.values()) else 1)' "$tmp/example.yaml"
check "a root admin who did not write it approves" test "$(gitea_api "" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$lab/config/pulls/${spr:-0}/reviews")" = 200
stack_ok() {
  gitea_api "" "$lab/config/commits/$(gitea_api "" "$lab/config/pulls/${spr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "the approval check passes"          until_true 120 stack_ok
# alice merges it on its card, the link the operator gave her.
card() { web -b "$tmp/home-alice.jar" "https://$DOMAIN/c/lab/config/${spr:-0}"; }
check "alice signs in to her dashboard"    until_true 60 home_login alice
waits_for_alice() { local page; page=$(web -b "$tmp/home-alice.jar" "https://$DOMAIN/") && grep -q "href=\"/c/lab/config/${spr:-0}\"" <<<"$page"; }
check "it waits for her there"             waits_for_alice
check "its card says what it changes, in plain words" bash -c 'grep -q "how the app example is deployed" <<<"$0" && grep -q "Merging it deploys it" <<<"$0" && grep -q "value=\"merge\"" <<<"$0"' "$(card)"
check "a post from another site is refused" test "$(web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code}' -H "Origin: https://example.$DOMAIN" -X POST -d action=merge "https://$DOMAIN/c/lab/config/${spr:-0}")" = 403
check "alice merges it on its card"        test "$(web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code}' -H "Origin: https://$DOMAIN" -X POST -d action=merge "https://$DOMAIN/c/lab/config/${spr:-0}")" = 303
check "the card says it is merged"         bash -c 'grep -q "It is merged." <<<"$0"' "$(card)"
check "the operator says in lab's channel that it is merged" until_true 120 lab_said "lab/config#${spr:-0}" "is merged, by alice" "I will say here when it is deployed"
deployed() { garrison applied | grep -q "^app lab/example@$1 $2 "; }
check "the deployer deploys main"          until_true 600 deployed main example
shows() { page "$1" | grep -qF -- "$2"; }
check "https://example.$DOMAIN shows the todo list" until_true 300 shows example "<h1>Todo</h1>"
check "on its light grey"                  shows example "--bg: #f6f7f9"
check "the operator says there that it is live" until_true 120 lab_said "lab/config#${spr:-0}" "is live: https://example.$DOMAIN"

echo "# 3. alice makes the branch dev in Gitea; it deploys at dev-example"
# Nobody merges for this: the stack names dev as an environment, so the
# deployer deploys the branch once it exists, from the image of its commit,
# which main's build made already. alice is lab's only person, so she
# pushes and merges in its repos freely.
check "alice makes dev from main"          test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"new_branch_name":"dev","old_branch_name":"main"}' "$lab/example/branches")" = 201
check "the deployer deploys dev"           until_true 900 deployed dev dev-example
check "https://dev-example.$DOMAIN shows the todo list" until_true 300 shows dev-example "<h1>Todo</h1>"

echo "# 4. alice has the operator, in lab's channel, change the background on dev"
answer=$(ask_agent lab alice "Change the background colour of the todo list of lab/example to #2e7d32, on its branch dev: read page.html there and propose it with --bg: #2e7d32 instead of #f6f7f9, with base dev. Change nothing else. Do not ask back.")
cpr=$(lab_pr example alice dev page.html)
if [ -z "$cpr" ]; then
  printf 'retry the operator proposed no change; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent lab alice "Please propose it now: page.html of lab/example on branch dev, with --bg: #2e7d32 instead of --bg: #f6f7f9, as a pull request with base dev.")
  cpr=$(lab_pr example alice dev page.html)
fi
check "it proposes it for dev, as alice"   test -n "$cpr"
changed() { gitea_api "" "$lab/example/pulls/${cpr:-0}/files" | python3 -c 'import json,sys; sys.exit(0 if [f["filename"] for f in json.load(sys.stdin)] == ["page.html"] else 1)'; }
check "changing page.html alone"           changed
check "alice merges it into dev in Gitea"  test "$(lab_merge example "${cpr:-0}")" = 200
check "dev-example turns green"            until_true 900 shows dev-example "#2e7d32"
check "example, on main, is still grey"    shows example "--bg: #f6f7f9"

echo "# 5. alice proposes dev for main and merges it in Gitea"
mpr=$(gitea_api alice -X POST -H 'Content-Type: application/json' -d '{"head":"dev","base":"main","title":"Green background"}' "$lab/example/pulls" | python3 -c 'import json,sys; print(json.load(sys.stdin)["number"])')
check "alice opens the pull request from dev to main" test -n "$mpr"
check "alice merges it"                    test "$(lab_merge example "${mpr:-0}")" = 200
check "example, on main, turns green"      until_true 900 shows example "#2e7d32"
check "alice opens her dashboard"          until_true 60 home_login alice
check "its feed shows lab's deploys"       in_feed alice ns=lab "e['kind'] == 'deploy' and e['status'] == 'ok'"
}
