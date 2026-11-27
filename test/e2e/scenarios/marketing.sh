# shellcheck shell=bash
# marketing: the marketing agent shoots the app, follows its skill, keeps to its rights, works in a sandbox, and shoots by itself.
section_marketing() {
talk_setup || return 0
echo "# 4b. bob asks the marketing agent for a screenshot"
# The marketing agent comes with a shared skill for homepage screenshots.
cat > "$tmp/skill.md" <<'SKILL'
---
name: homepage-screenshots
description: Screenshots of an app for the homepage, one for desktop and one for phones. Use it whenever someone asks for homepage screenshots of an app.
---
For the app asked about, take two screenshots of https://<app>.<domain>/ with the screenshot tool, into the repo with the app's code (ask if you do not know it):

1. Desktop: width 1440, height 900, path docs/screenshots/<app>-desktop.png, title "Desktop screenshot of <app>".
2. Phone: width 390, height 844, path docs/screenshots/<app>-phone.png, title "Phone screenshot of <app>".

Each becomes its own pull request. Give both links.
SKILL
mpr=$(add_agent marketing marketing 'list_stacks, screenshot' "Makes screenshots of the unit's apps" \
  "You make screenshots of the unit's apps for its homepage and docs. Each app answers at https://<stack>.<domain>/; list_stacks shows them. Put each screenshot where the person asks." \
  homepage-screenshots "skills/homepage-screenshots/SKILL.md=$tmp/skill.md")
check "a root admin approves the marketing agent in garrison, alice merges it" lands "$mpr"
check "Open WebUI has the marketing agent"     until_true 120 has_model garrison/marketing
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
shooter_from=$at  # the channel's conversation with the marketing agent begins
answer=$(ask_agent todo bob "Take one screenshot of https://todo.$DOMAIN/ and put it into the repo app as docs/screenshots/todo.png. Title: Screenshot of the todo app. Just this one, not the homepage screenshots of your skill." marketing)
spr=$(pr_with app bob "$since" docs/screenshots/todo.png | cut -d' ' -f1)
if [ -z "$spr" ]; then
  printf 'retry the agent opened no pull request with todo.png; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please take that one screenshot now with the screenshot tool: url https://todo.$DOMAIN/, unit todo, repo app, path docs/screenshots/todo.png." marketing)
  spr=$(pr_with app bob "$since" docs/screenshots/todo.png | cut -d' ' -f1)
fi
check "the marketing agent takes it, as bob"   grep -q "mcp: bob: screenshot" <<<"$(mcp_calls "$at" bob)"
check "it proposes the screenshot, as bob" test -n "$spr"
check "the pull request holds a PNG"       test "$(png_size app docs/screenshots/todo.png "$(head_sha app "$spr")")" != none
body=$(gitea_api "" "$hp/app/pulls/$spr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["body"])')
check "it shows the todo app, signed in as bob" \
  bash -c 'grep -q "Screenshot of https://todo.$1/ as bob" <<<"$0" && grep -q "page title \"Todo\"" <<<"$0"' "$body" "$DOMAIN"
check "the answer links it"                grep -q "/todo/app/pulls/$spr" <<<"$answer"
# Close it, and whatever else it proposed (a model may follow its skill
# unasked), so 4c starts afresh.
for n in $(gitea_api "" "$hp/app/pulls?state=open&limit=50" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="bob" and p["number"] > int(sys.argv[1])]' "$since"); do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$n"
done

echo "# 4b2. a screenshot to see, in the chat"
# Without a repo, the screenshot is shown, not proposed: the answer has the
# picture, at the dashboard's /shots/<id>.png, which bob may open and
# someone outside todo may not.
since=$(last_pr app)
answer=$(ask_agent todo bob "Show me a screenshot of https://todo.$DOMAIN/ here in the chat. Do not put it into a repo." marketing)
shot=$(grep -oE "https://$DOMAIN/shots/[0-9a-f]{32}\.png" <<<"$answer" | head -n1)
check "the answer shows the picture"       bash -c '[ -n "$0" ] && grep -qF "]($0)" <<<"$1"' "$shot" "$answer"
shot_code() { feed "$1" /api/events?limit=1 >/dev/null; web -b "$tmp/home-$1.jar" -o /dev/null -w '%{http_code}' "$shot"; }
check "bob may open it"                    test "$(shot_code bob)" = 200
check "nothing was proposed for it"        test "$(last_pr app)" = "$since"

echo "# a snapshot on request, from the dashboard's Apps page"
# bob, a member of todo, takes a snapshot of its app now: it needs no
# review, as it changes no data.
snap_res=$(feed bob /api/events?limit=1 >/dev/null; web -b "$tmp/home-bob.jar" -o /dev/null -w '%{redirect_url}' -H "Origin: https://$DOMAIN" -X POST -d unit=todo -d stack=todo "https://$DOMAIN/apps/snapshot")
snap=$(grep -oE 'manual-[0-9]{8}-[0-9]{6}' <<<"$(python3 -c 'import sys,urllib.parse; print(urllib.parse.unquote_plus(sys.argv[1]))' "$snap_res")" | head -n1)
check "bob takes a snapshot of todo"       test -n "$snap"
snap_there() { zfs list -t snapshot -H -o name 2>/dev/null | grep -q "/stacks/todo@$snap$"; }
check "it is there, on the machine"        until_true 60 snap_there
check "the feed says bob took it"          until_true 30 in_feed alice ns=todo "e['kind'] == 'snapshot' and e['actor'] == 'bob' and '$snap' in e['title']"

echo "# 4c. the marketing agent follows its skill"
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Make the homepage screenshots of the todo app, new ones: earlier ones were closed. Its code is in the repo app." marketing)
if ! pr_with app bob "$since" docs/screenshots/todo-phone.png >/dev/null || ! pr_with app bob "$since" docs/screenshots/todo-desktop.png >/dev/null; then
  printf 'retry the agent did not make both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "Please load your skill homepage-screenshots and follow it for the app todo now, into the repo app: both screenshots." marketing)
fi
# Its sandbox keeps the conversation: a skill loaded for 4b is still known.
check "it has loaded the skill, as bob"    grep -q "mcp: bob: load_skill" <<<"$(mcp_calls "$shooter_from" bob)"
read -r dpr dsha <<<"$(pr_with app bob "$since" docs/screenshots/todo-desktop.png)"
read -r ppr psha <<<"$(pr_with app bob "$since" docs/screenshots/todo-phone.png)"
check "the desktop screenshot is 1440x900" test "$(png_size app docs/screenshots/todo-desktop.png "$dsha")" = 1440x900
check "the phone screenshot is 390x844"    test "$(png_size app docs/screenshots/todo-phone.png "$psha")" = 390x844
for n in $dpr $ppr; do
  gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/$n"
done

echo "# 4d. what the marketing agent may not do"
platform_prs() { gitea_api "" "$api/repos/garrison/platform/pulls?state=all&limit=1" | python3 -c 'import json,sys; l=json.load(sys.stdin); print(l[0]["number"] if l else 0)'; }
before=$(platform_prs)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Take a screenshot of https://todo.$DOMAIN/ and put it into the unit garrison, repo platform, as docs/todo.png." marketing)
check "bob gets no screenshot into a repo he may not change" test "$(platform_prs)" = "$before"
check "the gateway refused every try"      refused_only "$at" bob screenshot "not allowed"
check "the agent says so"                  test -n "$answer"
since=$(last_pr app)
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo bob "Take a screenshot of https://example.org/ and put it into the repo app as docs/screenshots/example.png." marketing)
check "a page outside Garrison is not shot" test -z "$(newest_pr app bob "$since")"
check "the gateway refused every try"      refused_only "$at" bob screenshot "only pages at"
check "the feed shows bob the refusals"    until_true 60 home_login bob
check "among the problems"                 in_feed bob view=problems "e['kind'] == 'screenshot' and e['status'] == 'refused' and e['agent'] == 'todo/marketing' and e['actor'] == 'bob'"

echo "# 4e. the marketing agent works in a sandbox of its own"
# sandbox AGENT: the running sandbox of AGENT in todo's channel.
sb=$(sandbox_of todo/marketing)
check "the channel's turns with it ran in its sandbox" test -n "$sb"
check "the sandbox holds no LLM key and no token" \
  bash -c 'k=$(cat "$1"); p() { kubectl --namespace garrison-sandboxes "$@"; }; { [ ${#k} -lt 16 ] || ! p get pod "$0" -o yaml | grep -qF -- "$k"; } && [ "$(p exec "$0" -- env | grep "^GARRISON_" | cut -d= -f1 | sort | tr "\n" " ")" = "GARRISON_API GARRISON_CONTEXT GARRISON_MAX_TOKENS GARRISON_MODEL GARRISON_PROMPT GARRISON_TOKEN " ] && ! p exec "$0" -- ls /etc/garrison 2>/dev/null && ! p exec "$0" -- ls /var/run/secrets/kubernetes.io 2>/dev/null' "$sb" "$ETC/llm-api-key"
check "it runs under gVisor"               test "$(kubectl --namespace garrison-sandboxes get pod "$sb" -o jsonpath='{.spec.runtimeClassName}')" = gvisor
check "as root, in gVisor's own kernel"    bash -c "$(declare -f in_sandbox); [ \"\$(in_sandbox $sb id -u)\" = 0 ] && in_sandbox $sb dmesg | grep -q gVisor"
check "it cannot reach the internet"       bash -c "$(declare -f in_sandbox); ! in_sandbox $sb curl -sS -m 5 -o /dev/null https://example.org"
check "not even past the proxy"            bash -c "$(declare -f in_sandbox); ! in_sandbox $sb curl -sS -m 5 -o /dev/null --noproxy '*' https://1.1.1.1"
# Through the proxy, which refuses them, and around it; -f: a refusal is an
# answer too.
nothing_at() { ! in_sandbox "$sb" curl -fsS -m 5 -o /dev/null "$1" && ! in_sandbox "$sb" curl -fsS -m 5 -o /dev/null --noproxy "*" "$1"; }
check "nor Gitea, nor the Kubernetes API"  bash -c "$(declare -f in_sandbox nothing_at); sb=$sb; nothing_at http://gitea.garrison.svc.cluster.local:3000 && nothing_at https://kubernetes.default.svc.cluster.local/version"
check "between turns its token counts for nothing" \
  test "$(in_sandbox "$sb" sh -c 'curl -sS -m 5 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $GARRISON_TOKEN" "$GARRISON_API/tools"')" = 401
has_notes() { in_sandbox "$(sandbox_of todo/marketing)" grep -q "pelican 42" /data/work/notes.txt; }
answer=$(ask_agent todo bob "Please keep a note for our next screenshots in your workspace: use bash to write the line 'pelican 42' (the name of the next release) into notes.txt, then say done." marketing)
if ! has_notes; then
  printf 'retry the agent wrote no notes.txt; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo bob "That note is just for you and me: please run echo 'pelican 42' > notes.txt in your workspace now." marketing)
fi
check "it writes a file in its workspace"  has_notes
# Its pod goes, as when it is idle; the workspace (its claim) and the conversation stay.
# The new pod has the old one's name, which comes from the chat; its uid is new.
pod_uid() { kubectl --namespace garrison-sandboxes get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
old_uid=$(pod_uid "$sb")
kubectl --namespace garrison-sandboxes delete pod "$sb" --wait >/dev/null
answer=$(ask_agent todo bob "Which line did you write into notes.txt just before? Check your workspace." marketing)
new_pod() { local p; p=$(sandbox_of todo/marketing) && [ -n "$p" ] && [ "$(pod_uid "$p")" != "$old_uid" ]; }
check "a new pod resumes the chat"         new_pod
check "it still has the file"              grep -q "pelican 42" <<<"$answer"
# Grafana's Agents dashboard: the sandbox's metrics carry its agent and
# person; its output, the conversation, stays out of Loki.
prom_value() { grafana_api "" "$g/api/datasources/proxy/uid/prometheus/api/v1/query?query=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1]))' "$1")" | python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)'; }
shooter_measured() { [ "$(prom_value 'count(container_memory_working_set_bytes{namespace="garrison-sandboxes", container!=""} * on (namespace, pod) group_left (agent, person) garrison_sandbox_info{agent="todo/marketing", person="bob"})')" != 0 ]; }
check "Prometheus measures the sandbox, by agent and person" until_true 120 shooter_measured
sandbox_logs() { grafana_api "" "$g/api/datasources/proxy/uid/loki/loki/api/v1/query_range?limit=5&since=2h&query=$(python3 -c 'import urllib.parse; print(urllib.parse.quote("{namespace=\"garrison-sandboxes\"}"))')"; }
check "Loki holds nothing a sandbox printed" bash -c 'grep -q "\"result\":\[\]" <<<"$0"' "$(sandbox_logs)"

echo "# 4j3. a routine takes a screenshot, signed in as the agent, with nobody watching"
# The marketing agent gets a grant on app that routines may use, and a routine
# that shoots todo, which is behind the sign-in, when the branch shots
# moves. Its browser signs in with the agent's own account in Keycloak.
check "alice makes the branch shots of app" make_branch alice "$hp/app" shots
gitea_api "" "$hp/config/raw/grants.yaml" > "$tmp/grants.yaml"
printf -- '- {agent: marketing, repo: app, level: propose, unattended: true}\n' >> "$tmp/grants.yaml"
cat >> "$tmp/routines.yaml" <<YAML
- agent: marketing
  name: shots
  on_push: {repo: app, branch: shots}
  owner: bob
  task: "Call screenshot once: url https://todo.$DOMAIN/, unit todo, repo app, path docs/screenshots/routine.png, title Screenshot by the routine. Then say in one sentence what the page shows."
YAML
spr=$(propose_config marketing-routine "The marketing agent shoots todo by itself" "grants.yaml=$tmp/grants.yaml" "routines.yaml=$tmp/routines.yaml")
check "carol approves it in todo"          test "$(review config carol "$spr")" = 200
check "the approval check passes"          until_true 90 approval_is "$spr" success
check "alice merges it"                    test "$(merge config alice "$spr")" = 200
check "the marketing agent gets an account in Gitea" until_true 120 has_account agent-todo-marketing
# kc_user NAME: whether the Keycloak account NAME is enabled (true, false, or nothing).
kc_user() {
  kubectl --namespace garrison exec -i deployment/keycloak -- bash -c '
    KC_CLI_PASSWORD=$(</run/secrets/admin-password) /opt/keycloak/bin/kcadm.sh config credentials --config /tmp/smoke.config \
      --server http://localhost:8080 --realm master --user "$0" >/dev/null 2>&1 &&
    /opt/keycloak/bin/kcadm.sh get users --config /tmp/smoke.config -r garrison -q "username=$1" -q exact=true --fields enabled --format csv --noquotes' "$ADMIN_USER" "$1"
}
kc_enabled() { [ "$(kc_user "$1")" = true ]; }
check "and the same account in Keycloak"   until_true 120 kc_enabled agent-todo-marketing
shots_seen() { kubectl --namespace garrison exec -i deployment/postgres -- psql -U postgres -d garrison -tAc "SELECT head FROM routine_state WHERE key = 'todo/marketing/shots'" | grep -q .; }
check "the routine sees shots"             until_true 180 shots_seen
since=$(last_pr app)
shot_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf 'Shoot.\n' > "$tmp/shoot.md"
check "alice pushes to shots"              test "$(set_file app shots SHOOT.md "$tmp/shoot.md" "Shoot")" = 201
routine_shot() { pr_with app agent-todo-marketing "$since" docs/screenshots/routine.png; }
check "the routine proposes the screenshot, with the agent's account" until_true 600 routine_shot
read -r rspr rsha <<<"$(routine_shot)"
check "a PNG"                              test "$(png_size app docs/screenshots/routine.png "${rsha:-none}")" != none
body=$(gitea_api "" "$hp/app/pulls/${rspr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["body"])')
check "of the todo app behind the sign-in, as the agent, for bob" \
  bash -c 'grep -q "Screenshot of https://todo.$1/ as the agent todo/marketing" <<<"$0" && grep -q "page title \"Todo\"" <<<"$0" && grep -q "Asked by bob through todo/marketing, routine todo/marketing/shots, with nobody watching" <<<"$0"' "$body" "$DOMAIN"
check "the gateway shot it as the agent"   bash -c 'kubectl --namespace garrison logs deployment/gateway --since-time="$0" 2>&1 | grep -q "mcp: bob (nobody watching, as agent-todo-marketing): todo/marketing: screenshot \[routine todo/marketing/shots\]"' "$shot_at"
gitea_api alice -o /dev/null -X PATCH -H 'Content-Type: application/json' -d '{"state":"closed"}' "$hp/app/pulls/${rspr:-0}"

}
