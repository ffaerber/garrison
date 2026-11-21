# Contracts

Public contracts other repos may rely on. The deployer renders stacks against these names (`control-plane/internal/render`); the system stack must provide them.

| Name | Provided by | Meaning |
| --- | --- | --- |
| `app-<stack>` | Deployer | The namespace of one environment of an app: `todo`, `dev-todo` and `pr-7-todo` each have their own. Its services reach each other by their names, as on a network of their own, and nothing else reaches them but Traefik. |
| `traefik` | Traefik stack | The IngressClass of the routes; the default one. |
| `websecure` | Traefik stack | HTTPS entry point serving the wildcard certificate. |
| `sso@file` | Traefik stack, login proxy | Login check backed by Keycloak. Applied to `private` environments. The app receives `X-Auth-Request-User`, `X-Auth-Request-Email` and `X-Auth-Request-Preferred-Username`; Traefik overwrites any such header a client sends. |
| `*.<domain>` | Traefik stack, installer (CoreDNS) | Inside the cluster every name of the installation resolves to Traefik's Service, so pods reach Keycloak and the apps at their public URLs. |
| `registry-pull` | Gitea stack, deployer | The Secret pods pull the installation's images with; the deployer copies it into each app's namespace. |
| `X-Garrison-Ingress`, `GARRISON_INGRESS_KEY` | Deployer | The ingress key of a `private` environment. Traefik adds the key to each request it passes on after the login check, in the header `X-Garrison-Ingress`, replacing whatever a client sent; the routed services have it in the variable `GARRISON_INGRESS_KEY`. An app that acts on who is signed in compares the two (in constant time) before it believes the `X-Auth-Request-*` headers. Each environment's NetworkPolicy already keeps other apps from calling it past Traefik; the key keeps that true should they ever reach it. Each environment has its own key, the same on every deploy. |
| `nvidia.com/gpu` | The NVIDIA device plugin, where installed | The resource a service with `devices: [gpu]` asks for, one GPU. |

What the deployer makes of a routed service `<service>` of an environment, in its namespace `app-<stack>` (see `control-plane/internal/render/testdata` for whole examples):

```
Deployment <service>       its pod, labelled garrison.io/service=<service>
Service <service>          headless; port "http" when the service has a port
Ingress <service>          host <host>.<domain>, entry point websecure
  traefik.ingress.kubernetes.io/router.middlewares: sso@file,app-<stack>-<service>-ingress@kubernetescrd   # private environments only
Middleware <service>-ingress   customRequestHeaders X-Garrison-Ingress: <key>                             # private environments only
```

Every object carries the labels `garrison.unit`, `garrison.app`, `garrison.environment` and `garrison.io/stack`.
