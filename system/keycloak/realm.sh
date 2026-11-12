# Runs inside the Keycloak container, after kcadm has been logged in.
set -euo pipefail
realm=homecloud
kc() { /opt/keycloak/bin/kcadm.sh "$@" --config /tmp/kcadm.config; }
id_of() { kc get "$1" -r "$realm" "${@:2}" --fields id --format csv --noquotes; }

kc get "realms/$realm" >/dev/null 2>&1 ||
  kc create realms -s "realm=$realm" -s enabled=true -s "displayName=Home Cloud" >/dev/null

# Root admins: the people who may change the platform itself.
[ -n "$(id_of groups -q search=root-admins -q exact=true)" ] ||
  kc create groups -r "$realm" -s name=root-admins >/dev/null
group=$(id_of groups -q search=root-admins -q exact=true)

# client ID SECRET_NAME REDIRECT_URI
# Each client's tokens carry the person's groups in a `groups` claim. After
# signing out, a client may send people back to any of its redirect URIs.
client() {
  local id args
  args=(-s "clientId=$1" -s enabled=true -s publicClient=false -s standardFlowEnabled=true
        -s directAccessGrantsEnabled=false -s "secret=$(<"/run/secrets/$2")" -s "redirectUris=[\"$3\"]"
        -s 'attributes."post.logout.redirect.uris"=+')
  id=$(id_of clients -q "clientId=$1")
  if [ -z "$id" ]; then
    kc create clients -r "$realm" "${args[@]}" >/dev/null
    id=$(id_of clients -q "clientId=$1")
  else
    kc update "clients/$id" -r "$realm" "${args[@]}"
  fi
  if ! kc get "clients/$id/protocol-mappers/models" -r "$realm" --fields name --format csv --noquotes | grep -qx groups; then
    kc create "clients/$id/protocol-mappers/models" -r "$realm" -s name=groups -s protocol=openid-connect \
      -s protocolMapper=oidc-group-membership-mapper -s 'config."claim.name"=groups' -s 'config."full.path"=false' \
      -s 'config."id.token.claim"=true' -s 'config."access.token.claim"=true' -s 'config."userinfo.token.claim"=true' >/dev/null
  fi
}
client gitea gitea-oidc-secret "https://git.$DOMAIN/user/oauth2/keycloak/callback"
client sso   sso-oidc-secret   "https://sso.$DOMAIN/oauth2/callback"
client grafana grafana-oidc-secret "https://grafana.$DOMAIN/login/generic_oauth"
client openwebui openwebui-oidc-secret "https://chat.$DOMAIN/oauth/oidc/callback"

# audience CLIENT AUDIENCE: CLIENT's access tokens also name AUDIENCE.
audience() {
  local id
  id=$(id_of clients -q "clientId=$1")
  kc get "clients/$id/protocol-mappers/models" -r "$realm" --fields name --format csv --noquotes | grep -qx "audience-$2" ||
    kc create "clients/$id/protocol-mappers/models" -r "$realm" -s "name=audience-$2" -s protocol=openid-connect \
      -s protocolMapper=oidc-audience-mapper -s "config.\"included.client.audience\"=$2" \
      -s 'config."access.token.claim"=true' -s 'config."id.token.claim"=false' >/dev/null
}
# Open WebUI's tokens count at the login proxy too: an agent's browser, acting
# for the person who asked, shows the apps behind the sign-in as them.
audience openwebui sso

# The first admin. The password is set once; a later change by the person is kept.
user=$(id_of users -q "username=$ADMIN_USER" -q exact=true)
if [ -z "$user" ]; then
  kc create users -r "$realm" -s "username=$ADMIN_USER" -s "email=$ADMIN_EMAIL" -s emailVerified=true \
    -s enabled=true -s "firstName=$ADMIN_USER" -s lastName=Admin >/dev/null
  kc set-password -r "$realm" --username "$ADMIN_USER" --new-password "$(</run/secrets/admin-password)"
  user=$(id_of users -q "username=$ADMIN_USER" -q exact=true)
fi
kc update "users/$user/groups/$group" -r "$realm" -s "realm=$realm" -s "userId=$user" -s "groupId=$group" -n
