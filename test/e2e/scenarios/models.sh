# The agents' models of a test run, chosen as a root admin does after the
# install: on the dashboard's Models page, alice adds Ollama's cloud with its
# key, the model sync lists its models by itself, and she chooses which
# agents use which model, in one change. AGENT_MODELS (from test/vm/steps.sh)
# names them, e.g. "coder=ollama-cloud/glm-5.3"; the key is the file
# $ETC/test-provider-key, which this removes. Without AGENT_MODELS, every
# agent keeps the install's model.

# models_form FIELD=VALUE...: the Models page's form, as alice; prints the
# status and where it leads. A field NAME@FILE takes its value from FILE.
models_form() {
  local a=() f
  for f in "$@"; do a+=(--data-urlencode "$f"); done
  web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code} %{redirect_url}' -H "Origin: https://$DOMAIN" -X POST "${a[@]}" "https://$DOMAIN/models"
}

# config_pr_in RES: the pull request of garrison/config a form led to.
config_pr_in() { grep -oE "/c/garrison/config/[0-9]+$" <<<"$1" | grep -oE '[0-9]+$'; }

# approve_and_merge PR: the install's admin approves the change alice
# proposed, and she merges it once garrison/approval says so.
approve_and_merge() {
  local cfg=$api/repos/garrison/config st i
  [ "$(gitea_api "" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$cfg/pulls/$1/reviews")" = 200 ] || { echo "cannot approve #$1"; return 1; }
  for i in $(seq 60); do
    st=$(gitea_api "" "$cfg/commits/$(gitea_api "" "$cfg/pulls/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"], s["description"]); break' 2>/dev/null)
    case $st in success*) break ;; failure*) echo "approval: $st"; return 1 ;; esac
    sleep 2
  done
  for i in $(seq 15); do
    [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$cfg/pulls/$1/merge")" = 200 ] && return 0
    sleep 2
  done
  echo "cannot merge #$1 ($st)"; return 1
}

section_models() {
  [ -n "${AGENT_MODELS:-}" ] || { echo "skip  no AGENT_MODELS: the agents keep the install's model"; return 0; }
  echo "# the agents' models, chosen on the dashboard's Models page: $AGENT_MODELS"
  local cfg=$api/repos/garrison/config key=$ETC/test-provider-key res pr pair agent model fields=() provider
  llm() { gitea_api "" "$cfg/raw/llm/$1?ref=main"; }
  check "alice signs in to her dashboard" until_true 60 home_login alice
  for pair in $AGENT_MODELS; do
    provider=${pair#*=}; provider=${provider%%/*}
    [ "$provider" = ollama-cloud ] || { bad "only Ollama's cloud is set up by the test: $pair"; return 0; }
  done
  if ! llm providers.yaml | grep -q '^  ollama-cloud:'; then
    [ -s "$key" ] || { bad "Ollama's cloud needs its key: OLLAMA_API_KEY in .env"; return 0; }
    res=$(models_form action=add name=ollama-cloud url=https://ollama.com/v1 "key@$key")
    pr=$(config_pr_in "$res")
    check "alice adds Ollama's cloud with its key; she lands on its change" test -n "$pr"
    check "the install's admin approves it, alice merges it" approve_and_merge "${pr:-0}"
  fi
  rm -f "$key"
  for pair in $AGENT_MODELS; do
    agent=${pair%%=*} model=${pair#*=}
    listed() { llm models.yaml | grep -qF "\"$1\": {"; }
    check "the model sync lists $model on main by itself" until_true 300 listed "$model"
    fields+=("agent-$agent=$model")
  done
  # shellcheck disable=SC2046
  check "the Models page offers them" bash -c 'page=$1; shift; for m; do grep -qF "<option value=\"$m\"" <<<"$page" || exit 1; done' _ "$(feed alice /models)" $(for p in $AGENT_MODELS; do echo "${p#*=}"; done)
  res=$(models_form action=choose "default=$(llm providers.yaml | sed -n 's/^default: *"\{0,1\}\([^"]*\)"\{0,1\}.*/\1/p')" "${fields[@]}")
  pr=$(config_pr_in "$res")
  check "alice chooses them in one change" test -n "$pr"
  check "the install's admin approves it, alice merges it" approve_and_merge "${pr:-0}"
  for pair in $AGENT_MODELS; do
    agent=${pair%%=*} model=${pair#*=}
    check "$agent uses $model" grep -qF "model: \"$model\"" <<<"$(gitea_api "" "$cfg/raw/agents/$agent/agent.yaml?ref=main")"
    check "which is turned on" grep -qF "\"$model\": {enabled: true" <<<"$(llm models.yaml)"
  done
}
