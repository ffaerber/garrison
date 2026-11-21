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

echo "# who reaches whom (garrison/platform's NetworkPolicies)"
# reaches FROM HOST PORT: whether a connection from FROM opens. FROM is
# "pod" (a pod of another namespace, as an app's would be), "job" (a
# Docker container on the pipelines' network) or "garrison" (a pod of the
# platform without a role, like the agents' browser).
reaches() {
  local from=$1 host=$2 port=$3
  case $from in
    job) docker run --rm --network garrison-builds --dns 10.43.0.10 busybox:1.37 nc -z -w 3 "$host" "$port" >/dev/null 2>&1 ;;
    pod|garrison)
      local ns=garrison-check; [ "$from" = garrison ] && ns=garrison
      kubectl get namespace "$ns" >/dev/null 2>&1 || kubectl create namespace "$ns" >/dev/null
      kubectl --namespace "$ns" run "probe-$RANDOM" --rm -i --quiet --restart=Never --image=busybox:1.37 \
        --overrides='{"spec":{"automountServiceAccountToken":false}}' -- nc -z -w 3 "$host" "$port" >/dev/null 2>&1 ;;
  esac
}
pg=postgres.garrison.svc.cluster.local gitea=gitea.garrison.svc.cluster.local
check "a pod of another namespace cannot reach Postgres"   bash -c "$(declare -f reaches); ! reaches pod $pg 5432"
check "nor Gitea's port"                                   bash -c "$(declare -f reaches); ! reaches pod $gitea 3000"
check "a pipeline job reaches Gitea"                       bash -c "$(declare -f reaches); reaches job $gitea 3000"
check "but not Postgres"                                   bash -c "$(declare -f reaches); ! reaches job $pg 5432"
check "nor the user runner's Docker guard"                 bash -c "$(declare -f reaches); ! reaches job guard.garrison.svc.cluster.local 2375"
check "a pod of the platform without a role cannot reach Postgres" bash -c "$(declare -f reaches); ! reaches garrison $pg 5432"
check "everyone reaches the installation's names, through Traefik" bash -c "$(declare -f reaches); reaches pod git.$DOMAIN 443"
kubectl delete namespace garrison-check --wait=false >/dev/null 2>&1 || true

}
