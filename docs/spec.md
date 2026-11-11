# Home Cloud Platform — MVP Spec

A home cloud you run by talking to it, with Git underneath. This spec defines the first buildable version: a single Ubuntu machine running Docker Swarm, set up by one command, managed through a dashboard with a team chat where agents propose changes as pull requests.

## Goals

1. Install on a fresh Ubuntu machine with one command and a setup wizard.
2. Deploy apps (a homepage, Jellyfin, Immich) by conversation, each reachable on its own hostname with TLS.
3. Every change to the cluster goes through Git and leaves a change record with a snapshot.
4. People and agents share one team chat per level; agents act with the rights of whoever mentions them.
5. Coding agents build apps with the user, triggered from Gitea comments or chat.

## Scope

| In the MVP | Later |
| --- | --- |
| Single node, Docker Swarm | Multi-node, Kubernetes (k3s) with Longhorn or OpenEBS ZFS |
| Flat namespaces | — (nesting rejected) |
| Subdomains only, one level deep | — (path routing rejected) |
| One Let's Encrypt wildcard via DNS challenge | Workaround for Namecheap IP whitelisting |
| In-app team chat with @agent | Matrix or mobile chat app |
| ntfy for phone push | — |
| Coding agents in plain Docker sandboxes | NVIDIA OpenShell as sandbox runtime |
| ZFS snapshots, backup drive, restic to cloud | Storage shared across nodes over NFS |

## Core concepts

- **Root (system):** the platform itself and its shared services, the namespace `system`. One per installation.
- **Namespace:** a Gitea organisation: a flat group of stacks with its own members, channel and Docker network. No nesting.
- **Stack:** one app, possibly several services, in one repo of its namespace. App stacks are described in the platform's stack format; system stacks are plain Compose files, because they are the platform itself.
- **Service:** one container in a stack. Exactly one is the entry point.
- **Environment:** a branch mapped to a running copy of a stack, for example `dev` to dev-homepage.domain.com.
- **Agents:** operator agents (system, namespace) run the cluster; coding agents build apps.

## System stack

Each component is a repo of the `system` organisation in Gitea, named after its stack. The installer deploys the first six in this order, because each depends on the ones before it; they are what it takes to build and run the controller. The controller then deploys every repo of `system`, these included.

| Order | Repo | Component | Purpose |
| --- | --- | --- | --- |
| 1 | `postgres` | Postgres | Shared database for Keycloak, Gitea, Grafana |
| 2 | `traefik` | Traefik | Routing, wildcard TLS, login check for tools without their own |
| 3 | `keycloak` | Keycloak | Single sign-on for all people, in the realm `homecloud` |
| 4 | `sso` | Login proxy (oauth2-proxy) | Answers Traefik's login check and sends people without a session to Keycloak |
| 5 | `gitea` | Gitea + registry | Code, config, history, images |
| 6 | `runner` | Gitea Actions runner | Builds the platform's own images; serves the `system` organisation only, because its jobs reach the Docker daemon |
| 7 | `controller` | Controller and control plane | Applies Git to the cluster, holds the SOPS key, writes change records; the control plane serves the agents' MCP tools. Its repo holds its source; Gitea Actions builds its image and the controller deploys it, replacing itself |
| — | `grafana`, `prometheus`, `loki` | Grafana; Prometheus, node-exporter, cAdvisor; Loki, Alloy | Dashboards, metrics, logs |
| — | | Agent orchestrator | Runs operator and coding agents |
| — | `openwebui` | Open WebUI | Chat with one agent per namespace (see Chat) |
| — | | Platform UI | Dashboard |
| — | | Alertmanager, ntfy | Alerts and phone push |

A repo of `system` without a `compose.yaml` is not deployed: `system/platform` holds the public contracts and Grafana dashboards.

On the host, not in containers: ZFS, Sanoid for scheduled snapshots, restic for offsite backup.

## Installation

1. Run the install script on fresh Ubuntu. It installs Docker and ZFS tools and starts a bootstrap container with Docker Compose.
2. The setup wizard opens on a local port, protected by a one-time code printed in the terminal.
3. The wizard scans hardware and drives, warns if there is no second drive for backups, and asks for: LLM API key, domain, DNS provider token, admin account.
4. It runs swarm init, creates the ZFS pool, and deploys the system stack in order.
5. It pushes one repo per system stack into the `system` organisation. Gitea Actions builds the controller image from `system/controller`; the controller is deployed from it, takes over, and the bootstrap container removes itself.

Acceptance: on a clean machine, the user reaches the dashboard at their domain with valid TLS and logs in through Keycloak, with no manual steps beyond the wizard.

## Repositories

| Repository | Scope | Contents | Who writes |
| --- | --- | --- | --- |
| System repos | The `system` organisation, one per system stack | `compose.yaml` and the files it uses; for the controller also its source and build | Root admins, system agent via PR |
| Platform repo | `system/platform` | Grafana dashboards, public contracts (e.g. Traefik label schema) | Root admins, system agent via PR |
| Config repo | One per namespace, `<namespace>/config` | `members.yaml` (admins and members); later repos, autonomy levels, branch rights, hostnames, SOPS-encrypted secrets | Namespace admins, namespace agent via PR |
| History repo | One per namespace, `<namespace>/history` | Change records, one per deploy | Controller only |
| App repos | Inside a namespace, one per stack | Code, `stack.yaml`, numbered migrations | Members, coding agents |

Config files are JSON or YAML, validated by JSON Schema. Every repo, including the system repos and every namespace's config and history, is readable by everyone who is signed in; nothing is readable without signing in. Secret values stay SOPS-encrypted and readable by no one.

## Stack format

```yaml
stack: jellyfin
namespace: media
entrypoint: app

services:
  app:
    image: jellyfin/jellyfin:10.10
    port: 8096
    devices: [gpu]
    resources: { memory: 4Gi }
    volumes:
      config: { dataset: jellyfin-config, size: 5Gi }
      media:  { share: media-library, readonly: true }
    secrets: [jellyfin-api-key]
  admin:
    image: example/jellyfin-admin:1.0
    port: 8080
    public: admin
  redis:
    image: redis:7

environments:
  main: {}
  dev:  { private: true }
```

- The entry point gets the environment's hostname; `public: admin` gives `<hostname>-admin`, so jellyfin-admin.domain.com on main and dev-jellyfin-admin.domain.com on dev; other services stay internal.
- Hostnames are one level deep, so one wildcard certificate covers everything.
- A stack claims exactly the hostnames it declares: one per environment, plus `<hostname>-<public>` for each public service. The main environment's hostname is the stack name; every other environment's is `<environment>-<stack>`. The hostname follows from the rule, so `hostname` is optional and must match the rule when given. Nothing is claimed by prefix.
- The controller rejects a deploy whose claimed hostnames are already held by another stack or reserved by root. Claims are first come, first served across all namespaces.
- `dataset` is a ZFS dataset owned by the stack; `share` is a library other stacks in the namespace can read.
- A volume is mounted at `/<volume name>` in the container, so `config` appears at `/config`; `path` overrides this.
- Each environment is its own copy of the stack with its own data and secrets. `private: true` puts it behind the Keycloak login.
- `devices: [gpu]` is rendered to a Swarm generic resource. The installer sets the NVIDIA runtime as Docker's default and advertises the GPU as a generic resource on the node when the hardware scan finds one.
- The stack file carries no policy. Autonomy levels and branch rights live only in the namespace config repo, so nobody who can write an app repo can change them. The controller rejects a `stack.yaml` with policy keys, or any other key the schema does not know.
- `env` sets environment variables; `{{url}}` and `{{host}}` in a value become the environment's address. An image may say `{{registry}}/<namespace>/<name>:{{commit}}`: the controller then deploys a commit only once the namespace's pipeline has pushed that image, so a failing pipeline leaves the app at its last good commit.
- In a namespace other than `system`, every repo with a `stack.yaml` is an app; its stack name must equal the repo's name and its namespace the organisation's. Each environment is deployed from the branch of the same name, and taken down when the branch or repo is deleted.
- The format names no orchestrator; the controller renders it to Swarm in the MVP.

## Pipelines

Gitea Actions runs two runners:

- The **system runner** serves only the `system` organisation, which only root admins can change. Its jobs can use Docker, which is root on the machine; it builds the controller.
- The **user runner** serves every other namespace. Its jobs are containers on a builds network, where they reach Gitea and BuildKit and nothing else of the platform. It reaches Docker only through the controller's guard, which refuses anything that would let a job out of its container: privileges, capabilities, devices, security options, host namespaces, host mounts, other networks, and access to containers it did not create. Images are built by BuildKit, never by the host's Docker.

Each namespace has a build account, kept by the control plane, which may push that namespace's images and nothing else; its token is the organisation's Actions secret. Namespaces are created by root admins only.

## Controller and change history

The controller watches Gitea and applies merged changes. In the MVP it follows the `system` organisation: every repo there that holds a `compose.yaml` on its default branch is deployed as the stack named after the repo, and a stack whose repo is deleted is taken down.

1. **Pre-deploy:** take ZFS snapshots of affected datasets; record running services, versions and config.
2. **Apply:** decrypt secrets into Swarm secrets, render the stack, deploy, run migrations if present.
3. **Post-deploy:** record new state and health checks.
4. **Record:** commit one change record to the history repo, linking commit, PR, snapshots and before/after state; add a Grafana annotation.

Migrations exist only when a change needs a step a redeploy cannot do. Each change record notes whether a migration ran.

Restore from the History tab rolls back to a change record as a whole: the controller restores the recorded snapshots and redeploys the commit and config recorded with them, so data and code always match and a migration is undone together with the version that needed it. Before confirming, the History tab lists the deploys and migrations that will be undone and warns that data written since the snapshot is lost. The controller snapshots the current state first, and the restore gets its own change record, so a restore can itself be undone.

## Rights and autonomy

- An agent always acts with the rights of the person who mentioned it, never more.
- When an agent acts without a mention, for example on an alert, it has read-only rights in its own scope (logs, metrics, state, repos) and may only propose. Its proposals always need a human to approve them, whatever the autonomy level; the approver's rights are the ones checked.
- Everyone who is signed in can see everything: every repo, stack, log and history. Seeing is not changing.
- Rights to change flow downward only: root, then namespace, then repository. No scope can change the one above it.
- Agents propose; the controller enforces. Approvals are checked by the controller, never by the agent.
- Nobody approves their own role change. Namespace config changes always need a human merge.
- A namespace's `config/members.yaml` names its admins and members; the control plane keeps the Gitea teams `admins` and `members` in line with it. Its `main` branch takes changes only by pull request, approved by an admin of the namespace or a root admin who neither wrote it nor has their role changed by it; the control plane checks this and posts the required status `homecloud/approval`. The scenarios in `docs/scenarios.md` play this out through the agents.
- Per repo and branch, an autonomy level decides how far agents go:

| Level | Agent may | Human does |
| --- | --- | --- |
| Propose | Push a branch, open a PR, build a preview | Review, merge, deploy |
| Merge | Merge after checks pass | Approve deployment |
| Autonomous | Merge and deploy | Watch, roll back if needed |

- Per repo and branch autonomy is set in the namespace config repo and nowhere else.
- Secrets are either generated by the controller or entered by a person in a write-only form on the Config tab. The form sends the value straight to the controller, which SOPS-encrypts and commits it; the value is never shown again, only replaced. The LLM key and DNS token from the setup wizard are stored the same way in the platform repo.
- Agents reference secrets by name only and can never read values. An agent that needs a secret from outside asks a person to enter it in the form.
- Every request, including rejected ones, stays in Git as the audit log.

## Agents

| Agent | Scope | Lifetime | Job |
| --- | --- | --- | --- |
| System agent | Root | Long-running | System repos, system alerts, cross-namespace upgrades via PRs into each namespace |
| Namespace agent | One namespace | Long-running | Namespace config, members, stacks, namespace alerts |
| Coding agent | One repo | Per task, sandboxed | Builds the app with the user, pushes to a branch, replies in thread |

- Agents speak an OpenAI-compatible API (Ollama by default) and use the LLM key from setup. Namespace agents run in Open WebUI with the control plane's MCP tools (see Chat).
- Coding agents start from an @mention in a Gitea comment (commit, PR, issue) via webhook, or from the team chat. Each task gets a fresh Docker container with the repo checked out, removed when done.
- A namespace agent that finds a cause above its scope (e.g. a full disk) escalates a request to the system agent.
- Users can still work locally with their own coding tools and push to the same repos.

## Dashboard

Pages follow the levels: Home (cluster), Namespace, Stack, Service. Every page has the same tabs:

- **Overview:** status, hostnames, warnings (e.g. missing backup drive).
- **Logs and metrics:** one generic Grafana dashboard with template variables, embedded and filtered to the current level.

Dashboards are code: any repo can hold Grafana dashboards as `dashboards/*.json`. The controller keeps them in a Grafana folder named after the namespace, as they are on `main`; edits made in Grafana are put back.
- **Config:** a form generated from the JSON Schema, plus a Monaco code editor. Save validates and commits. Includes the write-only secrets form.
- **History:** change records and snapshots, with restore and a warning listing what a restore will undo.

An inbox collects everything waiting for the user: PRs to approve, previews to review, failed deploys.

## Chat

- Open WebUI, at `chat.<domain>`, is the chat. People sign in through Keycloak; root admins are its admins.
- Each namespace has a channel there, named after it: the team chat, permanent and shared by everyone signed in. Only admins may change a channel, and the control plane puts back what it set. Mentioning the namespace's agent, `@agent-<namespace>` (e.g. `@agent-system`), makes it answer in a thread; a pinned message in each channel explains how.
- Each namespace's agent is a model with that namespace's prompt and the control plane's tools. The control plane keeps one agent and one channel per Gitea organisation, adding and removing them as organisations come and go. People can also chat with an agent privately.
- The control plane is an MCP server. Open WebUI calls it with the signed-in person's Keycloak access token, so every tool acts with that person's rights: reads and proposals go to Gitea as them. It is reachable only from Open WebUI, not from outside.
- Agents can list namespaces and stacks, show a stack's tasks and errors, read service logs, show CPU and memory use, run Prometheus queries, read repos, list open changes, and propose a change to a file.
- `propose_change` opens a pull request as the person. It cannot merge or deploy: a person allowed to merges in Gitea, and the controller deploys `main`. The agent always gives the link.
- Not yet: conversational setup on a draft branch.

## Notifications

- **Sources:** Alertmanager (disk, node, backup, service health), controller (failed deploys, PRs waiting), agents (task updates), chat (@mentions).
- **Routing:** by namespace to its members, its channel and its agent; system alerts to root admins and the system agent.
- **Delivery:** in-app inbox, plus phone push through ntfy.
- People are notified immediately; agents investigate in parallel with read-only rights and follow up with a cause and a proposed fix for a person to approve.

## Storage and backup

- ZFS on the main drive, one dataset per stack volume, shared libraries as separate datasets.
- Sanoid takes scheduled snapshots; the controller snapshots before every deploy.
- A second drive is a dedicated backup drive; snapshots are copied to it regularly. Without it, the dashboard warns until one is added.
- restic sends the backup drive's contents, encrypted, to cloud storage.

## MVP acceptance

1. One-command install on fresh Ubuntu ends at a working dashboard with TLS and SSO.
2. The user creates a namespace "homepage" by chat, adds a member, and approves it with change cards.
3. A coding agent, mentioned on an issue, builds a site in an empty repo; it appears at dev-homepage.domain.com, and merging dev into main publishes homepage.domain.com.
4. Jellyfin deploys from a stack file with GPU access and a shared media library.
5. Every deploy has a change record and snapshot; a restore from History works.
6. A member without admin rights cannot gain them through the agent, nor raise an autonomy level by editing an app repo.
7. A failed service triggers a phone push and an agent follow-up in the channel.

## Testing

The installer and user stories are tested end to end on throwaway VMs.

1. **Scripted VMs:** Multipass creates a fresh Ubuntu VM per run, clones the repo and runs the install script. Variants with and without a second virtual disk cover both the backup-drive warning and the normal path.
2. **Snapshots:** a VM snapshot after the system stack is up lets later tests start from there instead of reinstalling Postgres, Traefik, Keycloak and Gitea each time.
3. **Headless install:** the installer accepts an answers file, so tests run without typing. Playwright drives the flows that must go through the real UI, such as login and approving a change card.
4. **Fake externals:** Pebble replaces Let's Encrypt, a local DNS server replaces the DNS provider, and a mock LLM returns fixed answers so runs are deterministic.
5. **AI exploratory testing:** separately from the scripted suite, an agent works through the user stories like a real person and reports anything that breaks.

6. **GPU on real hardware:** the VMs have no GPU, so the GPU part of acceptance item 4 is tested on a physical machine. A spike before the controller is built confirms that a Swarm service gets the GPU through a generic resource with the NVIDIA runtime as default.

The MVP acceptance list above becomes the end-to-end test suite.

## Open questions

- Namecheap DNS challenge with a changing home IP.
- Build the agent loop in-house or wrap existing agents.
- Whether agents may provision secrets for sub-agents.
- When to move from plain Docker sandboxes to OpenShell.
