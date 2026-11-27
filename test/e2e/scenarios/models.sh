# The agents' models of a test run, chosen as a root admin does after the
# setup: on the dashboard's Models page, alice chooses which agents use which
# model, of the providers the setup added (PROVIDERS in .env), in one change.
# AGENT_MODELS (from test/vm/steps.sh, out of .env) names them, e.g.
# "coder=ollama-cloud/glm-5.3". Without it, every agent keeps the default.

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
  local cfg=$api/repos/garrison/config res pr pair agent model fields=() provider
  llm() { gitea_api "" "$cfg/raw/llm/$1?ref=main"; }
  check "alice signs in to her dashboard" until_true 60 home_login alice
  for pair in $AGENT_MODELS; do
    provider=${pair#*=}; provider=${provider%%/*}
    check "the setup added $provider" grep -q "^  $provider:" <<<"$(llm providers.yaml)"
  done
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
