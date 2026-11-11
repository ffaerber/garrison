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
check "private dev environment is closed until SSO is wired" status_is 403 dev-hello /
check "dev has its own dataset"            zfs list homecloud/ns/demo/stacks/dev-hello/hello-data
check "redeploying hello is accepted"      $CTL apply -domain "$DOMAIN" -reserved $ETC/reserved-hostnames.yaml $STACKS/hello.yaml

echo "# hostname claims"
check "a stack claiming a held hostname is rejected"     bash -c "! $CTL apply -domain $DOMAIN -reserved $ETC/reserved-hostnames.yaml $STACKS/hello-clash.yaml"
check "a stack claiming a reserved hostname is rejected" bash -c "! $CTL apply -domain $DOMAIN -reserved $ETC/reserved-hostnames.yaml $STACKS/git-reserved.yaml"
check "rejected stacks were not deployed"  bash -c '! (docker stack ls --format "{{.Name}}" | grep -qx git)'
check "hello still answers after the rejected deploys"   status_is 200 hello / 30

echo
if [ $failed -eq 0 ]; then echo "all checks passed"; else echo "$failed check(s) failed"; fi
exit $((failed > 0))
