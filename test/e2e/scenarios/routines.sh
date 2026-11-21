# shellcheck shell=bash
# routines: a routine runs when main moves, with nobody watching, and asks its person.
section_routines() {
talk_setup || return 0
echo "# 4j0. the developer gets a routine for when main of app moves"
cat >> "$tmp/routines.yaml" <<'YAML'
- agent: developer
  name: main-moved
  on_push: {repo: app, branch: main}
  owner: bob
  task: Call read_file on stacks/todo.yaml of the repo config of the unit todo, then say in one sentence which environments it has. Change nothing.
YAML
rpr=$(propose_config main-moved "The developer's routine" "routines.yaml=$tmp/routines.yaml")
check "carol approves the routine"         test "$(review config carol "$rpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$rpr" success
check "alice merges it"                    merges config alice "$rpr" carol
# Waits until the routine has seen main, so the next push is news to it.
sleep 90
mainmoved=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf 'Notes.\n' > "$tmp/notes.md"
check "a change lands on app's main"       to_main app NOTES.md "$tmp/notes.md" "Notes"

echo "# 4j. a routine runs when main moves, with nobody watching"
hcid=$(channel_of todo)
routine_post() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**developer**") and "Routine main-moved, for bob" in c:
        print(m["id"]); sys.exit(0)
sys.exit(1)'
}
check "its answer is posted in the channel" until_true 600 routine_post
routine_post >/dev/null ||
  for s in agents gateway; do kubectl --namespace garrison logs "deployment/$s" --since=15m 2>&1 | grep -E 'main-moved|post: ' | tail -8 | sed 's/^/      /'; done
check "it read as its own account, for bob" bash -c 'kubectl --namespace garrison logs deployment/gateway --since-time="$0" 2>&1 | grep -q "mcp: bob (nobody watching, as agent-todo-developer): todo/developer: [a-z_]* \[routine todo/developer/main-moved\]"' "$mainmoved"
head_kept() { [ "$(kubectl --namespace garrison exec -i deployment/postgres -- psql -U postgres -d garrison -tAc "SELECT head FROM routine_state WHERE key = 'todo/developer/main-moved'")" = "$(gitea_api "" "$hp/app/branches/main" | python3 -c 'import json,sys; print(json.load(sys.stdin)["commit"]["id"])')" ]; }
check "the commit it saw is kept, for after a restart" head_kept
check "the feed has the run, for todo's people" in_feed carol ns=todo "e['kind'] == 'routine' and e['agent'] == 'todo/developer' and e['actor'] == 'bob' and e['status'] == 'ok' and e['detail'] != ''"
if id=$(routine_post); then web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$hcid/messages/$id/delete"; fi

echo "# 4j2. a routine asks its person, and goes on with the answer"
printf 'description: Asks before it decides\ntools: [list_repos]\n' > "$tmp/asker.yaml"
printf 'You ask before you decide anything, and say plainly what was decided.\n' > "$tmp/asker.md"
apr=$(REPO=$gc propose_config asker "Add the asker" "agents/asker/agent.yaml=$tmp/asker.yaml" "agents/asker/prompt.md=$tmp/asker.md")
check "a root admin approves the asker, alice merges it" lands "$apr"
cat >> "$tmp/routines.yaml" <<'YAML'
- agent: asker
  name: colour
  on_push: {repo: app, branch: main}
  owner: bob
  task: "Call ask_person with exactly this question: Which colour should the header of todo be, blue or green? Then end your turn. When you have the answer, reply with one sentence that names the colour chosen."
YAML
rpr=$(propose_config asker-routine "The asker's routine" "routines.yaml=$tmp/routines.yaml")
check "carol approves its routine in todo" test "$(review config carol "$rpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$rpr" success
check "alice merges it"                    test "$(merge config alice "$rpr")" = 200
# Its routine must see main before the push, so the push is news to it.
asker_seen() { kubectl --namespace garrison exec -i deployment/postgres -- psql -U postgres -d garrison -tAc "SELECT head FROM routine_state WHERE key = 'todo/asker/colour'" | grep -q .; }
check "the routine sees main"              until_true 180 asker_seen
printf 'More notes.\n' > "$tmp/notes2.md"
check "another change lands on app's main" to_main app NOTES2.md "$tmp/notes2.md" "More notes"
question_post() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**asker**") and "<@U:" in c and "|bob>" in c and "Reply in this thread" in c:
        print(m["id"]); sys.exit(0)
sys.exit(1)'
}
check "it asks bob in the channel, naming him" until_true 600 question_post
qid=$(question_post)
# The page is read whole first: grep -q stops at its first match, and curl,
# writing the rest of a large page into the closed pipe, fails, which fails
# the pipeline (pipefail).
waiting_page() { local page; page=$(feed bob /) && grep -q "Waiting for you" <<<"$page" && grep -q "todo/asker asks bob" <<<"$page"; }
check "bob's dashboard lists it as waiting for him" until_true 60 waiting_page
check "the feed has it, waiting"           in_feed bob ns=todo "e['kind'] == 'question' and e['agent'] == 'todo/asker' and e['actor'] == 'bob' and e['status'] == 'waiting'"
check "bob answers in its thread" \
  test "$(chat_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Green, please.","parent_id":"'"$qid"'"}' "$chat/api/v1/channels/$hcid/messages/post")" = 200
asker_reply() {
  chat_api bob "$chat/api/v1/channels/$hcid/messages/$qid/thread?limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**asker**") and "<@U:" not in c and len(c) > len("**asker**") + 2:
        print(c); sys.exit(0)
sys.exit(1)'
}
check "the routine goes on, and says so in the thread" until_true 600 asker_reply
check "with bob's answer"                  grep -qi green <<<"$(asker_reply)"
check "the question is answered in the feed" in_feed bob ns=todo "e['kind'] == 'question' and e['agent'] == 'todo/asker' and e['status'] == 'ok' and 'Green' in e['detail']"
# The feed itself still lists the question; the list above it is gone.
not_waiting() { local page; page=$(feed bob /) && ! grep -q "Waiting for you" <<<"$page"; }
check "bob's dashboard no longer lists it" until_true 60 not_waiting

}
