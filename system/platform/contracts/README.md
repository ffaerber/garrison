# Contracts

Public contracts other repos may rely on. The deployer renders stacks against these names (`control-plane/internal/render`); the system stack must provide them.

| Name | Provided by | Meaning |
| --- | --- | --- |
| `traefik-public` | Traefik stack | Overlay network Traefik routes over. Every routed service joins it. |
| `websecure` | Traefik stack | HTTPS entry point serving the wildcard certificate. |
| `sso@file` | Traefik stack, login proxy | Login check backed by Keycloak. Applied to `private` environments. The app receives `X-Auth-Request-User`, `X-Auth-Request-Email` and `X-Auth-Request-Preferred-Username`; Traefik overwrites any such header a client sends. |
| `keycloak.<domain>` | Traefik stack | Resolves to Traefik on `traefik-public` and on the system network, so containers reach Keycloak at its public URL. |
| `X-Homecloud-Ingress`, `HOMECLOUD_INGRESS_KEY` | Deployer | The ingress key of a `private` environment. Every routed service is on `traefik-public`, so another app can call a service directly, past Traefik, and put any name into the `X-Auth-Request-*` headers. Traefik adds the key to each request it passes on after the login check, in the header `X-Homecloud-Ingress`, replacing whatever a client sent; the routed services have it in the variable `HOMECLOUD_INGRESS_KEY`. An app that acts on who is signed in compares the two (in constant time) before it believes the `X-Auth-Request-*` headers. Each environment has its own key, the same on every deploy. |
| `GPU` | Installer | Swarm generic resource advertised once per GPU on the node. |

Labels the deployer sets on a routed service, with `<host>` the service's hostname:

```
traefik.enable=true
traefik.swarm.network=traefik-public
traefik.http.routers.<host>.rule=Host(`<host>.<domain>`)
traefik.http.routers.<host>.entrypoints=websecure
traefik.http.routers.<host>.tls=true
traefik.http.routers.<host>.middlewares=sso@file,<host>-ingress@swarm      # private environments only
traefik.http.middlewares.<host>-ingress.headers.customrequestheaders.X-Homecloud-Ingress=<key>   # private environments only
traefik.http.services.<host>.loadbalancer.server.port=<port>
```
