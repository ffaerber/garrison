# shellcheck shell=bash
# leaving: alice removes bob; the desk takes his request; a private repo; an agent that may only read.
section_leaving() {
talk_setup || return 0
echo "# 5. alice removes bob"
since=$(last_pr config)
answer=$(ask_agent todo alice "Remove bob from the unit todo.")
rpr=$(newest_pr config alice "$since")
if [ -z "$rpr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo alice "Please propose the change to members.yaml in todo/config now, without bob.")
  rpr=$(newest_pr config alice "$since")
fi
check "the agent proposes it, as alice"    test -n "$rpr"
check "it removes bob and keeps the rest"  grep -q "^admins=alice,carol members=$" <<<"$(roster "$(head_sha config "$rpr")")"
check "carol approves it"                  test "$(review config carol "$rpr")" = 200
check "the approval check passes"          until_true 90 approval_is "$rpr" success
check "alice merges it"                    test "$(merge config alice "$rpr")" = 200
gone_from_members() { ! team_has members bob; }
check "bob leaves the members"             until_true 90 gone_from_members
check "bob may no longer push"             until_true 60 cannot_push bob
check "bob can still read everything"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' "$hp/app")" = 200

# Open WebUI follows the members within the gateway's next sync.
no_channel() { [ -z "$(channel_of todo bob)" ]; }
check "bob no longer sees todo's channel" until_true 120 no_channel

echo "# 5b. bob, no longer in todo, asks the desk for a change, in a chat of his own"
# The desk works there in no unit, but a change to todo's repo goes
# with the grant todo gives it.
# ask_private PERSON AGENT QUESTION: asks an agent in a chat of PERSON's own,
# through Open WebUI's API, and prints its answer.
ask_private() {
  chat_api "$1" --max-time 600 -X POST -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"model": sys.argv[1], "stream": False, "messages": [{"role": "user", "content": sys.argv[2]}]}))' "$2" "$3")" \
    "$chat/api/chat/completions" | python3 -c 'import json,sys; print(json.load(sys.stdin)["choices"][0]["message"]["content"])'
}
since=$(last_pr app)
answer=$(ask_private bob garrison/desk "In the repo app of the unit todo, propose a new file IDEAS.md with the line: Dark mode, please. Title: An idea from bob. Read nothing else.")
opr=$(newest_pr app agent-todo-desk "$since")
if [ -z "$opr" ]; then
  printf 'retry the agent opened no pull request; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_private bob garrison/desk "Please propose that IDEAS.md for todo/app now with propose_change: unit todo, repo app, path IDEAS.md, content: Dark mode, please.")
  opr=$(newest_pr app agent-todo-desk "$since")
fi
check "the desk proposes it with its own account" test -n "$opr"
check "it says for whom"                   grep -q 'Asked by bob (not in todo) through todo/desk' <<<"$(gitea_api "" "$hp/app/pulls/${opr:-0}")"
check "on a branch of the desk's"          grep -q '"ref":"agent/desk/bob-' <<<"$(gitea_api "" "$hp/app/pulls/${opr:-0}")"
[ -n "$opr" ] && gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$opr"

echo "# 5c. a private repo has a private agent"
check "alice makes a private repo secret"  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"secret","private":true,"auto_init":true,"default_branch":"main"}' "$api/orgs/todo/repos")" = 201
check "carol's feed shows it"              until_true 60 in_feed carol ns=todo "e['kind'] == 'repo-created' and e['repo'] == 'secret'"
# The dashboard asks Gitea again after at most 20 seconds.
sleep 21
check "bob's, out of todo, does not"       not_in_feed bob ns=todo "e['kind'] == 'repo-created' and e['repo'] == 'secret'"
printf 'description: Knows a unit'"'"'s repo secret\ntools: [read_file]\n' > "$tmp/keeper.yaml"
printf 'You answer questions about the unit'"'"'s repo secret, reading its files.\n' > "$tmp/keeper.md"
kpr=$(REPO=$gc propose_config keeper "Add the keeper" "agents/keeper/agent.yaml=$tmp/keeper.yaml" "agents/keeper/prompt.md=$tmp/keeper.md")
check "a root admin approves the keeper, alice merges it" lands "$kpr"
# A grant on the private repo makes the keeper, as it works in todo, todo's
# people's only.
printf -- '- {agent: keeper, repo: secret, level: propose}\n' >> "$tmp/grants.yaml"
kgr=$(propose_config keeper-grant "The keeper may propose in secret" "grants.yaml=$tmp/grants.yaml")
check "carol approves its grant in todo"   test "$(review config carol "$kgr")" = 200
check "the approval check passes"          until_true 90 approval_is "$kgr" success
check "alice merges it"                    test "$(merge config alice "$kgr")" = 200
check "carol sees the keeper"              until_true 120 has_model garrison/keeper carol
answer=$(ask_agent todo carol "Read README.md of the repo secret of the unit todo and tell me its first line." keeper)
check "it answers carol in todo's channel" grep -qi secret <<<"$answer"
# In garrison's channel, which everyone reads, the commander may not hand
# work to the keeper as it works in todo.
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
refused_here() { docker service logs --since "$at" agents 2>&1 | grep -q "agents: carol: garrison/commander may not ask todo/keeper: .*answers only where only todo's people read"; }
answer=$(ask_agent garrison carol "Use ask_agent to ask todo/keeper for the first line of README.md of the repo secret of the unit todo." commander)
if ! refused_here; then
  printf 'retry the commander asked no keeper; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent garrison carol "Please call ask_agent now with agent todo/keeper, task: the first line of README.md of todo/secret, done_when: the line." commander)
fi
check "it does not answer in garrison's channel, which everyone reads" refused_here
gitea_api "" -o /dev/null -X DELETE "$hp/secret"

echo "# 6. alice adds an agent that may only read"
check "todo has no agents of its own; the operator is garrison's" \
  bash -c '[ "$0" = 404 ] && [ "$1" = 200 ] || { echo "todo: $0, garrison: $1"; false; }' "$(gitea_api "" -o /dev/null -w '%{http_code}' "$hp/config/raw/agents/operator/prompt.md?ref=main")" "$(gitea_api "" -o /dev/null -w '%{http_code}' "$gc/raw/agents/operator/prompt.md?ref=main")"
reader() { add_agent "$1" reader "$2" "Reads the unit's repos" "You read files of the unit's repos and explain them."; }
bad=$(reader reader-bad 'read_file, rm_rf')
check "an agent with a tool that does not exist fails the check" until_true 90 gc_approval_is "$bad" failure
check "the check names the tool"           grep -q 'rm_rf' <<<"$(gc_approval "$bad")"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$gc/pulls/$bad"
good=$(reader reader read_file)
check "a root admin approves the reader, alice merges it" lands "$good"
check "Open WebUI has the reader"          until_true 120 has_model garrison/reader
own=$(REPO=$hp/config propose_config own-agent "An agent of todo's own" "agents/mine/agent.yaml=$tmp/keeper.yaml" "agents/mine/prompt.md=$tmp/keeper.md")
check "an agent folder in todo fails the check" until_true 90 approval_is "$own" failure
check "the check says agents are garrison's" grep -q 'agents are defined in garrison/config only' <<<"$(approval "$own")"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/$own"
since=$(last_pr app)
answer=$(ask_agent todo alice "Propose a change to the repo app: add a file reader.txt containing hello. Title: Reader test." reader)
check "the reader answers"                 test -n "$answer"
check "it cannot propose a change"         test -z "$(newest_pr app alice "$since")"

}
