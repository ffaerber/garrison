# shellcheck shell=bash
# login: people sign in through Keycloak and get exactly their rights.
section_login() {
echo "# signing in"
check "the garrison realm serves OIDC discovery" status_is 200 keycloak /realms/garrison/.well-known/openid-configuration
check "Gitea settings need a login"        bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 https://git.$DOMAIN/user/settings)\" != 200 ]"
sso_login "$tmp/git.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$PASSWORD" "$tmp/git.html" >/dev/null 2>&1
check "the admin signs in to Gitea through Keycloak" test "$(web -b "$tmp/git.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/user/settings")" = 200

echo "# roles in Gitea"

for u in alice bob; do
  sso_login "$tmp/$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$TEST_PASSWORD" "$tmp/$u.html" $u >/dev/null 2>&1
  check "$u signs in to Gitea through Keycloak" test "$(web -b "$tmp/$u.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/user/settings")" = 200
done
check "alice is a Gitea admin"             grep -q '"is_admin":true'  <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/alice")"
check "bob is not a Gitea admin"           grep -q '"is_admin":false' <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/bob")"
check "every system stack is a repo of the garrison organisation" \
  bash -c 'for n in postgres traefik keycloak sso gitea runner control-plane grafana prometheus loki; do grep -q "\"full_name\":\"garrison/$n\"" <<<"$0" || { echo "missing garrison/$n"; exit 1; }; done' "$(gitea_api "" "$api/orgs/garrison/repos?limit=50")"
check "the traefik repo holds its stack"   test "$(gitea_api "" -o /dev/null -w '%{http_code}' $repo/contents/kubernetes.yaml)" = 200
check "alice can administer the garrison repos"    grep -q '"permissions":{"admin":true' <<<"$(gitea_api alice $repo)"
check "alice can change a garrison repo" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"YWxpY2UK","message":"smoke test","new_branch":"smoke-'$RANDOM$RANDOM'"}' $repo/contents/smoke-test.txt)" = 201
check "bob can see the garrison repos, read-only"  grep -q '"permissions":{"admin":false,"push":false,"pull":true}' <<<"$(gitea_api bob $repo)"
check "bob can read the system stack"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' $repo/contents/kubernetes.yaml)" = 200
check "bob can see it in the browser too"  test "$(web -b "$tmp/bob.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/garrison/traefik")" = 200
check "bob cannot change a garrison repo" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Ym9iCg==","message":"smoke test"}' $repo/contents/bob-was-here.txt)" = 403
check "bob cannot change it on a new branch either" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Ym9iCg==","message":"smoke test","new_branch":"bob"}' $repo/contents/bob-was-here.txt)" = 403
check "bob's change did not land"          test "$(gitea_api "" -o /dev/null -w '%{http_code}' $repo/contents/bob-was-here.txt)" = 404
check "bob cannot change the repo's settings" test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"description":"bob"}' $repo)" = 403
check "someone not signed in cannot read the garrison repos" bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 $repo)\" != 200 ]"
check "someone not signed in cannot browse it either" bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 https://git.$DOMAIN/garrison/traefik)\" != 200 ]"
check "bob cannot make himself admin"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"login_name":"bob","source_id":0,"admin":true}' "https://git.$DOMAIN/api/v1/admin/users/bob")" = 403
check "bob is still not a Gitea admin"     grep -q '"is_admin":false' <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/bob")"

echo "# Open WebUI"
check "Open WebUI is healthy over valid TLS" status_is 200 chat /health 300
check "it sends visitors to the Keycloak login" \
  grep -q "^https://keycloak.$DOMAIN/realms/garrison/protocol/openid-connect/auth" <<<"$(web -o /dev/null -w '%{redirect_url}' "$chat/oauth/oidc/login")"
check "it shows nothing without signing in" test "$(web -o /dev/null -w '%{http_code}' "$chat/api/models")" = 401
for u in alice bob; do
  chat_login $u
  check "$u signs in to Open WebUI through Keycloak" grep -q "\"name\":\"$u\"\|\"email\":\"$u@" <<<"$(chat_api $u "$chat/api/v1/auths/")"
done
check "alice is shown as alice"            grep -q '"name":"alice"' <<<"$(chat_api alice "$chat/api/v1/auths/")"
check "alice is an Open WebUI admin"       grep -q '"role":"admin"' <<<"$(chat_api alice "$chat/api/v1/auths/")"
check "bob is a user"                      grep -q '"role":"user"'  <<<"$(chat_api bob "$chat/api/v1/auths/")"
has_agent() { chat_api bob "$chat/api/models" | grep -q "\"id\":\"$1\""; }
check "bob has the operator agent of the unit garrison" until_true 180 has_agent garrison/operator
check "and the commander, which finds the agent for a task" has_agent garrison/commander
check "the agents come from the agent service" grep -q '"owned_by":"garrison"' <<<"$(chat_api bob "$chat/api/models")"
check "Open WebUI has no LLM key" \
  test -z "$(kubectl --namespace garrison exec -i deployment/openwebui -- printenv OPENAI_API_KEY)"
check "the agent service wants a token" \
  test "$(agents_api -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"model":"garrison/operator","messages":[]}' /v1/chat/completions)" = 401
check "it refuses a made-up token" \
  test "$(agents_api -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln' -d '{"model":"garrison/operator","messages":[]}' /v1/chat/completions)" = 401
check "the agent service is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' "https://agents.$DOMAIN/v1/models")" = 404
check "garrison has its config repo with the operator" \
  test "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/garrison/config/raw/agents/operator/agent.yaml")" = 200
check "bob sees the channel of the unit garrison" until_true 120 system_channel
cid=$(system_channel)
check "bob cannot rename it" \
  test "$(chat_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bobs-channel"}' "$chat/api/v1/channels/$cid/update")" = 403
check "bob cannot delete it"               test "$(chat_api bob -o /dev/null -w '%{http_code}' -X DELETE "$chat/api/v1/channels/$cid/delete")" = 403
check "it is still there, unchanged"       test "$(system_channel)" = "$cid"
howto_pinned() { chat_api bob "$chat/api/v1/channels/$cid/messages/pinned" | grep -q 'How to ask the agents'; }
check "it has the how-to pinned"           until_true 120 howto_pinned
check "the gateway's account says it is not the agent" \
  grep -q '"name":"Gateway (not the agent)"' <<<"$(chat_api bob "$chat/api/v1/channels/$cid/members?page=1")"
check "the gateway is not reachable from outside" test "$(web -o /dev/null -w '%{http_code}' "https://gateway.$DOMAIN/mcp")" = 404
check "its MCP endpoint wants a token"     test "$(mcp -o /dev/null -w '%{http_code}' -X POST -d '{}')" = 401
check "it refuses a made-up token"         test "$(mcp -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln' -d '{}')" = 401
check "it refuses identity headers"        test "$(mcp -o /dev/null -w '%{http_code}' -X POST -H 'X-OpenWebUI-User-Name: alice' -H 'X-Forwarded-User: alice' -d '{}')" = 401

echo "# roles in Grafana"
check "Grafana is healthy over valid TLS"  status_is 200 grafana /api/health 300
check "Grafana sends visitors to the Keycloak login" \
  grep -q "^https://keycloak.$DOMAIN/realms/garrison/protocol/openid-connect/auth" <<<"$(web -o /dev/null -w '%{redirect_url}' $g/login/generic_oauth)"
check "Grafana shows nothing without signing in" test "$(web -o /dev/null -w '%{http_code}' $g/api/search)" = 401
for u in alice bob; do
  sso_login "$tmp/grafana-$u.jar" "$g/login/generic_oauth" "$TEST_PASSWORD" "$tmp/grafana-$u.html" $u >/dev/null 2>&1
  check "$u signs in to Grafana through Keycloak" grep -q "\"login\":\"$u\"" <<<"$(grafana_api $u $g/api/user)"
done
check "alice is a Grafana admin"           grep -q '"isGrafanaAdmin":true'  <<<"$(grafana_api alice $g/api/user)"
check "bob is not a Grafana admin"         grep -q '"isGrafanaAdmin":false' <<<"$(grafana_api bob $g/api/user)"
check "bob is a viewer"                    grep -q '"role":"Viewer"' <<<"$(grafana_api bob $g/api/user/orgs)"
check "bob can look at dashboards"         test "$(grafana_api bob -o /dev/null -w '%{http_code}' $g/api/search)" = 200
check "bob can query metrics" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"queries":[{"refId":"A","datasource":{"uid":"prometheus"},"expr":"up","instant":true}],"from":"now-5m","to":"now"}' $g/api/ds/query)" = 200
check "bob cannot create a dashboard" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"dashboard":{"title":"bob"},"overwrite":false}' $g/api/dashboards/db)" = 403
check "bob cannot add a data source" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"name":"bob","type":"prometheus","url":"http://example.invalid","access":"proxy"}' $g/api/datasources)" = 403
check "bob cannot make himself admin" \
  test "$(grafana_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"role":"Admin"}' $g/api/org/users/2)" = 403
check "alice can create a dashboard" \
  test "$(grafana_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"dashboard":{"uid":"smoke","title":"Smoke test"},"overwrite":true}' $g/api/dashboards/db)" = 200

}
