# shellcheck shell=bash
# ---------- sections ----------
# system: the stacks the installer deploys are up and safe.
section_system() {
echo "# system stack"
check "ZFS pool is online"                 test "$(zpool list -H -o health garrison)" = ONLINE
check "Keycloak serves OIDC discovery over valid TLS" status_is 200 keycloak /realms/master/.well-known/openid-configuration 300
check "Keycloak issuer uses the public hostname" \
  bash -c "$(declare -f req); ETC=$ETC DOMAIN=$DOMAIN; req keycloak https://keycloak.$DOMAIN/realms/master/.well-known/openid-configuration | grep -q '\"issuer\":\"https://keycloak.$DOMAIN/realms/master\"'"
check "Gitea is healthy over valid TLS"    status_is 200 git /api/healthz
login=$(req git -u "$ADMIN_USER:$(cat $ETC/admin-password)" "https://git.$DOMAIN/api/v1/user" 2>&1)
check "Gitea admin can log in and is admin" grep -q '"is_admin":true' <<<"$login"
check "Gitea rejects a wrong password"     test "$(req git -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:wrong" "https://git.$DOMAIN/api/v1/user")" = 401
redirect=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "git.$DOMAIN:80:127.0.0.1" "http://git.$DOMAIN/")
check "HTTP redirects to HTTPS"            grep -q "^30[18] https://git.$DOMAIN/" <<<"$redirect"
check "Postgres is not published on the host" bash -c '! (ss -ltn | grep -q ":5432 ")'

}
