# shellcheck shell=bash
# invites: links for people, sent by hand (Garrison sends no mail). Here,
# who may make them and what a link refuses; in the browser
# (test/ui/invites.py), carol inviting frank into todo, frank joining, the
# pull request in carol's name that alice approves, bob's password link and
# a revoked one.

# people_post WHO FIELD=VALUE...: the People page's form, as WHO; prints the
# status, the page in $tmp/people-WHO.html.
people_post() {
  local who=$1 a=() f; shift
  for f in "$@"; do a+=(--data-urlencode "$f"); done
  web -b "$tmp/home-$who.jar" -o "$tmp/people-$who.html" -w '%{http_code}' -H "Origin: https://$DOMAIN" -X POST "${a[@]}" "https://$DOMAIN/people"
}
# link_of WHO: the link the last form of WHO made.
link_of() { grep -oE "https://$DOMAIN/join\?token=[0-9a-f]{64}" "$tmp/people-$1.html" | head -n1; }
# join_post FIELD=VALUE...: the link's form; prints the status, the page in $tmp/join.html.
join_post() {
  local a=() f
  for f in "$@"; do a+=(--data-urlencode "$f"); done
  web -o "$tmp/join.html" -w '%{http_code}' -H "Origin: https://$DOMAIN" -X POST "${a[@]}" "https://$DOMAIN/join"
}

section_invites() {
  echo "# who may make links"
  local link token
  for who in alice bob carol; do check "$who signs in to the dashboard" until_true 60 home_login "$who"; done
  check "bob, a member, may not invite"       test "$(people_post bob action=invite unit=todo role=member)" = 403
  check "carol may not invite without a unit" test "$(people_post carol action=invite unit=)" = 403
  check "nor into garrison"                   test "$(people_post carol action=invite unit=garrison role=member)" = 403
  check "carol may not make password links"   test "$(people_post carol action=reset username=bob)" = 403
  check "nor from another site's page" test "$(web -b "$tmp/home-alice.jar" -o /dev/null -w '%{http_code}' -H "Origin: https://hello.$DOMAIN" -X POST -d action=invite -d unit= "https://$DOMAIN/people")" = 403
  check "a wrong token opens nothing"         test "$(web -o /dev/null -w '%{http_code}' "https://$DOMAIN/join?token=$(printf '0%.0s' {1..64})")" = 404
  check "an invite's link is shown once"      test "$(people_post alice action=invite unit= note=once)" = 200
  link=$(link_of alice); token=${link##*token=}
  check "and is not on the page again"        bash -c '[ -n "$0" ] && ! grep -qF "$0" <<<"$1"' "$token" "$(feed alice /people)"
  check "it does not take a weak password"    test "$(join_post "token=$token" username=weak password=short again=short)" = 400
  check "nor someone else's username"         test "$(join_post "token=$token" username=carol "password=long-enough-pass" "again=long-enough-pass")" = 400
  check "nor the platform's account"          test "$(join_post "token=$token" "username=$ADMIN_USER" "password=long-enough-pass" "again=long-enough-pass")" = 400
  ui_run invites
  check "the feed says frank joined, invited by carol" until_true 30 in_feed alice "" "e['kind'] == 'people' and e['title'].startswith('frank joined Garrison, invited by carol')"
}
