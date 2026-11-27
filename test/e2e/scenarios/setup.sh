# The setup in the browser, right after the install, as a person completes
# it: the link the installer printed, the first root admin, the AI providers,
# each tested with its key, and the default model, tested with a question.
# From test/vm/steps.sh, out of .env: SETUP_PROVIDERS (PROVIDERS, "name=url
# ...", GATEWAY standing for this VM's gateway), SETUP_DEFAULT (DEFAULT_MODEL),
# and each provider's key, if it has one, in $ETC/test-provider-keys/<name>.

# setup_form FIELD=VALUE...: the setup's form, with the cookie of its link;
# prints the status and where it leads, the page in $tmp/setup.html.
setup_form() {
  local a=() f
  for f in "$@"; do a+=(--data-urlencode "$f"); done
  web -b "$tmp/setup.jar" -c "$tmp/setup.jar" -o "$tmp/setup.html" -w '%{http_code} %{redirect_url}' \
    -H "Origin: https://$DOMAIN" -X POST "${a[@]}" "https://$DOMAIN/setup"
}

section_setup() {
  echo "# the setup in the browser"
  local link pr res name url keys=$ETC/test-provider-keys password
  link=$(cat "$ETC/setup-link" 2>/dev/null)
  check "the installer wrote the setup's link, root only" bash -c '[[ $0 == "https://'"$DOMAIN"'/setup?token="* ]] && [ "$(stat -c %a '"$ETC"'/setup-link)" = 600 ]' "$link"
  # The dashboard may still be starting right after the install.
  closed() { [ "$(web -o /dev/null -w '%{http_code}' "https://$DOMAIN/setup")" = 403 ]; }
  check "the setup is closed without its token" until_true 300 closed
  check "and with a wrong one"          test "$(web -o /dev/null -w '%{http_code}' "https://$DOMAIN/setup?token=wrong")" = 403
  check "the rest of the dashboard stays behind the sign-in" bash -c '[ "$(web -o /dev/null -w "%{http_code}" "https://'"$DOMAIN"'/models")" != 200 ]'
  check "the link opens it"             grep -q '^303 https://'"$DOMAIN"'/setup$' <<<"$(web -c "$tmp/setup.jar" -o /dev/null -w '%{http_code} %{redirect_url}' "$link")"
  check "it asks for the first root admin" grep -q 'Make the root admin' <<<"$(web -b "$tmp/setup.jar" "https://$DOMAIN/setup")"

  # The first root admin: a person of their own, not the platform's account.
  password=$(openssl rand -hex 12)
  (umask 077; printf '%s' "$password" > "$ETC/test-owner-password")
  check "owner becomes the first root admin" grep -q '^303' <<<"$(setup_form action=admin username=owner "email=owner@$DOMAIN" "password=$password" "again=$password")"
  check "the platform's account cannot be it" grep -q '^4' <<<"$(setup_form action=admin username=admin "email=a@$DOMAIN" "password=$password" "again=$password")"

  # The providers, each tested as it is added; a wrong key is refused.
  for pair in $SETUP_PROVIDERS; do
    name=${pair%%=*} url=$(vm_url "${pair#*=}")
    if [ -s "$keys/$name" ]; then
      check "$name with a wrong key is refused" bash -c 'grep -q "^400" <<<"$0" && grep -q "The test failed" '"$tmp/setup.html" "$(setup_form action=provider "name=$name" "url=$url" key=wrong)"
      res=$(setup_form action=provider "name=$name" "url=$url" "key@$keys/$name")
    else
      res=$(setup_form action=provider "name=$name" "url=$url")
    fi
    check "$name works: it lists its models" grep -q "^303 .*works" <<<"$res"
  done
  rm -rf "$keys"
  check "the page offers their models for the default" grep -qF "value=\"$SETUP_DEFAULT\"" <<<"$(web -b "$tmp/setup.jar" "https://$DOMAIN/setup")"

  # The default, tested with a question, then saved.
  check "the default answers a question" bash -c 'grep -q "^200" <<<"$0" && grep -q "It answers" '"$tmp/setup.html" "$(setup_form action=try "default=$SETUP_DEFAULT")"
  check "saving completes the setup"     bash -c 'grep -q "^200" <<<"$0" && grep -q "Garrison is ready" '"$tmp/setup.html" "$(setup_form action=save "default=$SETUP_DEFAULT")"
  llm() { gitea_api "" "$api/repos/garrison/config/raw/llm/$1?ref=main"; }
  check "garrison's config has the providers and the default" grep -qF "default: \"$SETUP_DEFAULT\"" <<<"$(llm providers.yaml)"
  check "the default is on, the other models off" bash -c 'grep -qF "\"$1\": {enabled: true" <<<"$0" && grep -q "{enabled: false" <<<"$0"' "$(llm models.yaml)" "$SETUP_DEFAULT"
  check "the setup is closed now, even with its link" grep -q '^303' <<<"$(web -o /dev/null -w '%{http_code}' "$link")"
  check "owner signs in; the dashboard first has Gitea make the account" until_true 60 owner_first_visit
  check "after opening Gitea once, the dashboard is owner's" until_true 60 owner_dashboard
}

# owner_first_visit: the first root admin signs in to the dashboard, which
# sends them to make their Gitea account first.
owner_first_visit() {
  sso_login "$tmp/home-owner.jar" "https://$DOMAIN/" "$(cat "$ETC/test-owner-password")" "$tmp/home-owner.html" owner >/dev/null 2>&1
  grep -q "Open Gitea once" "$tmp/home-owner.html"
}

# owner_dashboard: owner opens Gitea through its sign-in, as the page's
# button does, and then reads the feed.
owner_dashboard() {
  sso_login "$tmp/git-owner.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$(cat "$ETC/test-owner-password")" "$tmp/git-owner.html" owner >/dev/null 2>&1
  web -b "$tmp/home-owner.jar" "https://$DOMAIN/api/events?limit=1" | python3 -c 'import json,sys; json.load(sys.stdin)'
}
