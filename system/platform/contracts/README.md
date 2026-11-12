# Contracts

Public contracts other repos may rely on. The deployer renders stacks against these names (`control-plane/internal/render`); the system stack must provide them.

| Name | Provided by | Meaning |
| --- | --- | --- |
| `traefik-public` | Traefik stack | Overlay network Traefik routes over. Every routed service joins it. |
| `websecure` | Traefik stack | HTTPS entry point serving the wildcard certificate. |
| `sso@file` | Traefik stack, login proxy | Login check backed by Keycloak. Applied to `private` environments. The app receives `X-Auth-Request-User`, `X-Auth-Request-Email` and `X-Auth-Request-Preferred-Username`; Traefik overwrites any such header a client sends. |
| `keycloak.<domain>` | Traefik stack | Resolves to Traefik on `traefik-public`, so containers reach Keycloak at its public URL. |
| `GPU` | Installer | Swarm generic resource advertised once per GPU on the node. |

Labels the deployer sets on a routed service, with `<host>` the service's hostname:

```
traefik.enable=true
traefik.swarm.network=traefik-public
traefik.http.routers.<host>.rule=Host(`<host>.<domain>`)
traefik.http.routers.<host>.entrypoints=websecure
traefik.http.routers.<host>.tls=true
traefik.http.routers.<host>.middlewares=sso@file      # private environments only
traefik.http.services.<host>.loadbalancer.server.port=<port>
```
