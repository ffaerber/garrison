# shellcheck shell=bash
# commander: the commander finds the agent; the operator guides, asking what it cannot know.
section_commander() {
talk_setup || return 0
echo "# 4l. bob asks the commander in todo's channel, which finds the agent for him"
answer=$(ask_agent todo bob "Which agent should I talk to for screenshots of the todo app for its homepage? Only tell me whom; do not do it yourself." commander)
if ! grep -qiE "sentry|marketing" <<<"$answer"; then
  printf 'retry it named no agent for screenshots; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please call find_agents and tell me which agent makes screenshots." commander)
fi
check "it names an agent for screenshots"  grep -qiE "sentry|marketing" <<<"$answer"
# In its thread, a Reply to its answer goes on without mentioning it: Open
# WebUI calls the model whose message is replied to. A plain message in the
# thread does not (AGENTS.md); the pinned how-to says so.
read -r tcid tmid < <(tail -n1 "$tmp/posted")
thread_json() { chat_api bob "$chat/api/v1/channels/$tcid/messages/$tmid/thread"; }
answers() { thread_json | python3 -c 'import json,sys; print(sum(1 for m in json.load(sys.stdin) if (m.get("meta") or {}).get("model_id") == "garrison/commander" and (m.get("meta") or {}).get("done")))'; }
its=$(thread_json | python3 -c 'import json,sys; print(next((m["id"] for m in json.load(sys.stdin) if (m.get("meta") or {}).get("model_id") == "garrison/commander"), ""))')
chat_api bob -o /dev/null -X POST -H 'Content-Type: application/json' \
  -d "{\"content\":\"And which one looks things up on the web? Only the name.\",\"parent_id\":\"$tmid\",\"reply_to_id\":\"$its\"}" \
  "$chat/api/v1/channels/$tcid/messages/post"
two() { [ "$(answers)" -ge 2 ]; }
check "a Reply to its answer, without @, gets another" until_true 300 two
# There it works in todo: it hands work to the agents as they work in todo.
handed() { in_feed bob ns=todo "e['kind'] == 'ask' and e['started_by'] == 'agent todo/commander' and e['actor'] == 'bob' and e['agent'].startswith('todo/')" >/dev/null; }
answer=$(ask_agent todo bob "Hand this to the agent that knows the stacks: which stacks does the namespace todo have? Have it answer here." commander)
if ! handed; then
  printf 'retry it handed nothing over; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please use ask_agent now: ask the operator which stacks the namespace todo has." commander)
fi
check "it hands a task over, as the feed shows" handed
check "which the agent finishes"           until_true 300 in_feed bob ns=todo "e['kind'] == 'ask' and e['started_by'] == 'agent todo/commander' and e['status'] == 'ok'"

echo "# 9b. the operator guides: it asks for what it cannot know, then does it"
# orgs: the names of every namespace.
orgs() { gitea_api "" "$api/admin/orgs?limit=50" | python3 -c 'import json,sys; print(" ".join(sorted(o["username"] for o in json.load(sys.stdin))))'; }
before=$(orgs)
answer=$(ask_agent garrison alice "Please create an organization for me.")
check "asked for an organisation, it makes none" test "$(orgs)" = "$before"
check "and asks for its name"              grep -Eqi '\?|tell me|what name|the name' <<<"$answer"
printf 'it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
asked_back=$answer
answer=$(ask_agent garrison alice "Call it family.")
family_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/family")" = 200 ]; }
check "told the name, it makes the namespace family, as alice" until_true 30 family_made
# The prompt says to call it a namespace; the local model does so in its
# question or once it made it, so either answer counts.
check "it names it a namespace"            grep -qi 'namespace' <<<"$asked_back $answer"

since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# routines.yaml may not exist yet: Gitea's 404 is JSON, not a list.
routines_before=$(gitea_api "" "$hp/config/raw/routines.yaml" | python3 -c 'import sys,yaml; d = yaml.safe_load(sys.stdin); print(len(d) if isinstance(d, list) else 0)')
answer=$(ask_agent todo alice "Please create me a cron job: every day at 9 o'clock in the morning, check that the todo app answers on main and tell us in this channel. Do not ask back.")
# routine_pr: the open pull request of todo/config by alice that changes routines.yaml.
routine_pr() {
  local n
  for n in $(gitea_api "" "$hp/config/pulls?state=open&limit=10" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="alice"]' 2>/dev/null); do
    gitea_api "" "$hp/config/pulls/$n/files" | grep -q '"filename":"routines.yaml"' && { echo "$n"; return 0; }
  done
  return 1
}
if ! routine_pr >/dev/null; then
  printf 'retry the operator proposed no routine; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo alice "Please go ahead now: propose the routine in todo with add_routine.")
fi
check "it proposes it with add_routine, as alice" grep -Eq "mcp: alice: .*add_routine" <<<"$(mcp_calls "$since" alice)"
cpr=$(routine_pr)
check "it proposes a routine in todo's config, as alice" test -n "$cpr"
gitea_api "" "$hp/config/raw/routines.yaml?ref=$(gitea_api "" "$hp/config/pulls/${cpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)" > "$tmp/cron.yaml"
check "daily at 9, owned by alice, by one of garrison's agents" python3 -c '
import sys,yaml
r = yaml.safe_load(open(sys.argv[1])) or []
sys.exit(0 if any(str(x.get("schedule","")).split()[:2] == ["0","9"] and x.get("owner") == "alice" and x.get("agent") in ("operator","commander","sentry","scout","coder","media","marketing") for x in r) else 1)' "$tmp/cron.yaml"
check "keeping the routines there"         python3 -c 'import sys,yaml; sys.exit(0 if len(yaml.safe_load(open(sys.argv[1])) or []) == int(sys.argv[2]) + 1 else 1)' "$tmp/cron.yaml" "${routines_before:-0}"
cron_ok() {
  gitea_api "" "$hp/config/commits/$(gitea_api "" "$hp/config/pulls/${cpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "carol approves it"                  test "$(gitea_api carol -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Good."}' "$hp/config/pulls/${cpr:-0}/reviews")" = 200
check "the approval check passes"          until_true 120 cron_ok
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/config/pulls/${cpr:-0}"

while read -r c m; do
  for id in $(web -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$c/messages/$m/thread" | python3 -c 'import json,sys; [print(x["id"]) for x in json.load(sys.stdin)]' 2>/dev/null) $m; do
    web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$c/messages/$id/delete"
  done
done < "$tmp/posted"
}
