# Control plane

Go. One image, run in Swarm as the stack `control-plane` from the repo `system/control-plane` (`Dockerfile`, `ci/build.yaml`), as five services:

| Service | Does |
| --- | --- |
| `deployer` | Makes the machine match Git: deploys every repo of `system` that holds a `compose.yaml` (this one last) and the apps of every other namespace, takes down what was deleted, keeps dashboards in Grafana. Later also holds the SOPS key, takes snapshots and writes change records. |
| `gateway` | The way in for people's agents: the MCP server Open WebUI calls with each person's Keycloak token, which reads and proposes pull requests as that person. Keeps each namespace's agents (from `agents/` in its config repo; each gets only its own tools) and channel, its members and approval checks, and its build account. Changes nothing that runs. |
| `agents` | The agent service: the OpenAI-compatible API Open WebUI talks to, whose models are the namespaces' agents. Runs each turn of an agent: the LLM, whose key only it holds, with the agent's prompt and its tools from the gateway, acting as the person chatting. |
| `guard` | The Docker API for the user runner, letting through only what pipeline jobs need. |
| `user-runner` | Gitea Actions runner of every namespace other than `system`. |

The same binary is installed on the host as `homecloud`: the installer uses it to deploy once before the services exist, and admins use it for `applied`, `validate`, `render` and `apply`.

| Package | Does |
| --- | --- |
| `internal/stack` | Parses `stack.yaml`, validates it against `schemas/stack.schema.json` and the rules a schema cannot express, fills in hostnames |
| `internal/hostnames` | Lists the hostnames a stack claims and rejects clashes and reserved names |
| `internal/render` | Renders one environment of a stack as a Compose file for `docker stack deploy` |
| `internal/apply` | Creates the ZFS datasets and Swarm secrets a stack needs, then runs `docker stack deploy` |
| `internal/system` | Deploys a system stack: a plain Compose file, its datasets and config hash |
| `internal/gitops` | Follows an organisation in Gitea and deploys every repo with a `compose.yaml`; the deployer itself last |
| `internal/hooks` | Gitea's webhooks: puts one on every organisation, its secret derived from the admin password, checks the signature of each call, and wakes the sync loops |
| `internal/dashboards` | Keeps `dashboards/*.json` of every repo in Grafana |
| `internal/agentapi` | The agent service: models, chat completions (streamed), the agent loop, Open WebUI's tasks |
| `internal/agents` | Agent folders: reads and checks `agents/` and `skills/` of a config repo; the operator; the system prompt |
| `internal/gateway` | The gateway: MCP tools, Open WebUI agents and channels, members and approvals, build accounts |
| `internal/guard` | The guarded Docker API |
| `cmd/homecloud` | CLI over these: `deploy` runs the deployer, `gateway` the gateway, `guard` the guard |

```
make test
make build
bin/homecloud validate control-plane/internal/stack/testdata/jellyfin.yaml
bin/homecloud claims -reserved system/platform/reserved-hostnames.yaml STACK.yaml...
bin/homecloud render -domain example.com -env dev STACK.yaml
bin/homecloud apply -domain example.com -reserved /etc/homecloud/reserved-hostnames.yaml STACK.yaml   # on an installed machine, as root
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
