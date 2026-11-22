# shellcheck shell=bash
# newsdemo: the news page of newsroom on the real web, with topics of your
# own, to see what the chain news-page finds. Not in the test tree: its
# results depend on the day's news. Run it on a clone of the fixture deploy,
# with NEWS_TOPICS naming a topics.md in the VM (default: the template's):
#   NEWS_TOPICS=/home/ubuntu/topics.md test/e2e/smoke.sh newsdemo
# It reports what news.yaml lists, and leaves everything running, so the
# page can be opened at https://news.<domain>/ (test/vm/dns.sh).
section_newsdemo() {
talk_setup || return 0

echo "# the page todo/news, with your topics"
render_app news todo news "$tmp/news"
[ -z "${NEWS_TOPICS:-}" ] || cp "$NEWS_TOPICS" "$tmp/news/topics.md"
check "alice pushes todo/news"             installer/push-repo.sh --update todo/news "$tmp/news"
check "it is private" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"private":true}' "$hp/news")" = 200
check "alice adds its stack, behind the sign-in" set_stack todo news "$tmp/news.stack.yaml"

echo "# its routines"
gitea_api "" -f "$hp/config/raw/grants.yaml" > "$tmp/grants.yaml" 2>/dev/null || : > "$tmp/grants.yaml"
cat >> "$tmp/grants.yaml" <<'YAML'
- agent: coder
  repo: news
  branch: main
  level: autonomous
  for: members
  unattended: true
YAML
gitea_api "" -f "$hp/config/raw/routines.yaml" > "$tmp/routines.yaml" 2>/dev/null || : > "$tmp/routines.yaml"
cat >> "$tmp/routines.yaml" <<'YAML'
- chain: news-page
  name: news-daily
  schedule: "0 7 * * *"
  owner: alice
  task: "The news page is the repo news of todo."
- chain: news-page
  name: news-topics
  on_push: {repo: news, branch: main, paths: [topics.md]}
  owner: alice
  task: "The news page is the repo news of todo; its topics.md changed."
YAML
npr=$(propose_config news-routines "The news page's routines" "grants.yaml=$tmp/grants.yaml" "routines.yaml=$tmp/routines.yaml")
check "a root admin approves them"         test "$(review config "" "$npr")" = 200
check "the approval check passes"          until_true 90 approval_is "$npr" success
check "alice merges them"                  merges config alice "$npr"
seen() { [ -n "$(kubectl --namespace garrison exec -i deployment/postgres -- psql -U postgres -d garrison -tAc "SELECT head FROM routine_state WHERE key = 'todo/coder/news-topics'" 2>/dev/null | tr -d '[:space:]')" ]; }
check "its routine has seen main"          until_true 180 seen

echo "# the agents look on the web"
printf '\n<!-- Look now -->\n' >> "$tmp/news/topics.md"
check "alice touches the topics"           test "$(set_file news main topics.md "$tmp/news/topics.md" "Look now")" = 200
news_yaml() { gitea_api "" -f "$hp/news/raw/news.yaml?ref=main"; }
some() { news_yaml | grep -q '^  - title:\|^- title:'; }
if ! until_true 1800 some; then
  echo "note  nothing listed after 30 minutes; the feed says what the chain did"
fi
echo "# news.yaml on main:"
news_yaml | sed 's/^/      /'
code=$(sso_login "$tmp/news.jar" "https://news.$DOMAIN/" "$TEST_PASSWORD" "$tmp/news.html" alice 2>&1)
built() { web -b "$tmp/news.jar" "https://news.$DOMAIN/" | grep -q 'from the last'; }
until_true 900 built || true
web -b "$tmp/news.jar" "https://news.$DOMAIN/" > "$tmp/page.html" && cp "$tmp/page.html" /home/ubuntu/news-page.html 2>/dev/null || true
echo "note  the page as alice sees it: /home/ubuntu/news-page.html in the VM"
}
