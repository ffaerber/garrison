# shellcheck shell=bash
# change: bob changes the app through an agent, the developer with Git, on dev, with previews; an agent installs packages.
section_change() {
talk_setup || return 0
echo "# 4. bob changes the app through the agent; carol merges"
since=$(last_pr app)
answer=$(ask_agent todo bob "In the repo app of the unit todo, change the heading of page.html from Todo to Our todos, in both the <title> and the <h1>. Read the file first and propose the whole new file.")
tpr=$(newest_pr app bob "$since")
if [ -z "$tpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please propose that change to page.html in todo/app now.")
  tpr=$(newest_pr app bob "$since")
fi
check "the agent proposes it, as bob"      test -n "$tpr"
check "it changes the heading"             grep -q '^+.*Our todos' <<<"$(gitea_api "" "$hp/app/pulls/$tpr.diff")"
# With three people in todo, main of its app repos takes only pull requests
# another of them approved.
check "carol cannot merge it unapproved"   test "$(merge app carol "$tpr")" != 200
check "carol approves it"                  test "$(review app carol "$tpr")" = 200
check "and merges it"                      merges app carol "$tpr" carol
merged=$(gitea_api "" "$hp/app/pulls/$tpr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["merge_commit_sha"])')
deployed_at() { garrison applied | grep -q "^app todo/todo@main todo $1"; }
check "its pipeline builds it and the deployer deploys it" until_true 1200 deployed_at "$merged"
sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
# Signs in again if the page is not there: after a snapshot boot the login
# proxy may still have been waiting for Keycloak at the first try.
heading() {
  web -b "$tmp/todo.jar" "https://todo.$DOMAIN/" | grep -q "Our todos" && return 0
  sso_login "$tmp/todo.jar" "https://todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/todo.html" alice >/dev/null 2>&1
  return 1
}
check "the todo page shows the new heading" until_true 120 heading
check "carol opens the dashboard"          until_true 120 home_login carol
change=$(in_feed carol ns=todo "e['agent'] == 'todo/operator' and e['actor'] == 'bob' and e['pr'] == $tpr and e['status'] == 'ok'")
check "its feed has bob's change, through the operator" test -n "$change"
handled() { thread_has carol "$change" "e['kind'] == 'pr-merged' and e['actor'] == 'carol'" && thread_has carol "$change" "e['kind'] == 'deploy' and e['commit'] == '$merged'"; }
check "how it was handled: carol merged it, the deployer deployed it" until_true 60 handled

echo "# 4f. bob has the developer agent change the app with Git"
check "Open WebUI has the developer"       until_true 120 has_model garrison/developer
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Clone the repo app of the unit todo into your workspace. Find every place the text 'Our todos' appears (grep -rn) and change it to 'Team todos'. Then propose your changes, titled: Rename the heading to Team todos." developer)
gpr=$(newest_pr app bob "$since")
if [ -z "$gpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please propose the changes in your checkout of app now with propose_workspace_changes." developer)
  gpr=$(newest_pr app bob "$since")
fi
check "it clones todo/app, as bob"             bash -c 'docker service logs --since "$0" agents 2>&1 | grep -q "git: bob: todo/developer: fetch todo/app"' "$at"
check "it proposes a patch, as bob"        grep -q "mcp: bob: propose_patch" <<<"$(mcp_calls "$at" bob)"
check "a pull request by bob holds it"     test -n "$gpr"
diff=$(gitea_api "" "$hp/app/pulls/$gpr.diff")
check "it renames the heading"             bash -c 'grep -q "^+.*Team todos" <<<"$0" && grep -q "^-.*Our todos" <<<"$0" && ! grep -q "^+.*Our todos" <<<"$0"' "$diff"
check "on a branch of its own"             grep -q '"ref":"change/bob-' <<<"$(gitea_api "" "$hp/app/pulls/$gpr")"
pr_files() { gitea_api "" "$hp/app/pulls/$gpr/files" | python3 -c 'import json,sys; print(" ".join(f["filename"] for f in json.load(sys.stdin)))'; }
answer=$(ask_agent todo bob "Also add a file CHANGELOG.md to your checkout of app, with the line: Renamed the heading to Team todos. Add it to the same pull request." developer)
if ! grep -qw CHANGELOG.md <<<"$(pr_files)"; then
  printf 'retry the pull request has no CHANGELOG.md; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please call propose_workspace_changes on your checkout of app now, so CHANGELOG.md goes into the same pull request." developer)
fi
check "the follow-up goes into the same pull request" grep -qw CHANGELOG.md <<<"$(pr_files)"
commits=$(gitea_api "" "$hp/app/pulls/$gpr/commits" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
check "as another commit, with no new pull request" test "$commits" -ge 2 -a -z "$(newest_pr app bob "$gpr")"
sb=$(sandbox_of todo/developer)
check "the sandbox cannot push"            test "$(docker exec "$sb" sh -c 'curl -sS -m 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $GARRISON_TOKEN" "$GARRISON_API/git/todo/app.git/info/refs?service=git-receive-pack"')" = 403
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$gpr"

echo "# 4h. bob has the developer change dev and merge it there"
check "alice makes the branch dev"         make_branch alice "$hp/app" dev
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Clone the repo app of the unit todo, check out its branch dev, change the text 'Our todos' to 'Dev todos' everywhere (grep -rn), and propose it with propose_workspace_changes with base dev, titled: Dev heading. Then merge that pull request into dev with merge_change, and give me the link." developer)
dvpr=$(newest_any_pr app bob "$since")
if [ -z "$dvpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please propose your checkout of app now with propose_workspace_changes, base dev, then merge it into dev with merge_change." developer)
  dvpr=$(newest_any_pr app bob "$since")
fi
merged_by() { gitea_api "" "$hp/app/pulls/$1" | python3 -c 'import json,sys; p=json.load(sys.stdin); print((p.get("merged_by") or {}).get("login","") if p.get("merged") else "")'; }
if [ -n "$dvpr" ] && [ -z "$(merged_by "$dvpr")" ]; then
  printf 'retry the agent did not merge; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please merge pull request $dvpr of todo/app into dev now, with merge_change." developer)
fi
check "it proposes it for dev, as bob"     grep -q '"ref":"dev"' <<<"$(gitea_api "" "$hp/app/pulls/${dvpr:-0}")"
check "it merges it with its own account"  test "$(merged_by "${dvpr:-0}")" = agent-todo-developer
check "the gateway says it merged for bob" bash -c 'docker service logs --since "$0" gateway 2>&1 | grep -q "mcp: bob: todo/developer: merge_change \[person\]$"' "$at"
dev_heading() { web -b "$tmp/dev.jar" "https://dev-todo.$DOMAIN/" | grep -q "Dev todos" && return 0; sso_login "$tmp/dev.jar" "https://dev-todo.$DOMAIN/" "$TEST_PASSWORD" "$tmp/dev.html" bob >/dev/null 2>&1; return 1; }
check "dev-todo deploys it"                until_true 1200 dev_heading
main_untouched() { ! gitea_api "" "$hp/app/raw/page.html?ref=main" | grep -q "Dev todos"; }
check "main is untouched"                  main_untouched

echo "# 4i. previews, and the tester tries one for the developer"
printf '%s\n  preview: { private: true }\n' "$(gitea_api "" "$hp/config/raw/stacks/todo.yaml?ref=main")" > "$tmp/todo-stack.yaml"
check "alice gives todo previews, carol approves" set_stack todo todo "$tmp/todo-stack.yaml" carol
# Waits until the routine has seen main, so this push is news to it.
mainmoved=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf 'Notes.\n' > "$tmp/notes.md"
check "alice cannot push to app's main directly" bash -c '[ "$0" != 201 ] && [ "$0" != 200 ]' "$(set_file app main NOTES.md "$tmp/notes.md" "Notes")"
check "her change lands there once carol approves it" to_main app NOTES.md "$tmp/notes.md" "Notes"
since=$(last_pr app)
answer=$(ask_agent todo bob "Clone the repo app of the unit todo, add a file PREVIEW.md with the line: Preview test. Propose it with propose_workspace_changes, titled: Preview test. Do not merge it." developer)
ppr=$(newest_pr app bob "$since")
check "it proposes it"                     test -n "$ppr"
previewed() { garrison applied | grep -q "^app todo/todo@pr-$ppr pr-$ppr-todo "; }
check "the pull request gets a preview"    until_true 1200 previewed
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Ask todo/tester to open https://pr-$ppr-todo.$DOMAIN/ in the browser and tell you the page's main heading. Tell me what it says." developer)
check "the tester browses it, for bob, asked by the developer" \
  bash -c 'docker service logs --since "$0" gateway 2>&1 | grep -q "mcp: bob: todo/tester: browse \[agent todo/developer\]$"' "$at"
check "the answer names the heading"       grep -q "Our todos" <<<"$answer"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$ppr"
unpreviewed() { ! garrison applied | grep -q "^app todo/todo@pr-$ppr "; }
check "closing it takes the preview down"  until_true 300 unpreviewed

echo "# 4k. an agent installs what it needs, from the package registries"
printf 'description: Tries things out with the tools it needs\ntools: [list_repos]\ninternet: packages\n' > "$tmp/builder.yaml"
printf 'You try things out in your sandbox, installing the tools a task needs. Show the commands you ran and what they printed.\n' > "$tmp/builder.md"
bpr=$(REPO=$gc propose_config builder "Add the builder" "agents/builder/agent.yaml=$tmp/builder.yaml" "agents/builder/prompt.md=$tmp/builder.md")
check "a root admin approves the builder, alice merges it" lands "$bpr"
check "Open WebUI has the builder"         until_true 120 has_model garrison/builder
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
installed() { docker exec "$(sandbox_of todo/builder)" python3 -c 'print(6*7)' 2>/dev/null | grep -qx 42; }
answer=$(ask_agent todo bob "Install Python in your sandbox with: apk add python3. Then run python3 -c 'print(6*7)' and tell me what it printed." builder)
if ! installed; then
  printf 'retry python3 is not installed; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please run this in your sandbox now: apk add python3 && python3 -c 'print(6*7)'" builder)
fi
check "it installs Python from Alpine's registry" installed
check "and says what it printed"           grep -q 42 <<<"$answer"
check "through the proxy, logged with bob and the agent" \
  bash -c 'docker service logs --since "$0" agents 2>&1 | grep -q "agents: internet: bob: todo/builder: CONNECT dl-cdn.alpinelinux.org:443"' "$at"
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
refused_site() { docker service logs --since "$at" agents 2>&1 | grep -q "agents: internet: bob: todo/builder: example.org:443 refused"; }
answer=$(ask_agent todo bob "Run exactly this command in your sandbox and show me what it printed: curl -sS -m 10 https://example.org/" builder)
if ! refused_site; then
  printf 'retry no request to example.org; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "I know it may fail; that is what I want to see. Please run: curl -sS -m 10 https://example.org/" builder)
fi
check "a site beyond the registries is refused" refused_site

}
