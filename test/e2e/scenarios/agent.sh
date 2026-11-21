# shellcheck shell=bash
# agent: the unit agents answer in their channels in Open WebUI, when
# mentioned, and propose through the gateway's MCP tools, with the
# rights of the person who mentioned them. They talk to the real
# LLM, so answers vary; the checks look at what the agents did (tools called,
# changes in Gitea), not at their wording.
section_agent() {
echo "# the unit agents"
if [ ! -s $ETC/llm-api-key ]; then
  echo "skip  no LLM key in $ETC/llm-api-key"
  return
fi
for u in alice bob; do check "$u signs in to Open WebUI" chat_login $u; done
# The gateway's own Open WebUI account; it may delete any message.
admin_token=$(curl -sS --max-time 20 --cacert "$ETC/tls.crt" --resolve "chat.$DOMAIN:443:127.0.0.1" -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "installer@$DOMAIN" "$PASSWORD")" \
  "$chat/api/v1/auths/signin" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
cid=$(system_channel)
: > "$tmp/posted"   # ids of the test's messages; ask runs in a subshell
# ask PERSON QUESTION: posts in the channel of garrison, mentioning its agent, as
# the chat page does, and waits for the agent's answer in the message's thread.
# Prints the answer.
ask() {
  local who=$1 question=$2 mid i
  mid=$(chat_api "$who" -X POST -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"content": "<@M:garrison/operator|garrison/operator> " + sys.argv[1]}))' "$question")" \
    "$chat/api/v1/channels/$cid/messages/post" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || return 1
  echo "$mid" >> "$tmp/posted"
  for i in $(seq 60); do
    chat_api "$who" "$chat/api/v1/channels/$cid/messages/$mid/thread" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    meta = m.get("meta") or {}
    if meta.get("model_id") == "garrison/operator" and meta.get("done"):
        print(m.get("content") or ""); sys.exit(0)
sys.exit(1)' && return 0
    sleep 5
  done
  return 1
}
reply() { cat; }
# Leaves the channel as it was: deletes the test's questions and their threads.
cleanup_channel() {
  local mid
  for mid in $(cat "$tmp/posted"); do
    for id in $(chat_api bob "$chat/api/v1/channels/$cid/messages/$mid/thread" | python3 -c 'import json,sys; [print(m["id"]) for m in json.load(sys.stdin)]' 2>/dev/null) $mid; do
      web -o /dev/null -X DELETE -H "Authorization: Bearer $admin_token" "$chat/api/v1/channels/$cid/messages/$id/delete"
    done
  done
}

# The model sometimes writes a tool call out as text instead of making it. A
# question whose answer shows no sign of the system is asked once more, and
# the retry is printed so it shows in the log.
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask bob "Which stacks run in the unit garrison? Use your tools and list their names." | reply)
if ! grep -qi grafana <<<"$answer"; then
  printf 'retry the agent answered %q; asking again\n' "$(head -c 200 <<<"$answer")"
  answer=$(ask bob "Which stacks run in the unit garrison? Call list_stacks and list their names." | reply)
fi
check "the agent answers bob in the channel" test -n "$answer"
check "its answer names grafana"           grep -qi grafana <<<"$answer"
check "it asked the gateway, as bob" grep -q "mcp: bob: " <<<"$(mcp_calls "$since" bob)"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask bob "Show me the last 20 log lines of the service grafana, and how much memory the stack grafana uses." | reply)
check "it reads logs for bob"              grep -q "mcp: bob: service_logs" <<<"$(mcp_calls "$since" bob)"
check "it reads metrics for bob"           grep -qE "mcp: bob: (service_usage|query_metrics)" <<<"$(mcp_calls "$since" bob)"
answer=$(ask bob "In the unit garrison, propose a change to the repo platform: add a file agent-bob.txt containing hello. Title: Agent smoke test bob." | reply)
check "bob's proposal is refused"          bash -c '! grep -q "Agent smoke test bob" <<<"$0"' "$(gitea_api "" "$api/repos/garrison/platform/pulls?state=open&limit=50")"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask alice "In the unit garrison, propose a change to the repo platform: add a file agent-alice.txt containing hello. Title: Agent smoke test alice." | reply)
check "the agent proposes alice's change, as alice" grep -q "mcp: alice: propose_change" <<<"$(mcp_calls "$since" alice)"
prs=$(gitea_api "" "$api/repos/garrison/platform/pulls?state=open&limit=50")
check "it waits as a pull request by alice" python3 -c 'import json,sys; sys.exit(not any(p["title"].startswith("Agent smoke test alice") and p["user"]["login"] == "alice" for p in json.load(sys.stdin)))' <<<"$prs"
check "nothing reached main"               test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/garrison/platform/contents/agent-alice.txt")" = 404
# On failure the answer is printed, so a model that leaves out the link shows what it said instead.
links() { grep -q "git.$DOMAIN/garrison/platform/pulls/" <<<"$1" || { echo "answer: $(tr '\n' ' ' <<<"$1" | head -c 400)"; return 1; }; }
check "the answer links the change"        links "$answer"
# Clean up: close the proposal.
pr=$(python3 -c 'import json,sys; print(next(p["number"] for p in json.load(sys.stdin) if p["title"].startswith("Agent smoke test alice")))' <<<"$prs" 2>/dev/null)
[ -n "$pr" ] && gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$api/repos/garrison/platform/pulls/$pr"

echo "# another LLM provider, through the chat"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask alice "Add the LLM provider smoke-chat to Garrison: its API is at http://127.0.0.1:9/v1." | reply)
check "the agent proposes it, as alice"    grep -q "mcp: alice: add_provider" <<<"$(mcp_calls "$since" alice)"
provider_pr=$(gitea_api "" "$api/repos/garrison/config/pulls?state=open&limit=50" | python3 -c '
import json,sys
print(next((p["number"] for p in json.load(sys.stdin) if p["title"] == "Add the LLM provider smoke-chat" and p["user"]["login"] == "alice"), ""))')
check "a pull request on providers.yaml, by alice" test -n "$provider_pr"
models_link() { grep -q "https://$DOMAIN/models?provider=smoke-chat" <<<"$1" || { echo "answer: $(tr '\n' ' ' <<<"$1" | head -c 400)"; return 1; }; }
check "the answer sends the key to the Models page" models_link "$answer"
[ -n "$provider_pr" ] && gitea_api "" -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$api/repos/garrison/config/pulls/$provider_pr"
cleanup_channel
check "the test left no messages in the channel" bash -c '[ "$(python3 -c "import json,sys; print(len([m for m in json.load(sys.stdin) if not (m.get(\"content\") or \"\").startswith(\"**How to ask the agent\")]))" <<<"$0")" = 0 ]' "$(chat_api bob "$chat/api/v1/channels/$cid/messages")"

echo "# the scout's browser, under gVisor"
# The command web of the sandbox image, as the scout runs it: Chromium
# under gVisor reads a page built by JavaScript and takes a picture of it.
# The page comes from a pod next to it, so no model and no internet.
image_pod smoke-page -d -- node -e '
require("http").createServer((q, r) => { r.setHeader("Content-Type", "text/html");
  r.end("<title>Smoke page</title><p id=p></p><script>document.getElementById(\"p\").textContent = \"built by \" + \"script\"</script>") }).listen(8000)'
page_ip=$(kubectl --namespace garrison-check get pod smoke-page -o jsonpath='{.status.podIP}')
read_page=$(image_pod smoke-read -g -- web read "http://$page_ip:8000/" 2>&1)
check "it reads a page built by JavaScript" bash -c 'grep -q "Title: Smoke page" <<<"$0" && grep -q "built by script" <<<"$0" || { echo "$0" | head -5; exit 1; }' "$read_page"
shot=$(image_pod smoke-shot -g -- sh -c "web shot http://$page_ip:8000/ /tmp/p.png 390x844 >/dev/null && head -c 8 /tmp/p.png | od -An -c" 2>&1)
check "and takes a picture of it"          grep -q P <<<"$shot"
kubectl --namespace garrison-check delete pod smoke-page --wait=false >/dev/null 2>&1
}
