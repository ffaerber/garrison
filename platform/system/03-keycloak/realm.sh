# Runs inside the Keycloak container, after configure.sh has logged kcadm in.
set -euo pipefail
realm=homecloud
kc() { /opt/keycloak/bin/kcadm.sh "$@" --config /tmp/kcadm.config; }

kc get "realms/$realm" >/dev/null 2>&1 ||
  kc create realms -s "realm=$realm" -s enabled=true -s "displayName=Home Cloud" >/dev/null

# client ID SECRET_NAME REDIRECT_URI
client() {
  local id args
  args=(-s "clientId=$1" -s enabled=true -s publicClient=false -s standardFlowEnabled=true
        -s directAccessGrantsEnabled=false -s "secret=$(<"/run/secrets/$2")" -s "redirectUris=[\"$3\"]")
  id=$(kc get clients -r "$realm" -q "clientId=$1" --fields id --format csv --noquotes)
  if [ -z "$id" ]; then
    kc create clients -r "$realm" "${args[@]}" >/dev/null
  else
    kc update "clients/$id" -r "$realm" "${args[@]}"
  fi
}
client gitea gitea-oidc-secret "https://git.$DOMAIN/user/oauth2/keycloak/callback"
client sso   sso-oidc-secret   "https://sso.$DOMAIN/oauth2/callback"

# The first admin. The password is set once; a later change by the person is kept.
user=$(kc get users -r "$realm" -q "username=$ADMIN_USER" -q exact=true --fields id --format csv --noquotes)
if [ -z "$user" ]; then
  kc create users -r "$realm" -s "username=$ADMIN_USER" -s "email=$ADMIN_EMAIL" -s emailVerified=true \
    -s enabled=true -s "firstName=$ADMIN_USER" -s lastName=Admin >/dev/null
  kc set-password -r "$realm" --username "$ADMIN_USER" --new-password "$(</run/secrets/admin-password)"
fi
