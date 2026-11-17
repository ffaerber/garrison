# Garrison — MVP Spec

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
| Coding agents in gVisor sandboxes, internet per agent through a proxy | NVIDIA OpenShell as sandbox runtime |
| ZFS snapshots, backup drive, restic to cloud | Storage shared across nodes over NFS |

## Core concepts

- **Root (garrison):** the platform itself and its shared services, the namespace `garrison`. One per installation.
- **Namespace:** a Gitea organisation: a flat group of stacks with its own members, channel and Docker network. No nesting.
- **Stack:** one app, possibly several services, described in the platform's stack format in its namespace's config repo, `config/stacks/<name>.yaml`. An app with code of its own names the repo holding it as its source; the repo itself holds nothing of the home cloud. System stacks are plain Compose files, one repo each, because they are the platform itself.
- **Service:** one container in a stack. Exactly one is the entry point.
- **Environment:** a branch mapped to a running copy of a stack, for example `dev` to dev-homepage.domain.com.
- **Agents:** the home cloud's agents, defined once in `garrison` and working in every namespace: the operator runs the cluster and its namespaces; coding agents build apps.

## System stack

Each component is a repo of the `garrison` organisation in Gitea, named after its stack. The installer deploys the first six in this order, because each depends on the ones before it; they are what it takes to build and run the deployer. The deployer then deploys every repo of `garrison`, these included.

| Order | Repo | Component | Purpose |
| --- | --- | --- | --- |
| 1 | `postgres` | Postgres | Shared database for Keycloak, Gitea, Grafana |
| 2 | `traefik` | Traefik | Routing, wildcard TLS, login check for tools without their own |
| 3 | `keycloak` | Keycloak | Single sign-on for all people, in the realm `garrison` |
| 4 | `sso` | Login proxy (oauth2-proxy) | Answers Traefik's login check and sends people without a session to Keycloak |
| 5 | `gitea` | Gitea + registry | Code, config, history, images |
| 6 | `runner` | Gitea Actions runner | Builds the platform's own images; serves the `garrison` organisation only, because its jobs reach the Docker daemon |
| 7 | `control-plane` | Deployer, gateway, agent service, guards and user runner | The deployer applies Git to the cluster, holds the SOPS key and writes change records; the gateway serves the agents' MCP tools and keeps namespaces' members, channels and build accounts; the agent service runs each chat with an agent in a sandbox of its own (Pi, image `garrison/sandbox`). Its repo holds its source; Gitea Actions builds its image and the deployer deploys it, replacing itself |
| — | `grafana`, `prometheus`, `loki` | Grafana; Prometheus, node-exporter, cAdvisor; Loki, Alloy | Dashboards, metrics, logs |
| — | `openwebui` | Open WebUI | Chat with one agent per namespace (see Chat) |
| — | | Platform UI | Dashboard |
| — | | Alertmanager, ntfy | Alerts and phone push |

A repo of `garrison` without a `compose.yaml` is not deployed: `garrison/platform` holds the public contracts and Grafana dashboards.

On the host, not in containers: ZFS, Sanoid for scheduled snapshots, restic for offsite backup.

## Installation

1. Run the install script on fresh Ubuntu. It installs Docker and ZFS tools and starts a bootstrap container with Docker Compose.
2. The setup wizard opens on a local port, protected by a one-time code printed in the terminal.
3. The wizard scans hardware and drives, warns if there is no second drive for backups, and asks for: LLM API key, domain, DNS provider token, admin account.
4. It runs swarm init, creates the ZFS pool, and deploys the system stack in order.
5. It pushes one repo per system stack into the `garrison` organisation. Gitea Actions builds the control plane image from `garrison/control-plane`; the control plane is deployed from it, its deployer takes over, and the bootstrap container removes itself.

Acceptance: on a clean machine, the user reaches the dashboard at their domain with valid TLS and logs in through Keycloak, with no manual steps beyond the wizard.

## Repositories

| Repository | Scope | Contents | Who writes |
| --- | --- | --- | --- |
| Platform repos | The `garrison` organisation, one per system stack | `compose.yaml` and the files it uses; for the deployer also its source and build | Root admins, garrison's operator via PR |
| Platform repo | `garrison/platform` | Grafana dashboards, public contracts (e.g. Traefik label schema) | Root admins, garrison's operator via PR |
| Config repo | One per namespace, `<namespace>/config` | `members.yaml` (admins and members; not in `garrison`), `grants.yaml` and `routines.yaml` (what the agents may do and do by themselves there), `stacks/` (its apps; not in `garrison`), `secrets/`; in `garrison` only, `agents/` and `skills/`: every agent of the home cloud | Namespace admins (root admins in `garrison`), agents via PR |
| History repo | One per namespace, `<namespace>/history` | Change records, one per deploy | Deployer only |
| App repos | Inside a namespace, one per app with code of its own | Code, Dockerfile, tests, the pipeline that builds its image, numbered migrations | Members, coding agents |

Config files are JSON or YAML, validated by JSON Schema. Every repo, including the platform repos and every namespace's config and history, is readable by everyone who is signed in; nothing is readable without signing in. Secret values stay SOPS-encrypted and readable by no one.

## Stack format

`media/config/stacks/jellyfin.yaml`: its place names the stack and its namespace.

```yaml
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
```

An app with code of its own names its repo as `source`; its branches and pull requests are then its environments, each deployed at its newest commit:

```yaml
# todo/config/stacks/todo.yaml: its code is the repo todo/app
source: app
entrypoint: web
services:
  web: { image: "{{registry}}/todo/app:{{commit}}", port: 8080 }
environments:
  main:    { private: true }
  dev:     { private: true }   # the branch dev
  preview: { private: true }   # each open pull request, as pr-<n>
```

- The entry point gets the environment's hostname; `public: admin` gives `<hostname>-admin`, so jellyfin-admin.domain.com on main and dev-jellyfin-admin.domain.com on dev; other services stay internal.
- Hostnames are one level deep, so one wildcard certificate covers everything.
- A stack claims exactly the hostnames it declares: one per environment, plus `<hostname>-<public>` for each public service. The main environment's hostname is the stack name; every other environment's is `<environment>-<stack>`. The hostname follows from the rule, so `hostname` is optional and must match the rule when given. Nothing is claimed by prefix.
- The deployer rejects a deploy whose claimed hostnames are already held by another stack or reserved by root. Claims are first come, first served across all namespaces.
- `dataset` is a ZFS dataset owned by the stack; `share` is a library other stacks in the namespace can read.
- A volume is mounted at `/<volume name>` in the container, so `config` appears at `/config`; `path` overrides this.
- Each environment is its own copy of the stack with its own data and secrets. `private: true` puts it behind the Keycloak login. Apps share Traefik's network, so one could call another directly and name any person in the login's headers: a private environment's services get an ingress key (`GARRISON_INGRESS_KEY`), which Traefik adds to every request it passes on (`X-Garrison-Ingress`), and an app believes the login's headers only with it.
- `devices: [gpu]` is rendered to a Swarm generic resource. The installer sets the NVIDIA runtime as Docker's default and advertises the GPU as a generic resource on the node when the hardware scan finds one.
- The stack file carries no policy, and lives in the namespace's config repo, so changing how an app runs needs an admin's approval, like `members.yaml`; nobody who can only write an app repo can change it. The deployer and the approval check reject any key the schema does not know.
- `env` sets environment variables; `{{url}}` and `{{host}}` in a value become the environment's address. An image may say `{{registry}}/<namespace>/<name>:{{commit}}`: the deployer then deploys a commit only once the namespace's pipeline has pushed that image, so a failing pipeline leaves the app at its last good commit.
- In a namespace other than `garrison`, every file `config/stacks/<name>.yaml` on `main` is an app named `<name>`. With a `source`, each environment is deployed from the source repo's branch of the same name, at its newest commit, and taken down when the branch is deleted; with `preview`, each open pull request of the source repo gets its environment `pr-<n>`. Without a source, only `main` exists, deployed as the file says. Removing the file takes the app down; a change to it redeploys it.
- The format names no orchestrator; the deployer renders it to Swarm in the MVP.

## Pipelines

Gitea Actions runs two runners:

- The **system runner** serves only the `garrison` organisation, which only root admins can change. Its jobs can use Docker, which is root on the machine; it builds the control plane.
- The **user runner** serves every other namespace. Its jobs are containers on a builds network, where they reach Gitea and BuildKit and nothing else of the platform. It reaches Docker only through the control plane's guard, which refuses anything that would let a job out of its container: privileges, capabilities, devices, security options, host namespaces, host mounts, other networks, and access to containers it did not create. Images are built by BuildKit, never by the host's Docker.
- **Scheduled jobs** of an app are Gitea Actions workflows `on: schedule` (cron) in its repo, run by the user runner like its pipeline (`runs-on: garrison-user`, Alpine, so `shell: sh`). Gitea starts the default branch's schedules, at most about every two minutes; a workflow is disabled from the repo's Actions tab. Agents on a schedule are routines (below).

Each namespace has a build account, kept by the gateway, which may push that namespace's images and nothing else; its token is the organisation's Actions secret. Namespaces are created by root admins only.

## Deployer and change history

The deployer watches Gitea and applies merged changes. In the MVP it follows the `garrison` organisation: every repo there that holds a `compose.yaml` on its default branch is deployed as the stack named after the repo, and a stack whose repo is deleted is taken down. Gitea tells the deployer and the gateway about every push, branch, repo, pull request, review, commit status and finished pipeline run through a webhook on every organisation, signed with a secret each derives from the admin password; they act on it at once, and still check on their own every few seconds in case a call is lost.

1. **Pre-deploy:** take ZFS snapshots of affected datasets; record running services, versions and config.
2. **Apply:** decrypt secrets into Swarm secrets, render the stack, deploy, run migrations if present.
3. **Post-deploy:** record new state and health checks.
4. **Record:** commit one change record to the history repo, linking commit, PR, snapshots and before/after state; add a Grafana annotation.

Migrations exist only when a change needs a step a redeploy cannot do. Each change record notes whether a migration ran.

Restore from the History tab rolls back to a change record as a whole: the deployer restores the recorded snapshots and redeploys the commit and config recorded with them, so data and code always match and a migration is undone together with the version that needed it. Before confirming, the History tab lists the deploys and migrations that will be undone and warns that data written since the snapshot is lost. The deployer snapshots the current state first, and the restore gets its own change record, so a restore can itself be undone.

## Rights and autonomy

- An agent acts with the rights of the person who mentioned it. It has more only through grants a namespace gives it in `grants.yaml` of its config repo: an autonomy level per repo and branch of that namespace, and the groups of that namespace whose requests may use it (`for`). An outsider never uses an agent's grants beyond proposing, and no detour through another agent adds rights. See [agents.md](agents.md), Accounts and grants.
- When an agent acts without a mention, for example on an alert, it has read-only rights in its own scope (logs, metrics, state, repos) and may only propose, unless a grant allows unattended use for the run's owner. Its proposals always need a human to approve them; the approver's rights are the ones checked.
- Everyone who is signed in can see everything: every repo, stack, log and history, except private repos, which only their namespace's people see. Seeing is not changing. Rights are per namespace, never per repo: a namespace's people change all its repos. An agent that works on a private repo is private too: only the namespace's people see it, and it answers only in private chats and the namespace's channel.
- Rights to change flow downward only: root, then namespace, then repository. No scope can change the one above it.
- Agents propose; the deployer enforces. Approvals are checked by the deployer, never by the agent.
- Nobody approves their own role change. Namespace config changes always need a human merge.
- A namespace's `config/members.yaml` names its admins and members; the gateway keeps the Gitea teams `admins` and `members` in line with it. Its `main` branch takes changes only by pull request, approved by an admin of the namespace or a root admin who neither wrote it (opened it or pushed to its branch, as Gitea's activity feed of the repo tells) nor has their role changed by it; the gateway checks this and posts the required status `garrison/approval`. The scenarios in `docs/scenarios.md` play this out through the agents.
- A namespace is set up within seconds of being made, by an agent or by hand in Gitea: Gitea sends no event for a new organisation, so the gateway and the deployer list the organisations every two seconds and wake every sync when one is new (the webhooks do the rest: repos, pushes, pull requests, pipelines). `create_namespace` answers only once the config repo and the channel are there, and the dashboard's feed says the namespace is set up.
- A namespace's other repos, its apps', follow its size. With one person in `members.yaml`, they push and merge to `main` freely. With two or more, `main` takes no direct pushes, and a pull request into it needs the approval of another of the namespace's people (an approval goes stale with new commits); so nobody deploys alone what the others have not seen. Other branches, `dev` among them, stay free. The gateway keeps this rule on every app repo as people join and leave; an agent's `merge_change` into `main` needs that approval too.
- Per repo and branch, an autonomy level decides how far agents go:

| Level | Agent may | Human does |
| --- | --- | --- |
| Propose | Push a branch, open a PR, build a preview | Review, merge, deploy |
| Merge | Merge after checks pass | Approve deployment |
| Autonomous | Merge and deploy | Watch, roll back if needed |

- Per repo and branch autonomy is set in the namespace config repo and nowhere else, as grants in `grants.yaml`, each naming one of garrison's agents. An agent with grants in a namespace has an account of its own there in Gitea; it reaches a granted branch only by merging a pull request, and pushes only its own `agent/<name>/*` branches.
- Secrets: see Secrets below.
- Every request, including rejected ones, stays in Git as the audit log.

## Secrets

- **Where:** a namespace's secrets are files in `<namespace>/config/secrets/`, SOPS-encrypted with age. Everyone signed in can read the repos, so they see names and encrypted values, never the values; the same holds for backups, mirrors and the Git history.
- **Entering:** a namespace's admins and root admins set, replace and delete its secrets in a write-only form, the dashboard's Secrets page. The dashboard encrypts the value with the namespace's public keys, which the deployer gives it, so it can write secrets but never read one, and commits the file to config's `main` as the platform's account, the only one that may push there, with the person as the commit's author; the feed records it. The value is never shown again, only replaced. With sops, a person can also propose an encrypted file by pull request: config's `.sops.yaml` names the keys. The LLM key and DNS token from the setup wizard will be stored the same way in `garrison`.
- **Use:** an app's `main` environment gets each stored secret its stack file names under `secrets:`, decrypted by the deployer into a Swarm secret named by its version, and is redeployed when one changes. Other environments (`dev`, previews), which run code not yet merged, get generated values, as does every secret with no stored value.
- **Keys:** one age key per namespace, so a namespace's backup or export opens on its own, and a namespace is rotated or deleted without touching the others. The deployer makes a namespace's key the first time its public half is asked for. The keys live in `/etc/garrison/sops/` (root only), mounted only into the deployer: not Gitea, not the dashboard, not the agents' sandboxes, not the LLM.
- **Sealed to the machine:** where the machine has a TPM, the keys are sealed to it with `systemd-creds`, so a stolen disk or disk image decrypts nothing, while the machine still starts unattended. Without a TPM they are files readable by root only.
- **Recovery:** every secret is also encrypted for the installation's recovery key. The installer makes it, keeps only its public half (`/etc/garrison/sops/recovery.pub`) and shows the private half once, at the end of the first install, to be kept offline: with it and sops, any secret in a config repo can be decrypted should the machine be lost.
- **Rotation:** `garrison secrets rotate <namespace>` makes a new key and re-encrypts the namespace's files (`sops updatekeys`). Old commits stay encrypted with the old key, so after a suspected leak the values themselves are changed too.
- **Limit:** root on the machine can read everything, as it already can `/etc/garrison` and Docker. No scheme protects a secret from the machine that must use it; a secret worth more than the machine (a main wallet key) is not kept on it.

### Agents and secrets

An agent never holds a secret: anything in its sandbox or its context could be leaked by a page that tricks it. It uses one by name, through a tool the agent service carries out, and gets back the result, never the value.

- **Declared in its folder:** `agent.yaml` names the secrets the agent may use, what kind each is, and where it may go. The file is reviewed like a grant, and only the people its grants name can have it used.

```yaml
secrets:
  shop-login:
    kind: login                 # username and password for a web app
    hosts: [shop.example.com]   # the only place it is ever sent
  shop-api:
    kind: token                 # added to requests as a header
    hosts: [api.shop.example.com]
  wallet:
    kind: ethereum              # a hot wallet: little in it
    chain: 1
    to: [0xAbc...]              # the only addresses and contracts it pays or calls
    max_per_tx: 0.01 ETH
    max_per_day: 0.05 ETH
    confirm_above: 0.01 ETH     # above this a person approves in chat
```

- **Delivery:** the deployer decrypts an agent's declared secrets into Swarm secrets mounted only into the agent service.
- **Tokens:** `http_request(secret, url, ...)`, run by the agent service, adds the token only on requests to the secret's hosts.
- **Logins:** `browser_login(secret, url)` has the browser service fill in the login form and keep the session for that agent; the agent gets screenshots and page text.
- **Ethereum:** a signer in the agent service holds the key. `send_transaction` and `sign_message` are checked against the chain, the addresses and the limits; above `confirm_above` the transaction waits for a person's approval in chat. A main key stays on a hardware wallet or a remote signer that asks on the phone.
- **Feed:** every use is an event: who, which agent, which secret, which host or transaction.
- **What it cannot stop:** an agent logged in can do whatever the account can, if tricked. Agents get accounts of their own with the fewest rights, and wallets with little in them.
- **Order:** storage and the form, then tokens, then logins, then the signer.

## Agents

| Agent | Scope | Lifetime | Job |
| --- | --- | --- | --- |
| System agent | Root | Long-running | Platform repos, platform alerts, cross-namespace upgrades via PRs into each namespace |
| Namespace agent | One namespace | Long-running | Namespace config, members, stacks, namespace alerts |
| Coding agent | One repo | Per task, sandboxed | Builds the app with the user, pushes to a branch, replies in thread |

- Agents speak an OpenAI-compatible API (Ollama by default) and use the LLM key from setup. The agents run in Open WebUI with the gateway's MCP tools (see Chat).
- Coding agents start from an @mention in a Gitea comment (commit, PR, issue) via webhook, or from the team chat. Each task gets a fresh Docker container with the repo checked out, removed when done.
- An agent working in a namespace that finds a cause above its scope (e.g. a full disk) escalates a request to the operator working in garrison.
- Users can still work locally with their own coding tools and push to the same repos.
- Agents are folders in `garrison/config`, each working in every namespace (see Chat). Each chat runs in its own sandbox behind an OpenAI-compatible agent service. See [agents.md](agents.md).

## Dashboard

Served at the installation's domain, behind the sign-in. Built so far: the **feed**, Home's first page, one timeline of everything that happens and how it was handled: pushes, reviews and merges, pipeline runs, deploys and restores, what agents change for whom, routines' runs, and every refusal and failure, each person seeing what they may read; an entry opens its thread (its pull request, commits and runs). See [status.md](status.md).

Pages follow the levels: Home (cluster), Namespace, Stack, Service. Every page has the same tabs:

- **Overview:** status, hostnames, warnings (e.g. missing backup drive).
- **Logs and metrics:** one generic Grafana dashboard with template variables, embedded and filtered to the current level.

Dashboards are code: any repo can hold Grafana dashboards as `dashboards/*.json`. The deployer keeps them in a Grafana folder named after the namespace, as they are on `main`; edits made in Grafana are put back.
- **Config:** a form generated from the JSON Schema, plus a Monaco code editor. Save validates and commits. Includes the write-only secrets form.
- **History:** change records and snapshots, with restore and a warning listing what a restore will undo.

An inbox collects everything waiting for the user: PRs to approve, previews to review, failed deploys.

## Chat

- Open WebUI, at `chat.<domain>`, is the chat. People sign in through Keycloak; root admins are its admins.
- Each namespace has a channel there, named after it: its team chat, permanent, which only its people (and root admins) read; `garrison`'s, the platform's, everyone signed in reads. Outside a namespace, people talk to the agents in private chats. Only admins may change a channel, and the gateway puts back what it set. Mentioning an agent by its name, `@<agent>` (e.g. `@operator`), makes it answer in a thread, working in the channel's namespace; a pinned message in each channel lists them.
- The agents are folders under `agents/` in `garrison/config`, which only root admins change: `agent.yaml` (description, model, tools, skills) and `prompt.md`, with skills in the Agent Skills format (`SKILL.md`) under `skills/`. `garrison/config` starts with the operator (`tools: all`) and the commander, from `templates/garrison-config/`, compiled into the control plane; without an `agents/` folder it has the operator. Namespaces have no agents of their own (the approval check refuses an `agents/` folder there); every agent works in every namespace, as `<namespace>/<agent>`, with the rights (`grants.yaml`) and routines (`routines.yaml`) that namespace gives it. See [agents.md](agents.md).
- Open WebUI's one model connection is the agent service, an OpenAI-compatible API in the control plane whose models are the agents, `garrison/<agent>`, named `<agent>`; in a namespace's channel it runs the agent as it works there. Open WebUI sends it the person's Keycloak token and the chat's id; it has no LLM key and no tools itself. The agent service runs each turn: the LLM, whose key only it holds, with the agent's prompt and the platform's rules after it, calling the agent's tools at the gateway as the person. Tool calls show as the answer's reasoning. Open WebUI's own tasks (titles, tags) go straight to the LLM.
- Models: `llm/providers.yaml` in `garrison/config` lists the LLM providers, OpenAI-compatible APIs (e.g. Ollama's cloud, OpenRouter, a local Ollama), and the default model; `llm/models.yaml` lists every model they offer, each `enabled` or not and, once its provider stops offering it, `gone` with the date. Root admins change both by pull request; everyone signed in reads them, and agents with `list_models`. The install writes the first provider from its answers. The agent service asks each provider for its models once a day and soon after `providers.yaml` changes, and proposes the changed list as a pull request (on a branch `garrison/models-<time>`): new models come turned off, retired ones are marked, and the agents still naming them listed; a provider that cannot be reached keeps its models. An agent names its model as `<provider>/<model>` (a bare name is the default provider's), and the approval check refuses one that is not enabled. The agent service sends each request to the model's provider; an agent whose model is turned off or retired gets the default, and its namespace's feed says so once a day. Each provider's key is a root-only file on the machine, `/etc/garrison/llm-keys/<provider>` (the install's LLM keeps `/etc/garrison/llm-api-key`), read only by the agent service.
- The gateway serves an agent only the tools its `agent.yaml` lists (plus `load_skill` and `read_skill_file` if it has skills); a call that names no agent of a namespace gets none. It keeps an entry per agent in Open WebUI, for everyone, or only for the namespace's people if the agent works on a private repo, and one channel per namespace, for its people, adding and removing them as folders and organisations come and go. People can also chat with an agent privately.
- The `garrison/approval` check on config pull requests also checks the agent files: an agent with an unknown tool or skill, or without a prompt, fails it.
- The gateway is an MCP server. The agent service calls it with the signed-in person's Keycloak access token, so every tool acts with that person's rights: reads and proposals go to Gitea as them. Neither is reachable from outside.
- Agents can list namespaces and stacks, show a stack's tasks and errors, read service logs, show CPU and memory use, run Prometheus queries, read repos, list open changes, propose a change to a file, and take a screenshot of a page. As the person, they make namespaces (root admins only, as Gitea allows) and empty repos (a namespace's admins); everything in a repo still comes as a pull request. Every agent's system prompt says how the home cloud works: namespaces, `garrison` only for the platform, apps as files in `config/stacks/`, secrets made by the deployer.
- `screenshot` loads a page of the installation in the control plane's headless Chrome, signed in as the person: to private environments and the dashboard the browser sends their Keycloak token, which the sign-in in front of them accepts (Open WebUI's tokens name `sso` as an audience), and Traefik removes it before the request reaches the app. Other pages, which have no sign-in in front to remove it, get no token. The browser's only network reaches Traefik and the gateway, every name it looks up goes to Traefik, and the gateway refuses its requests to other names. Each screenshot gets a fresh browser context. The PNG becomes a pull request, as with `propose_change`. A routine, which has no person, signs in with the agent's own account in Keycloak (client `agents`, whose tokens name `sso` too) and proposes through a grant with `unattended: true`; see [agents.md](agents.md).
- `propose_change` opens a pull request as the person. It cannot merge or deploy: a person allowed to merges in Gitea, and the deployer deploys `main`. The agent always gives the link.
- Agents with grants have accounts of their own: they propose for people who may not write themselves, and merge with `merge_change` where a grant allows it for the person asking. Agents ask each other with `ask_agent`; `garrison/commander` finds the agent for a task and hands it over or names it. Agents run routines with nobody watching, and use pages with `browse`; pull requests of an app's source repo get a preview each if its stack has a `preview` environment. See [agents.md](agents.md).
- Not yet: conversational setup on a draft branch.

## Notifications

- **Sources:** Alertmanager (disk, node, backup, service health), deployer (failed deploys, PRs waiting), agents (task updates), chat (@mentions). Built: Alertmanager's alerts go to the dashboard's feed by namespace and start the agents' `on_alert` routines; see [status.md](status.md).
- **Routing:** by namespace to its members, its channel and the agents its routines name; platform alerts to root admins and garrison's routines.
- **Delivery:** in-app inbox, plus phone push through ntfy.
- **Questions from agents:** a routine that needs something only its person can tell it (a choice, a missing fact, an approval) calls `ask_person`. The question is posted in the namespace's channel as a mention of its owner, which Open WebUI tells them of even in a muted channel, and is listed under **Waiting for you** at the top of their dashboard, with a link that opens the question's thread. The run ends its turn and waits, up to 24 hours: the agent service watches the question's thread, and the owner's first reply there is the answer. The run then goes on in the same sandbox, with its workspace and history, and posts what it did in the thread; it may ask again there. Without an answer it stops waiting and says so in the thread. Questions are kept in Postgres (`questions`), so a restart does not lose them, and the feed records each one, waiting, answered or not. Only a routine's own agent asks this way: in a chat the person reads the answer, and an agent another one asked puts its question in its answer. Not yet: phone push (ntfy).
- People are notified immediately; agents investigate in parallel with read-only rights and follow up with a cause and a proposed fix for a person to approve.

## Storage and backup

- ZFS on the main drive, one dataset per stack volume, shared libraries as separate datasets.
- Sanoid takes scheduled snapshots; the deployer snapshots before every deploy.
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

6. **GPU on real hardware:** the VMs have no GPU, so the GPU part of acceptance item 4 is tested on a physical machine. A spike before the deployer is built confirms that a Swarm service gets the GPU through a generic resource with the NVIDIA runtime as default.

The MVP acceptance list above becomes the end-to-end test suite.

## Open questions

- Namecheap DNS challenge with a changing home IP.
- Build the agent loop in-house or wrap existing agents.
- Whether agents may provision secrets for sub-agents.
- Whether the browser service may keep an agent's login session across turns, or logs in each time.
- When to move from gVisor sandboxes to OpenShell.
