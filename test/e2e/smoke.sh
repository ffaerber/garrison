#!/usr/bin/env bash
# Smoke test for an installed machine. Run on the machine, as root, from the repo root.
set -uo pipefail

. /etc/homecloud/install.env
ETC=/etc/homecloud
STACKS=test/e2e/stacks
CTL="homecloud-controller"
failed=0

ok()   { printf 'ok    %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n' "$1"; [ $# -gt 1 ] && printf '      %s\n' "$2"; failed=$((failed + 1)); }
check() { local name=$1; shift; if out=$("$@" 2>&1); then ok "$name"; else bad "$name" "$(printf '%s' "$out" | tail -n 3)"; fi; }

# curl a hostname of the installation, verifying the installed certificate.
req() { local host=$1; shift; curl -sS --max-time 10 --cacert "$ETC/tls.crt" --resolve "$host.$DOMAIN:443:127.0.0.1" "$@"; }

# Poll until a URL returns the wanted status code.
status_is() {
  local want=$1 host=$2 path=$3 got= deadline=$((SECONDS + ${4:-180}))
  while [ $SECONDS -lt $deadline ]; do
    got=$(req "$host" -o /dev/null -w '%{http_code}' "https://$host.$DOMAIN$path" 2>/dev/null)
    [ "$got" = "$want" ] && return 0
    sleep 3
  done
  echo "https://$host.$DOMAIN$path returned $got, want $want"; return 1
}

# curl with every hostname of the installation pointed at this machine.
web() {
  local args=() h
  for h in auth sso git hello dev-hello; do args+=(--resolve "$h.$DOMAIN:443:127.0.0.1"); done
  curl -sS --max-time 20 --cacert "$ETC/tls.crt" "${args[@]}" "$@"
}

# sso_login JAR URL PASSWORD OUT [USER]: open URL, fill in the Keycloak login form
# it leads to, follow the redirects back. Prints the final status; body goes to OUT.
sso_login() {
  local jar=$1 url=$2 password=$3 out=$4 user=${5:-$ADMIN_USER} form action
  form=$(web -L -c "$jar" -b "$jar" "$url") || return 1
  action=$(grep -o 'action="[^"]*"' <<<"$form" | head -n1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
  [ -n "$action" ] || { echo "no login form behind $url"; return 1; }
  web -L -c "$jar" -b "$jar" -o "$out" -w '%{http_code}' \
    --data-urlencode "username=$user" --data-urlencode "password=$password" "$action"
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
PASSWORD=$(cat $ETC/admin-password)

echo "# system stack"
check "ZFS pool is online"                 test "$(zpool list -H -o health homecloud)" = ONLINE
check "Keycloak serves OIDC discovery over valid TLS" status_is 200 auth /realms/master/.well-known/openid-configuration 300
check "Keycloak issuer uses the public hostname" \
  bash -c "$(declare -f req); ETC=$ETC DOMAIN=$DOMAIN; req auth https://auth.$DOMAIN/realms/master/.well-known/openid-configuration | grep -q '\"issuer\":\"https://auth.$DOMAIN/realms/master\"'"
check "Gitea is healthy over valid TLS"    status_is 200 git /api/healthz
login=$(req git -u "$ADMIN_USER:$(cat $ETC/admin-password)" "https://git.$DOMAIN/api/v1/user" 2>&1)
check "Gitea admin can log in and is admin" grep -q '"is_admin":true' <<<"$login"
check "Gitea rejects a wrong password"     test "$(req git -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:wrong" "https://git.$DOMAIN/api/v1/user")" = 401
redirect=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "git.$DOMAIN:80:127.0.0.1" "http://git.$DOMAIN/")
check "HTTP redirects to HTTPS"            grep -q "^30[18] https://git.$DOMAIN/" <<<"$redirect"
check "Postgres is not published on the host" bash -c '! (ss -ltn | grep -q ":5432 ")'

echo "# deploying a stack"
check "controller deploys hello (main)"    $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "hello is reachable at its hostname" status_is 200 hello /
check "hello got its dataset with a quota" test "$(zfs get -H -o value quota homecloud/ns/demo/stacks/hello/hello-data)" = 1G
check "hello's dataset is mounted in the container" \
  bash -c 'c=$(docker ps -q --filter label=com.docker.swarm.service.name=hello_web | head -n1); docker inspect "$c" --format "{{range .Mounts}}{{.Source}}:{{.Destination}} {{end}}" | grep -q "/homecloud/ns/demo/stacks/hello/hello-data:/data"'
check "hello got a generated secret"       docker secret inspect hello.hello-token
check "controller deploys hello (dev)"     $CTL apply -domain "$DOMAIN" -env dev -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml
check "dev has its own dataset"            zfs list homecloud/ns/demo/stacks/dev-hello/hello-data
check "redeploying hello is accepted"      $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml

echo "# single sign-on"
check "the homecloud realm serves OIDC discovery" status_is 200 auth /realms/homecloud/.well-known/openid-configuration
check "a private environment answers visitors with a redirect" status_is 302 dev-hello /
target=$(web -o /dev/null -w '%{redirect_url}' "https://dev-hello.$DOMAIN/")
check "the redirect goes to the Keycloak login" grep -q "^https://auth.$DOMAIN/realms/homecloud/protocol/openid-connect/auth" <<<"$target"
check "a forged identity header does not get in" \
  test "$(web -o /dev/null -w '%{http_code}' -H "X-Auth-Request-Email: $ADMIN_EMAIL" "https://dev-hello.$DOMAIN/")" = 302
sso_login "$tmp/bad.jar" "https://dev-hello.$DOMAIN/" "wrong-password" "$tmp/bad.html" >/dev/null 2>&1
check "a wrong password does not reach the private environment" bash -c "! grep -q '^Hostname:' $tmp/bad.html"
code=$(sso_login "$tmp/sso.jar" "https://dev-hello.$DOMAIN/" "$PASSWORD" "$tmp/app.html" 2>&1)
check "the admin signs in and reaches the private environment" bash -c "[ '$code' = 200 ] && grep -q '^Hostname:' $tmp/app.html"
check "the app is told who signed in"      grep -qi "^X-Auth-Request-Email: $ADMIN_EMAIL" "$tmp/app.html"
check "the session works for later requests" test "$(web -b "$tmp/sso.jar" -o /dev/null -w '%{http_code}' "https://dev-hello.$DOMAIN/again")" = 200
check "the public environment needs no login" status_is 200 hello / 30
check "Gitea settings need a login"        bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --cacert $ETC/tls.crt --resolve git.$DOMAIN:443:127.0.0.1 https://git.$DOMAIN/user/settings)\" != 200 ]"
sso_login "$tmp/git.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$PASSWORD" "$tmp/git.html" >/dev/null 2>&1
check "the admin signs in to Gitea through Keycloak" test "$(web -b "$tmp/git.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/user/settings")" = 200

echo "# roles"
# Two test people. Their passwords are kept so they can also log in by hand.
ADD_USER=platform/system/03-keycloak/add-user.sh
mkdir -p -m 700 $ETC/test-users
for u in alice bob; do [ -s $ETC/test-users/$u ] || openssl rand -hex 12 > $ETC/test-users/$u; done
# gitea_api USER_TO_ACT_AS ARGS...: call the Gitea API as the local admin, or,
# with a name, with exactly that person's rights (Gitea's Sudo header).
gitea_api() {
  local as=$1; shift
  web -u "$ADMIN_USER:$PASSWORD" ${as:+-H "Sudo: $as"} "$@"
}
repo=https://git.$DOMAIN/api/v1/repos/system/platform

check "alice is added as root admin"       env USER_PASSWORD="$(cat $ETC/test-users/alice)" $ADD_USER alice "alice@$DOMAIN" --admin
check "bob is added as user"               env USER_PASSWORD="$(cat $ETC/test-users/bob)" $ADD_USER bob "bob@$DOMAIN"
for u in alice bob; do
  sso_login "$tmp/$u.jar" "https://git.$DOMAIN/user/oauth2/keycloak" "$(cat $ETC/test-users/$u)" "$tmp/$u.html" $u >/dev/null 2>&1
  check "$u signs in to Gitea through Keycloak" test "$(web -b "$tmp/$u.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/user/settings")" = 200
done
check "alice is a Gitea admin"             grep -q '"is_admin":true'  <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/alice")"
check "bob is not a Gitea admin"           grep -q '"is_admin":false' <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/bob")"
check "the platform repo is in Gitea and private" grep -q '"private":true' <<<"$(gitea_api "" $repo)"
check "the platform repo holds the system stack"  test "$(gitea_api "" -o /dev/null -w '%{http_code}' $repo/contents/system/02-traefik/compose.yaml)" = 200
check "alice can administer the platform repo"    grep -q '"permissions":{"admin":true' <<<"$(gitea_api alice $repo)"
check "alice can change the platform repo" \
  test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"YWxpY2UK","message":"smoke test","new_branch":"smoke-'$RANDOM$RANDOM'"}' $repo/contents/smoke-test.txt)" = 201
check "bob cannot see the platform repo"   test "$(gitea_api bob -o /dev/null -w '%{http_code}' $repo)" = 404
check "bob cannot see it in the browser either" test "$(web -b "$tmp/bob.jar" -o /dev/null -w '%{http_code}' "https://git.$DOMAIN/system/platform")" = 404
check "bob cannot change the platform repo" \
  test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"content":"Ym9iCg==","message":"smoke test"}' $repo/contents/bob-was-here.txt)" = 404
check "bob's change did not land"          test "$(gitea_api "" -o /dev/null -w '%{http_code}' $repo/contents/bob-was-here.txt)" = 404
check "bob cannot make himself admin"      test "$(gitea_api bob -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' -d '{"login_name":"bob","source_id":0,"admin":true}' "https://git.$DOMAIN/api/v1/admin/users/bob")" = 403
check "bob is still not a Gitea admin"     grep -q '"is_admin":false' <<<"$(gitea_api "" "https://git.$DOMAIN/api/v1/users/bob")"

echo "# hostname claims"
check "a stack claiming a held hostname is rejected"     bash -c "! $CTL apply -domain $DOMAIN -reserved $ETC/reserved-hostnames.yaml $STACKS/hello-clash.yaml"
check "a stack claiming a reserved hostname is rejected" bash -c "! $CTL apply -domain $DOMAIN -reserved $ETC/reserved-hostnames.yaml $STACKS/git-reserved.yaml"
check "rejected stacks were not deployed"  bash -c '! (docker stack ls --format "{{.Name}}" | grep -qx git)'
check "hello still answers after the rejected deploys"   status_is 200 hello / 30

echo
if [ $failed -eq 0 ]; then echo "all checks passed"; else echo "$failed check(s) failed"; fi
exit $((failed > 0))
