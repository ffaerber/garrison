# Implementation status

What is built, measured against `docs/spec.md`. Updated with each slice.

## Works, tested end to end on a fresh Ubuntu 26.04 VM

`test/vm/steps.sh` runs the end-to-end test as a chain of steps on a Multipass VM (base, install, login, platform, agent), each saved as a VM snapshot when green; see `test/README.md`.

- `install.sh --answers FILE`: installs Docker and ZFS, initialises Swarm, creates the ZFS pool, generates secrets, deploys the bootstrap stacks in order (Postgres, Traefik, Keycloak, login proxy, Gitea, Actions runner), waits for Gitea Actions to build the control plane, and hands over to its deployer. Safe to run again.
- Namespaces are Gitea organisations. The system stack is the organisation `system`, one repo per stack: `postgres`, `traefik`, `keycloak`, `sso`, `gitea`, `runner`, `control-plane`, `grafana`, `prometheus`, `loki`, and `platform` for contracts. Everyone signed in can read them; only root admins can change them. Gitea shows nothing without signing in.
- Deployer: runs in Swarm and deploys every repo of `system` that holds a `compose.yaml`, itself last, as soon as Gitea's webhook reports a change (and every 15 s otherwise); a deleted repo's stack is taken down. Its image is built from `system/control-plane` by Gitea Actions on the `system` organisation's runner, pushed to Gitea's registry, and deployed by a commit that sets the tag in that repo's `compose.yaml`.
- Single sign-on: people sign in to the Keycloak realm `homecloud`. Private environments redirect to that login and pass the signed-in person's identity to the app. Gitea and Grafana sign in through Keycloak.
- Roles: the Keycloak group `root-admins` holds the people who may change the platform. Its members are Gitea admins and Grafana admins; everyone else views. Both re-read the groups at every login. `system/keycloak/add-user.sh` adds people.
- Monitoring: Grafana (`grafana.<domain>`) with Prometheus (node, container and Traefik metrics) and Loki (container logs), neither reachable from outside. Dashboards are code: any repo's `dashboards/*.json` goes into a Grafana folder per namespace, kept as on `main` by the deployer; `system/grafana` brings Overview, Stack, Logs, Machine, Traffic and Agents. Agents' sandboxes are measured but their output, the conversations, is kept out of Loki.
- Chat: Open WebUI at `chat.<domain>`, signed in through Keycloak. Every namespace has a permanent channel, its team chat, which only its people read (`system`'s everyone reads), and agents that answer there when mentioned, `@<namespace>/<agent>`. The agents are folders in the namespace's config repo (`agents/<name>/agent.yaml`, `prompt.md`, skills), the operator by default; Open WebUI talks to them through the agent service in the control plane, which runs each chat with an agent in a sandbox of its own: a container with Pi, the agent harness, and a workspace kept in a volume per chat, which gets the LLM (only the agent service has the key) and the agent's tools at the gateway through the agent service, and can clone the repos the person may read and propose its changes as a pull request by them; the gateway keeps the agents and channels in sync with Gitea and serves each agent only its own tools, acting with each person's own Keycloak token: they read stacks, logs, metrics and repos, and propose changes as pull requests a person merges in Gitea. Agents are for everyone, except those that work on a private repo: only the namespace's people see them, and they answer only in private chats and the namespace's channel. A namespace can give an agent grants: an account of its own in Gitea, writing only to the repos granted, that proposes for people who may not write there and merges into the branches granted, for the groups each grant names. Sandboxes run under gVisor, as root, so agents install what a task needs, and reach the internet only through the agent service's proxy, as far as their `agent.yaml` says (`internet`: the package registries, named domains, or all; never the home network; the registries at most for agents on private repos). Agents ask each other for work (`ask_agent`), run routines on a schedule or when a branch moves, with nobody watching, and use the apps' pages as the person (`browse`). The gateway brings open proposals up to date when their base moves. The LLM is Ollama's `gpt-oss:120b` by default.
- Apps: in every namespace other than `system`, each file `config/stacks/<name>.yaml` is an app. With a `source` repo, it is deployed from that repo's branches (`main` → `<name>.<domain>`, `dev` → `dev-<name>.<domain>`) once its pipeline has built the commit's image, and with a `preview` environment each open pull request gets one, `pr-<n>-<name>.<domain>`, until it closes; without one, as the file says. App repos hold only code. The examples: `todo/app` (from `examples/todo/app`) is a Go and Postgres todo app, a list of their own for everyone signed in, with its tests, Dockerfile and pipeline, deployed by `todo/config/stacks/todo.yaml`; `homepage/app` (from `examples/homepage/app`) is a hello-world page, deployed by `homepage/config/stacks/homepage.yaml` at `homepage.<domain>`, open to everyone.
- Snapshots and restore: Sanoid snapshots all data, the platform's and the apps', hourly, daily and monthly (24, 30, 6 kept). Before redeploying an environment that holds data, the deployer snapshots its datasets (`deploy-<time>-<commit>`, 10 kept) and does not deploy if it cannot. To bring an environment's data back, a person (or an agent, which finds the snapshots with `list_snapshots`) proposes `restores/<name>.yaml` in the namespace's config, naming the stack, environment and snapshot; the approval check validates it. Once merged, the deployer snapshots the current state (`restore-<time>`, so a restore can be undone), stops the environment's services, copies each of its datasets back from the snapshot and deploys it again. Each request runs once. Shares are not restored.
- Namespace members: `<namespace>/config/members.yaml`, applied to Gitea teams; changes only by approved pull request, and nobody approves their own role change. The agents can show members and propose changes but never approve or merge (`docs/scenarios.md`).
- Pipelines: a system runner for `system` and a user runner for every other namespace, whose jobs reach Docker only through the control plane's guard and build images with BuildKit; one build account per namespace for its registry.
- Reboot: `homecloud-repair.service` replaces Swarm tasks that Docker restarted without their secrets or configs.
- CLI on the host (`homecloud`): validates a stack file, claims its hostnames, creates its ZFS datasets and Swarm secrets, and deploys it behind Traefik; `applied` shows what the deployer deployed.

## Differs from the spec for now

| Spec | Now |
| --- | --- |
| Setup wizard in a bootstrap container | Answers file only; no wizard |
| Let's Encrypt wildcard via DNS challenge; Pebble in tests | Self-signed wildcard certificate generated by the installer |
| ZFS pool on a drive | A sparse file on the root disk, unless `POOL_DEVICE` is set |
| Hostname claims come from Git | Kept in `/var/lib/homecloud/claims.json` |
| Restore from the History tab, code and data together, with a change record | A restore request, `restores/<name>.yaml` in the namespace's config, brings back an environment's data only |
| Tests with and without a second virtual disk | One disk; Multipass cannot attach a second |
| — | Test installs pull Docker Hub images through `mirror.gcr.io` (`REGISTRY_MIRROR`) |

## Not started

History repos and the rest of the config repo (secrets); SOPS, change records and approvals in the deployer; the backup drive and restic; the dashboard, alerts and ntfy, Playwright tests.

## Known limits

Problems the platform cannot close itself, as Gitea, Open WebUI or Docker offer no way to, are in [unfixable.md](unfixable.md); ideas for later in [ideas.md](ideas.md).

- The apps, `dev-` environments and previews of private repos can be opened by everyone signed in, not only the namespace's people (an idea to fix it is in [ideas.md](ideas.md)).

- The deployer pulls its image from the local Docker daemon, which built it. A second node would need registry credentials.
- The system runner's jobs can use the Docker daemon, which is root on the machine. It serves only the `system` organisation, which only root admins can write to.
- Agent sandboxes share one network with the agent service, so they can reach each other; nothing listens in them, but an agent could start a server. Their volumes are not yet removed when a chat is unused for long.
- An agent with internet can be talked into sending out what it reads. Agents on private repos reach only the registries, and an agent reaching more clones only public repos; but its platform tools still read what the person may read, private files included, and npm and crates.io accept uploads from anyone with an account of theirs, so even the registries are not a closed door. Give wide internet to agents for people you trust with the namespace's data.
- What a sandbox installs goes to its container's layer on the root disk, without a size limit, until the container stops.
- In a channel, one sandbox serves all threads with an agent: Open WebUI names the channel, not the thread.
- Posts of routines and background answers come from the gateway's Open WebUI account, with the agent's name in bold, into the channel rather than a thread.
- Routines keep their schedule in memory: a restart of the agent service misses pushes in between. A background `ask_agent` acts with the person's Keycloak token, which lasts five minutes.
- Previews' datasets and secrets stay after a preview is taken down.
- Snapshots stay on the pool's own disk: no backup drive or off-site copy yet. A restore copies files back rather than rolling back, which takes as long as the data is large, and needs the pool's free space for the data changed since. Change records and a restore of code and data together, as the spec's History describes, are not built: a restore brings back data only, and the app keeps running its current version.
- BuildKit runs privileged, outside Swarm. Build steps run in its own sandboxes without privileges; a flaw in BuildKit itself would reach the host. Its build cache is shared between namespaces.
