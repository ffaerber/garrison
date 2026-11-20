# shellcheck shell=bash
# operator: why an app does not run, a new app from nothing, data back.
section_operator() {
talk_setup || return 0
hcid=$(channel_of todo)
echo "# 7. the operator finds out why an app does not run"
# First the operator learns to look by itself when an app keeps failing.
printf -- '- agent: operator\n  name: on-failure\n  on_alert: { alertname: ServiceFailing }\n  owner: alice\n  task: An app of todo keeps failing. Find out why from its stack'"'"'s tasks and logs, and say what to change, naming the setting.\n' >> "$tmp/routines.yaml"
opr=$(propose_config operator "The operator looks when an app keeps failing" "routines.yaml=$tmp/routines.yaml")
check "carol approves the operator's alert routine" test "$(review config carol "$opr")" = 200
check "the approval check passes"          until_true 90 approval_is "$opr" success
check "alice merges it"                    test "$(merge config alice "$opr")" = 200
# Its source, todo/quotes, was pushed by the deploy step: todo has several
# people now, so nobody pushes to main of its repos any more.
check "alice adds its stack, carol approves" set_stack todo quotes test/e2e/apps/stacks/quotes.yaml carol
quotes_deployed() { garrison applied | grep -q "^app todo/quotes@main quotes "; }
check "its pipeline builds it and the deployer deploys it" until_true 900 quotes_deployed
quotes_crashes() { docker service ps quotes_web --format '{{.Error}}' 2>/dev/null | grep -q 'non-zero exit'; }
check "it does not start"                  until_true 120 quotes_crashes
am_alerts() { docker exec "$(docker ps -q -f name=prometheus_alertmanager | head -n1)" wget -qO- http://localhost:9093/api/v2/alerts; }
quotes_alert() { am_alerts | python3 -c '
import json,sys
sys.exit(0 if any(a["labels"].get("alertname") == "ServiceFailing" and a["labels"].get("app") == "quotes" and a["labels"].get("namespace") == "todo" for a in json.load(sys.stdin)) else 1)'; }
check "an alert fires: it keeps failing"   until_true 400 quotes_alert
check "todo's people see it in the feed"   until_true 120 in_feed carol ns=todo "e['kind'] == 'alert' and e['status'] == 'failed' and 'quotes_web' in e['title']"
# Who outside todo sees it is home's TestEachSeesWhatTheyMayRead: here bob
# and carol are both todo's (the fixture team).
alert_post() {
  chat_api alice "$chat/api/v1/channels/$hcid/messages?skip=0&limit=50" | python3 -c '
import json,sys
for m in json.load(sys.stdin):
    c = m.get("content") or ""
    if c.startswith("**operator**") and "Routine on-failure, for alice (the alert ServiceFailing fired" in c:
        print(c); sys.exit(0)
sys.exit(1)'
}
check "it starts the operator, which posts in todo's channel" until_true 600 alert_post
check "naming the missing setting"         grep -q QUOTES_FILE <<<"$(alert_post)"
check "the feed has its run"               in_feed carol ns=todo "e['kind'] == 'routine' and e['agent'] == 'todo/operator' and 'ServiceFailing' in e['title']"
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo alice "The quotes app of the namespace todo, https://quotes.$DOMAIN/, does not work. Find out why and tell me what to change.")
check "it looks at the stack, as alice"    grep -qE "mcp: alice: (stack_status|service_logs)" <<<"$(mcp_calls "$at" alice)"
check "it names the missing setting"       grep -q QUOTES_FILE <<<"$answer"
check "alice removes its stack, carol approves" set_stack todo quotes - carol
quotes_gone() { ! docker service inspect quotes_web >/dev/null 2>&1; }
check "that takes the app down"            until_true 120 quotes_gone
check "and the alert resolves"             until_true 300 in_feed carol ns=todo "e['kind'] == 'alert' and e['status'] == 'ok' and 'quotes_web' in e['title']"
gitea_api "" -o /dev/null -X DELETE "$hp/quotes"

echo "# 8. alice has the operator set up a new app, from nothing"
answer=$(ask_agent garrison alice "Set up a new app for us: make a namespace photos for our photo apps, and in it the app gallery, a small web app that shows who is visiting: the image traefik/whoami:v1.11 on port 80, behind the sign-in. It comes from elsewhere, so it needs no repo of its own. Propose its stack. You have everything you need; do not ask back.")
org_made() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/photos")" = 200 ]; }
# stack_pr: the open pull request of photos/config by alice that adds stacks/gallery.yaml.
stack_pr() {
  local n
  for n in $(gitea_api "" "$api/repos/photos/config/pulls?state=open&limit=10" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="alice"]' 2>/dev/null); do
    gitea_api "" "$api/repos/photos/config/pulls/$n/files" | grep -q '"filename":"stacks/gallery.yaml"' && { echo "$n"; return 0; }
  done
  return 1
}
if ! org_made || ! stack_pr >/dev/null; then
  printf 'retry the operator did not do both; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent garrison alice "Please go ahead now: create_namespace photos if it is missing, then propose stacks/gallery.yaml in photos/config with propose_change (image traefik/whoami:v1.11, port 80, entrypoint web, environment main private).")
fi
check "it makes the namespace photos, as alice" org_made
photos_admin() { gitea_api "" "$api/repos/photos/config/raw/members.yaml" | python3 -c 'import sys,yaml; sys.exit(0 if (yaml.safe_load(sys.stdin) or {}).get("admins") == ["alice"] else 1)'; }
check "with alice as its first admin"      until_true 180 photos_admin
gpr=$(stack_pr)
check "it proposes stacks/gallery.yaml in photos' config, as alice" test -n "$gpr"
check "no repo for it, and nothing in garrison" bash -c '[ "$0" = 404 ] && [ "$1" = 404 ]' "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/photos/gallery")" "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/garrison/gallery")"
gitea_api "" "$api/repos/photos/config/raw/stacks/gallery.yaml?ref=$(gitea_api "" "$api/repos/photos/config/pulls/${gpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)" > "$tmp/gallery.yaml"
check "one the deployer takes"             garrison validate --in photos "$tmp/gallery.yaml"
check "a root admin who did not write it approves" test "$(gitea_api "" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$api/repos/photos/config/pulls/${gpr:-0}/reviews")" = 200
photos_approved() {
  gitea_api "" "$api/repos/photos/config/commits/$(gitea_api "" "$api/repos/photos/config/pulls/${gpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "the approval check passes"          until_true 120 photos_approved
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$api/repos/photos/config/pulls/${gpr:-0}/merge")" = 200
gallery_deployed() { garrison applied | grep -q "^app photos/gallery@main gallery "; }
check "the deployer deploys it"            until_true 300 gallery_deployed
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent garrison bob "Make me a namespace bobs for my own apps.")
check "bob cannot make a namespace"        test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/orgs/bobs")" = 404
refused_ns() { refused_only "$since" bob create_namespace "only root admins may"; }
check "the gateway refused every try"      refused_ns

echo "# 9. alice has the operator bring todo's data back"
todo_ds=garrison/ns/todo/stacks/todo
was=$(zfs get -H -o value garrison:restored "$todo_ds")
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
answer=$(ask_agent todo alice "I deleted todos in the todo app by mistake. Bring todo's data on main back to the newest snapshot taken before a deploy. Propose it; I know newer data gets lost. Do not ask back.")
# restore_pr: the open pull request of todo/config by alice that changes restores/.
restore_pr() {
  local n
  for n in $(gitea_api "" "$hp/config/pulls?state=open&limit=10" | python3 -c 'import json,sys; [print(p["number"]) for p in json.load(sys.stdin) if p["user"]["login"]=="alice"]' 2>/dev/null); do
    gitea_api "" "$hp/config/pulls/$n/files" | grep -q '"filename":"restores/[^"]*\.yaml"' && { echo "$n"; return 0; }
  done
  return 1
}
if ! restore_pr >/dev/null; then
  printf 'retry the operator proposed no restore; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent todo alice "Please go ahead now: list_snapshots for the stack todo in todo, take the newest deploy-… one, and propose restores/todo.yaml in todo/config with propose_change (stack: todo, environment: main, snapshot: <its name>).")
fi
lists_snaps() { mcp_calls "$since" alice | grep -q "mcp: alice: list_snapshots"; }
check "it lists todo's snapshots, as alice" lists_snaps
rpr=$(restore_pr)
check "it proposes a restore in todo's config, as alice" test -n "$rpr"
rhead=$(gitea_api "" "$hp/config/pulls/${rpr:-0}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])' 2>/dev/null)
rfile=$(gitea_api "" "$hp/config/pulls/${rpr:-0}/files" | python3 -c 'import json,sys; [print(f["filename"]) for f in json.load(sys.stdin) if f["filename"].startswith("restores/")]' 2>/dev/null | head -n1)
rsnap=$(gitea_api "" "$hp/config/raw/$rfile?ref=$rhead" | python3 -c 'import sys,yaml; print((yaml.safe_load(sys.stdin) or {}).get("snapshot",""))' 2>/dev/null)
check "of a snapshot todo has"             zfs list "$todo_ds@${rsnap:-none}"
check "carol approves it"                  test "$(gitea_api carol -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Go ahead."}' "$hp/config/pulls/${rpr:-0}/reviews")" = 200
restore_approved() {
  gitea_api "" "$hp/config/commits/$rhead/status" | python3 -c '
import json,sys
sys.exit(0 if any(s["context"] == "garrison/approval" and s["status"] == "success" for s in json.load(sys.stdin).get("statuses") or []) else 1)'
}
check "the approval check passes"          until_true 120 restore_approved
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$hp/config/pulls/${rpr:-0}/merge")" = 200
restored_again() { v=$(zfs get -H -o value garrison:restored "$todo_ds"); [ "$v" != - ] && [ "$v" != "$was" ]; }
check "the deployer restores it"           until_true 300 restored_again
todo_back() { status_is 302 todo / 5; }
check "todo answers again"                 until_true 180 todo_back

}
