# Controller

Go. Watches Gitea, renders stacks to Swarm, holds the SOPS key, takes snapshots, enforces approvals and hostname claims, writes change records.

## What exists

The offline core, with no cluster or Gitea access yet:

| Package | Does |
| --- | --- |
| `internal/stack` | Parses `stack.yaml`, validates it against `schemas/stack.schema.json` and the rules a schema cannot express, fills in hostnames |
| `internal/hostnames` | Lists the hostnames a stack claims and rejects clashes and reserved names |
| `internal/render` | Renders one environment of a stack as a Compose file for `docker stack deploy` |
| `cmd/controller` | CLI over the three |

```
make test
make build
bin/controller validate controller/internal/stack/testdata/jellyfin.yaml
bin/controller claims -reserved platform/reserved-hostnames.yaml STACK.yaml...
bin/controller render -domain example.com -env dev STACK.yaml
```

Go runs in a container, so only Docker is needed. `make golden` rewrites the expected render output after an intended change.

## Naming

| Thing | Name |
| --- | --- |
| Swarm stack | the environment's hostname, e.g. `dev-jellyfin` (unique across the installation) |
| Dataset on the host | `<data root>/<namespace>/stacks/<environment hostname>/<dataset>` |
| Share on the host | `<data root>/<namespace>/shares/<share>` |
| Swarm secret | `<environment hostname>.<secret>` |

What rendered stacks expect from the system stack is in `platform/contracts`.

## Not built yet

Watching Gitea, deploying, snapshots, SOPS, migrations, change records, approvals, restore.

## Known gaps

- GPU rendering is untested on real hardware; see the spike in `docs/spec.md`, "Testing".
- Namespace networks are not rendered. Swarm gives every service its short name (`app`, `redis`) as a DNS alias on each network it joins, so two stacks sharing a network can resolve each other's services by accident. Routed services already share `traefik-public`. This needs a decision before stacks in a namespace are put on one network.
