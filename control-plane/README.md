# Control plane

Go. One image, run in Swarm as the stack `control-plane` from the repo `system/control-plane` (`Dockerfile`, `ci/build.yaml`), as seven services:

| Service | Does |
| --- | --- |
| `deployer` | Makes the machine match Git: deploys every repo of `system` that holds a `compose.yaml` (this one last) and the apps of every other namespace, takes down what was deleted, keeps dashboards in Grafana, snapshots an app's data before each redeploy and carries out restore requests (`restores/*.yaml` in a namespace's config). Later also holds the SOPS key and writes change records. |
| `gateway` | The way in for people's agents: the MCP server Open WebUI calls with each person's Keycloak token, which reads and proposes pull requests as that person. Keeps each namespace's agents (from `agents/` in its config repo; each gets only its own tools), their accounts for grants, its channels and Open WebUI group, its members and approval checks, and its build account. Acts as an agent's own account where only a grant allows it (`merge_change`, outsiders' proposals), keeps open proposals up to date, and drives the browser for `screenshot` and `browse`. Changes nothing that runs. |
| `agents` | The agent service: the OpenAI-compatible API Open WebUI talks to, whose models are the namespaces' agents. Runs each chat with an agent in a sandbox of its own (image from `sandbox/`, built with this one): Pi, the agent harness, with a workspace kept in a volume per chat. Passes on to the sandboxes the LLM, whose key only it holds, and the agent's tools from the gateway, acting as the person chatting. Serves `ask_agent` and runs the agents' routines. |
| `sandbox-guard` | The Docker API for the agent service, letting through only sandboxes: the sandbox image, labelled, with limits, on the sandboxes network. |
| `browser` | Headless Chrome for the gateway's `screenshot` tool. Its only network reaches Traefik and the gateway; it loads the installation's pages only, as the person asking. |
| `guard` | The Docker API for the user runner, letting through only what pipeline jobs need. |
| `user-runner` | Gitea Actions runner of every namespace other than `system`. |

The same binary is installed on the host as `garrison`: the installer uses it to deploy once before the services exist, and admins use it for `applied`, `validate`, `render` and `apply`.

| Package | Does |
| --- | --- |
| `internal/stack` | Parses stack files (`config/stacks/<name>.yaml`, or standalone for the CLI), validates them against `schemas/stack.schema.json` and the rules a schema cannot express, fills in hostnames |
| `internal/hostnames` | Lists the hostnames a stack claims and rejects clashes and reserved names |
| `internal/render` | Renders one environment of a stack as a Compose file for `docker stack deploy` |
| `internal/apply` | Creates the ZFS datasets and Swarm secrets a stack needs, then runs `docker stack deploy` |
| `internal/system` | Deploys a system stack: a plain Compose file, its datasets and config hash |
| `internal/gitops` | Follows an organisation in Gitea and deploys every repo with a `compose.yaml`, the deployer itself last; and every other namespace's apps, `config/stacks/<name>.yaml`, from their source repos' branches and pull requests |
| `internal/hooks` | Gitea's webhooks: puts one on every organisation, its secret derived from the admin password, checks the signature of each call, and wakes the sync loops |
| `internal/dashboards` | Keeps `dashboards/*.json` of every repo in Grafana |
| `internal/agentapi` | The agent service: models, chat completions (streamed), turns in sandboxes and the API they use (LLM, tools, read-only Git), `ask_agent`, routines, the in-process loop for requests without a chat, Open WebUI's tasks |
| `internal/sandbox` | The sandboxes: Docker through the guard (attached to stdin and stdout), Pi's RPC protocol, one container per chat started, restarted and stopped when idle |
| `internal/agents` | Agent folders: reads and checks `agents/` and `skills/` of a config repo, with grants and routines; the operator; the system prompt |
| `internal/gateway` | The gateway: MCP tools, Open WebUI agents, groups and channels, members and approvals, agents' accounts and grants, private repos, keeping proposals current, build accounts |
| `internal/guard` | The guarded Docker API, for the user runner and, more narrowly, for the sandboxes |
| `cmd/garrison` | CLI over these: `deploy` runs the deployer, `gateway` the gateway, `agents` the agent service, `guard` the guards |

```
make test
make build
bin/garrison validate control-plane/internal/stack/testdata/jellyfin.yaml
bin/garrison claims -reserved system/platform/reserved-hostnames.yaml STACK.yaml...
bin/garrison render -domain example.com -env dev STACK.yaml
bin/garrison apply -domain example.com -reserved /etc/garrison/reserved-hostnames.yaml STACK.yaml   # on an installed machine, as root
```

Go runs in a container, so only Docker is needed. `make golden` rewrites the expected render output after an intended change.

## Naming

| Thing | Name |
| --- | --- |
| Swarm stack | the environment's hostname, e.g. `dev-jellyfin` (unique across the installation) |
| Dataset on the host (data root `/garrison/ns`) | `<data root>/<namespace>/stacks/<environment hostname>/<dataset>` |
| Share on the host | `<data root>/<namespace>/shares/<share>` |
| Swarm secret | `<environment hostname>.<secret>` |

What rendered stacks expect from the system stack is in `system/platform/contracts`.

## Not built yet

SOPS, migrations, change records, approvals, and restoring code and data together. See `docs/status.md`.

## Known gaps

- GPU rendering is untested on real hardware; see the spike in `docs/spec.md`, "Testing".
- Namespace networks are not rendered. Swarm gives every service its short name (`app`, `redis`) as a DNS alias on each network it joins, so two stacks sharing a network can resolve each other's services by accident. Routed services already share `traefik-public`. This needs a decision before stacks in a namespace are put on one network.
