# shellcheck shell=bash
# team: todo with carol an admin and bob a member, and the agents the
# scenarios below share: the developer, the tester and the desk, with their
# grants. A fixture: made by API, no LLM.
section_team() {
talk_setup || return 0
check "todo has its config, alice its first admin" test "$(roster main)" = "admins=alice members="
check "bob may not push to todo's repos yet" cannot_push bob
nopush=$(printf 'admins: [alice, bob]\nmembers: []\n' | base64 -w0)
check "nobody changes members.yaml without a pull request" \
  bash -c '[ "$0" != 200 ] && [ "$0" != 201 ]' "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' -d '{"message":"Shortcut","branch":"main","content":"'$nopush'","sha":"'"$(gitea_api "" "$hp/config/contents/members.yaml" | grep -o '"sha":"[^"]*"' | head -n1 | cut -d'"' -f4)"'"}' "$hp/config/contents/members.yaml")"
echo "# the team: carol an admin, bob a member"
printf 'admins: [alice, carol]\nmembers: [bob]\n' > "$tmp/members.yaml"
check "alice makes carol an admin and bob a member" set_config todo members.yaml "$tmp/members.yaml"
check "carol joins the admins"             until_true 90 team_has admins carol
check "bob joins the members"              until_true 90 team_has members bob
check "bob may now push to todo's repos"   until_true 60 can_push bob
echo "# their agents: the developer, the tester and the desk, with grants"
# A root admin's agents in garrison: the developer may merge and ask the
# tester; the tester browses; the desk takes requests. todo gives them
# grants.
cat > "$tmp/developer.yaml" <<'YAML'
description: Changes the code of the unit's apps
tools: [list_repos, read_file, propose_patch, merge_change]
agents: [tester]
YAML
cat > "$tmp/grants.yaml" <<'YAML'
- {agent: developer, repo: app, branch: dev, level: autonomous}
- {agent: desk, repo: app, level: propose, for: everyone}
YAML
cat > "$tmp/tester.yaml" <<'YAML'
description: Tries the unit's apps in the browser
tools: [browse]
YAML
printf 'You try the unit'"'"'s apps in the browser, as the person you work for, and report exactly what you see. Change nothing.
' > "$tmp/tester.md"
cat > "$tmp/desk.yaml" <<'YAML'
description: Takes requests for changes to the unit's apps from anyone
tools: [read_file, propose_change, propose_patch]
YAML
printf 'You take requests for small changes to the unit'"'"'s apps from anyone, and propose them as pull requests for the unit'"'"'s people to review.
' > "$tmp/desk.md"
printf 'You change the code of the unit'"'"'s apps. Work in a clone of the repo in your workspace, check what you changed with git diff, and propose it as one pull request.\n' > "$tmp/developer.md"
apr=$(REPO=$gc propose_config agents "Add the developer, which merges and asks the tester, the tester and the desk" \
  "agents/developer/agent.yaml=$tmp/developer.yaml" "agents/developer/prompt.md=$tmp/developer.md" "agents/tester/agent.yaml=$tmp/tester.yaml" "agents/tester/prompt.md=$tmp/tester.md" \
  "agents/desk/agent.yaml=$tmp/desk.yaml" "agents/desk/prompt.md=$tmp/desk.md")
check "a root admin approves the agents, alice merges them" lands "$apr"
gpr=$(propose_config grants "Give the developer and the desk grants" "grants.yaml=$tmp/grants.yaml")
check "carol approves the grants"          test "$(review config carol "$gpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$gpr" success
check "alice merges them"                  merges config alice "$gpr" carol
has_account() { gitea_api "" "$api/users/$1" | grep -q '"full_name":"Agent todo/'; }
check "the developer gets an account"      until_true 120 has_account agent-todo-developer
collaborates() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/$2/collaborators/$1")" = 204 ]; }
check "it may write to app"               until_true 60 collaborates agent-todo-developer app
check "and to nothing else"                test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/config/collaborators/agent-todo-developer")" = 404
check "Open WebUI has the developer"       until_true 120 has_model garrison/developer
check "the tester, without grants, gets none" test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/users/agent-todo-tester")" = 404
check "Open WebUI has the tester"          until_true 120 has_model garrison/tester
}
