# Controller

Go. Watches Gitea, renders stacks to Swarm, holds the SOPS key, takes snapshots, enforces approvals and hostname claims, writes change records.

## What exists

Runs in Swarm as the stack `controller`, from an image Gitea Actions builds from the repo `system/controller` (`Dockerfile`, `ci/build.yaml`). It follows the `system` organisation and deploys every repo there that holds a `compose.yaml`. The same binary is installed on the host as `homecloud-controller`: the installer uses it to deploy once before the service exists, and admins use it for `applied`, `validate`, `render` and `apply`.

| Package | Does |
| --- | --- |
| `internal/stack` | Parses `stack.yaml`, validates it against `schemas/stack.schema.json` and the rules a schema cannot express, fills in hostnames |
| `internal/hostnames` | Lists the hostnames a stack claims and rejects clashes and reserved names |
| `internal/render` | Renders one environment of a stack as a Compose file for `docker stack deploy` |
| `internal/apply` | Creates the ZFS datasets and Swarm secrets a stack needs, then runs `docker stack deploy` |
| `internal/system` | Deploys a system stack: a plain Compose file, its datasets and config hash |
| `internal/gitops` | Follows an organisation in Gitea and deploys every repo with a `compose.yaml`; the controller itself last |
| `internal/hooks` | Gitea's webhooks: puts one on every organisation, its secret derived from the admin password, checks the signature of each call, and wakes the sync loops |
| `cmd/controller` | CLI over these; `serve` runs the controller |

```
make test
make build
bin/controller validate controller/internal/stack/testdata/jellyfin.yaml
bin/controller claims -reserved system/platform/reserved-hostnames.yaml STACK.yaml...
bin/controller render -domain example.com -env dev STACK.yaml
bin/controller apply -domain example.com -reserved /etc/homecloud/reserved-hostnames.yaml STACK.yaml   # on an installed machine, as root
```

Go runs in a container, so only Docker is needed. `make golden` rewrites the expected render output after an intended change.

## Naming

| Thing | Name |
| --- | --- |
| Swarm stack | the environment's hostname, e.g. `dev-jellyfin` (unique across the installation) |
| Dataset on the host (data root `/homecloud/ns`) | `<data root>/<namespace>/stacks/<environment hostname>/<dataset>` |
| Share on the host | `<data root>/<namespace>/shares/<share>` |
| Swarm secret | `<environment hostname>.<secret>` |

What rendered stacks expect from the system stack is in `system/platform/contracts`.

## Not built yet

Snapshots, SOPS, migrations, change records, approvals, restore, and namespaces other than `system`. See `docs/status.md`.

## Known gaps

- GPU rendering is untested on real hardware; see the spike in `docs/spec.md`, "Testing".
- Namespace networks are not rendered. Swarm gives every service its short name (`app`, `redis`) as a DNS alias on each network it joins, so two stacks sharing a network can resolve each other's services by accident. Routed services already share `traefik-public`. This needs a decision before stacks in a namespace are put on one network.
