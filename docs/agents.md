# Agents — Design

How agents are defined, run and updated. Status: steps 1 to 3 of the plan are built, and the proxies of step 4: agent folders and their checks; the agent service, which Open WebUI talks to; and a sandbox per chat, Pi in a container with its workspace in a volume, which gets the LLM and the agent's platform tools through the agent service. The gateway serves each agent only its own tools and skills. Agents can clone the repos the person may read into their workspace and propose their changes as pull requests by the person. Steps 7 to 10 are built too: agents' own accounts and grants, private agents for private repos, agents asking agents, routines that run without a person, previews of pull requests and browsing. Still design: datasets instead of volumes, clean-up of old chats, Pi's native skills. See Chat in [spec.md](spec.md).

## Goals

1. Agents are files in Git: a prompt, a model, tools and skills, changed by reviewed pull requests like `members.yaml`.
2. Garrison has several agents, e.g. an operator and a marketing agent, defined once in `garrison/config` and working in every namespace; a namespace gives them rights and routines there.
3. Every chat with an agent is a real agent with its own workspace: files, a shell, a browser, its own history, in a sandbox.
4. Open WebUI stays the chat; it talks to agents as if they were models.
5. An agent has the rights of the person talking to it, and holds no secrets. It has more only where its folder grants them, reviewed like a role change, and only for the people of its namespace the grant names.
6. Agents can ask each other for work, without anyone gaining rights by the detour.

## Overview

```
Open WebUI ──/v1/chat/completions──▶ agent service (control plane)
  model "todo/marketing"              │ one container per chat
  + chat id, signed user              ▼
                                ┌─────────────────────────────┐
                                │ sandbox: Pi (agent harness) │
                                │ workspace + session history │
                                │ (dataset per chat)          │
                                └──────┬───────────────┬──────┘
                                   LLM proxy      gateway tools
                                (key stays out)  (as the person)
```

| Part | Where | Job |
| --- | --- | --- |
| Agent folders | `garrison/config/agents/<name>/` | What an agent is: prompt, model, tools, skills; the same agent in every namespace |
| Grants and routines | `<namespace>/config/grants.yaml`, `routines.yaml` | What a namespace lets the agents do there beyond the person's rights, and what they do there by themselves |
| Agent service | Control plane, service `agents` | OpenAI-compatible endpoint for Open WebUI; starts, routes to and stops sandboxes; proxies the LLM and the tools |
| Sandbox image | `sandbox/` in the control plane's repo, built with it by Gitea Actions and pushed as `garrison/sandbox` | The harness (Pi) and what agents work with: bash, git, ripgrep, curl, jq, ffmpeg, and Chromium behind the command `web` (`sandbox/web.mjs`, Playwright through the sandbox's proxy, no sign-in) |
| Gateway | Control plane, service `gateway` | The tools, as today, acting as the person |

## Agent folders

The agents are Garrison's: all of them are folders in `garrison/config`, which only root admins change. Namespaces have no agents of their own; each of garrison's agents works in every namespace. Mentioned in a namespace's channel, it works in that namespace: there it is an instance, `<namespace>/<agent>` (e.g. `todo/marketing`), with the rights and routines that namespace gives it in its own config repo (see Accounts and grants, and Runs without a person). In a private chat, and in `garrison`'s channel, it is garrison's instance and works in whichever namespace the person names.

```
garrison/config/
  skills/                       # shared by the agents
    homepage-screenshots/
      SKILL.md
      viewports.yaml
  agents/
    commander/
    operator/
      agent.yaml
      prompt.md
    marketing/
      agent.yaml
      prompt.md
      skills/                   # this agent's own skills
        youtube-thumbnail/
          SKILL.md
          template.svg
  grants.yaml                   # garrison's own, as in any namespace
  routines.yaml

todo/config/
  members.yaml
  stacks/
  grants.yaml                   # rights of garrison's agents in todo
  routines.yaml                 # what they do in todo by themselves
```

`agent.yaml`:

```yaml
description: Makes screenshots and media and keeps the homepage current
model: openrouter/anthropic/claude-sonnet-5.5   # an enabled model of garrison/config llm/models.yaml; empty for the default
tools:                       # gateway tools this agent may use; `all` for every one, also later ones
  - list_repos
  - read_file
  - propose_change
  - screenshot
skills: [homepage-screenshots]   # shared skills; its own skills/ are always included
visible: members             # optional: only the people of the namespace it works in
agents: [tester]             # agents it may ask for work, by name (see Agents asking agents); or any
internet: [packages, api.example.com]   # what its sandbox reaches; none without it
```

- `prompt.md` is the system prompt. The platform appends fixed rules after it (act as the person, propose and never claim a change is merged, give the link); a prompt can add rules but not remove these. Before it, the agent is told where it works: the namespace of the channel, or none in particular.
- People mention an agent by its name, `@marketing`, the same in every channel. Its model in Open WebUI is `garrison/<agent>`; the agent service runs the instance of the channel's namespace. Open WebUI lists only garrison's agents.
- `garrison/config` is written at install from `templates/garrison-config/`: the operator (the default model, `tools: all`, and its skill `build`, which turns a wish such as "a cron job every day at 9" into garrison's parts: namespace, repo, stack, grant, routine) and the commander, and agents with one role each and only the tools it needs: `coder`, `sentry`, `scout` and `media` (see the README of `agents/` there). The operator asks once for what only the person can say, such as a name or a time, and makes nothing until they answer. Without an `agents/` folder, garrison has the operator. A new namespace's config gets `members.yaml` and a README, no agents.
- The `garrison/approval` check on `garrison/config`'s pull requests checks agent files: known tools and skills, an enabled model (see Models in [spec.md](spec.md)), a prompt that is not empty; root admins approve them. On a namespace's `config`, it checks `grants.yaml` and `routines.yaml` (each entry names one of garrison's agents), and refuses an `agents/` folder: agents are garrison's only. `repos`, `grants` and `routines` in an `agent.yaml` are refused too: those are a namespace's to give.
- `internet`: what the agent's sandbox reaches, through the agent service's proxy (see Sandbox). `packages` opens the package registries: Alpine, Debian, Ubuntu, PyPI, npm, Go modules and Rust crates, download hosts only, no GitHub. `all` opens every public address. A list names `packages` and domains, each with its subdomains. Without it, nothing. Where an agent works on a private repo (a grant or a routine on one), or is `visible: members`, it reaches the registries at most: the approval check refuses a namespace's grant on a private repo to an agent that reaches more, and should a repo turn private later, the proxy holds it to the registries, if it had them.

## Accounts and grants

By default an agent has no rights of its own: every tool call acts as the person whose message started the turn, and changes leave only as pull requests by that person. A namespace can give garrison's agents more there, per repo and branch, with the autonomy levels of [spec.md](spec.md) (Rights and autonomy), in `grants.yaml` of its config. Built: `control-plane/internal/agents/grants.go` (the file format and its checks), `internal/gateway/grants.go` (the decisions), `internal/gateway/members.go` (the accounts).

### Private repos and who sees an agent

Rights are per namespace: its people (`members.yaml`) change its repos, everyone signed in reads the public ones, and only its people read the private ones. There are no people of single repos. An agent, as it works in a namespace, follows the repos it works on there (those of its grants, and those its routines watch):

- **All public, or none** (like the operator in most namespaces): everyone may talk to it. It acts with each person's rights, so someone outside the namespace gets answers but no changes: proposing fails, as they may not write, unless a grant lets the agent propose for everyone.
- **One is private**: the agent, as it works in that namespace, is private. It is only for the namespace's people and root admins, and answers only where only they read: a private chat, or the namespace's channel. Elsewhere, e.g. handed work in `garrison`'s channel, which everyone reads, the agent service refuses it with a note saying where to ask. A repo that does not exist yet counts as private. The same agent elsewhere is not affected.
- `visible: members` makes an agent private wherever it works: only the people of the namespace it works in use it there, and in Open WebUI's list only root admins see it.

Everyone signed in sees garrison's agents in Open WebUI. The gateway keeps an Open WebUI group per namespace, `garrison <namespace>`, with the people of its `members.yaml` who have signed in to Open WebUI (matched by their email in Gitea); the namespace's channel is given to that group; root admins are Open WebUI's admins and see everything. The agent service and the gateway check it again on every request, since Open WebUI is not the only way in: someone who may not use the agent gets a refusal that says why.

### The agent's account

Each agent with grants or routines in a namespace gets an account in Gitea for its work there, `agent-<namespace>-<name>` (e.g. `agent-todo-dev`; a hash where that is longer than Gitea allows), with the full name `Agent <namespace>/<name>`. The gateway makes it, gives it write access to the repos its grants name and to no other repo of the namespace, and locks it, with that access taken away, when the agent is gone, switched off or without grants there. Nobody signs in with it and no token of it exists: the gateway acts as it through Gitea's Sudo, as it acts as people.

The same account is in Keycloak, in the realm `garrison`, so the agent can sign in where a person would: in a routine, with nobody watching, its browser (`screenshot`, `browse`) shows the apps as the agent, also private environments. The gateway makes it with the Gitea account (email `<account>@agents.invalid`, no groups) and disables it when it locks that. It gets the account's tokens through the Keycloak client `agents`, which it sets up itself: password grants only, its tokens naming `sso`, so the sign-in in front of private environments takes them; the gateway's own MCP endpoint does not. The account's password and the client's secret are derived from the admin password, which the gateway holds anyway, so there is no further secret; a password someone changes in Keycloak is set back at the next sign-in.

### Grants

`grants.yaml` in `todo/config`:

```yaml
- agent: dev              # one of garrison's agents
  repo: todo
  branch: dev
  level: autonomous       # merge and deploy
  for: members
- agent: dev
  repo: todo
  branch: main
  level: autonomous
  for: admins
  checks: [ci]            # commit statuses that must be green before it merges
- agent: desk
  repo: todo
  level: propose          # outsiders' proposals, see below
  for: everyone
  unattended: true        # routines may use it too
```

| Level | The agent may, with its own account |
| --- | --- |
| `propose` | Write its own branches, `agent/<name>/...`, and open pull requests from them |
| `merge` | Also merge proposals into `branch` once the checks pass |
| `autonomous` | The same on a branch that deploys: the merge deploys as any merge to `main` or `dev` does |

- **A change to a namespace's repo uses that namespace's grants**, wherever the person asked: in its channel, another one, or a private chat. A run without a person keeps to the grants of its own namespace.
- **`for` says whose requests may use a grant**, and only names groups of the namespace that gives it: `members` (the default, admins included) or `admins`. `everyone` is allowed for `propose` only, and not on a private repo. Anything else fails the approval check.
- **The agent's account acts only for people who may read the repo.** Before it proposes or merges for someone, the gateway asks Gitea whether they may read it; nobody changes, or learns about, a repo they cannot see through an agent. Without `for`, a member could get through the agent what they may not do themselves, e.g. merge to `main`. Root admins count as admins everywhere.
- **Each action is decided at the gateway, in this order:** if the person may do it themselves, it is done as the person, as before. Otherwise, if a grant of the repo's namespace covers the agent, repo, branch and level and the person is in its `for`, it is done with the agent's account there. Otherwise it is refused, and the agent is told which grant would be needed.
- **Gitea limits the account too:** it may write only to its granted repos. Which branches it may merge into is the gateway's decision, as app repos take pushes from their members and have no branch protection.
- **`merge` waits for the deployer.** Until the deployer can hold a deploy for a person's approval (not built), every merge to `main` or `dev` deploys, so the approval check refuses `merge` on those branches and asks for `autonomous`, which says what happens.
- **Pull requests name whom they were for.** The gateway, not the model, writes *Asked by bob through todo/dev.* into each pull request an agent opens, and *Changes added, asked by alice ...* as a comment on each addition; merges carry the same line. Runs without a person say so: *Asked by bob through todo/dev, routine todo/dev/nightly, with nobody watching.* The gateway logs every call with the person, the agent and what started the run.
- **Someone else's work needs someone else's approval.** A shared channel is one workspace, so one person's turn may carry another's work: bob edits, alice says "merge it". The agent merges a pull request only if everyone it was asked for (its author, if a person, and every *Asked by*) is the person asking now, or if someone who did not ask for it approved it in Gitea. So people can put their own work live through an agent, and never another's unreviewed.

### New namespaces and repos

`create_namespace` and `create_repo` act as the person: Gitea lets only root admins make namespaces, and a namespace's admins and root admins make its repos. A new namespace gets its config and its channel from the gateway within a minute, the person as its first admin; the agents work in it at once. A new repo is empty but for a README, and only for code of an app's own; its files come as a pull request like any other change. Every agent's system prompt (`agents.Platform`) says how Garrison is laid out, with a full `config/stacks/<name>.yaml` that a test checks the deployer takes, so a request for a new app becomes a namespace if needed and a proposed stack in its config, never a stack in `garrison` or a secret made by hand.

### Proposing and merging

- **Propose:** `propose_patch` (`propose_workspace_changes` in a sandbox) as before. Where the person may not write to the repo but a grant lets the agent propose there for them, Gitea applies the patch as the agent's account, on a branch `agent/<name>/<person>-<time>`. The Git proxy stays read-only: changes leave the sandbox only as patches, and granted branches are reached only by merging a pull request, so every change keeps its diff, its checks and an easy revert. `propose_workspace_changes` takes a `base`, e.g. `dev`.
- **Merge:** a tool `merge_change(namespace, repo, number)`, only for agents (never on the plain `/mcp` endpoint), and only of proposals (branches `change/...` and `agent/<name>/...`). The gateway checks the grant, `for`, the checks and the approval rule above, then has Gitea merge as the agent's account.
- **Deploy and roll back:** no tool of their own. A merge deploys through the pipeline, as for people. Rolling back is a revert, merged the same way.
- **Keeping proposals current:** when the base branch of an open proposal moves, the gateway has Gitea merge the base into it, as the pull request's author. Where they conflict, it says so once in the pull request, for a person or the agent to resolve. `config`'s proposals are left alone: a new head would undo their approval.

### Outsiders

With `propose` for `everyone`, someone who is not in the namespace can ask the agent for a change, e.g. in a private chat. The agent's account in that namespace writes the branch, since the person cannot, and the pull request says *Asked by bob (not in todo) through todo/desk.* The namespace's people review and merge it as any pull request. Without that grant, an outsider gets the agent with their own rights: they read what they may read, and proposing fails with the reason.

### What no grant can lift

The gateway refuses these whatever an agent folder or `grants.yaml` says, and the approval check refuses grants that would need them:

- Writing to a `config` repo, so an agent never widens its own grants, and so changing `members.yaml`.
- Changing branch protection, deleting repos, or anything in Gitea's or Open WebUI's settings: there is no tool for it, and the agent's account has no admin rights.
- Grants on repos of another namespace: each namespace grants only on its own. Grants in `garrison` need a root admin's approval, as every change to `garrison/config` does.
- `enabled: false` in `agent.yaml` switches an agent off everywhere: at the next sync, seconds after the merge, it leaves Open WebUI and the gateway's catalogue, its tool calls are refused and its accounts are locked; the approval check then refuses grants and routines naming it. A turn already running ends at its next tool call.

## Agents asking agents

A tool, `ask_agent(agent, task, limits, done_when, background)`, served by the agent service to agents with an `agents` list, hands work to another agent: what to do, what bounds it, and what a good answer looks like, as fields, not a paragraph the other agent has to guess from.

- **Whom:** only the agents in the asker's `agents` list, by name, and only those the person may use themselves (`visible`). The approval check checks that they exist. The asked agent works where the asker does: `tester`, asked by `todo/dev`, is `todo/tester`; `<namespace>/<agent>` names it as it works in another namespace.
- **Rights:** the asked agent runs for the same person, with its own tools and the grants of where it works. A grant is always checked against the person who started the chain, never against the asking agent, so no detour adds rights: bob asking `todo/dev` to ask `lab/dev` is bob asking `lab/dev`. The gateway logs the asked agent's calls as started by `agent todo/dev`.
- **Where:** in its own sandbox for the same chat (key: the chat and the asked agent), with its own workspace. The two share work through Git, e.g. a proposal's branch or its preview.
- **Limits:** at most three agents in one run, and never one twice, so A asking B asking A ends at once. The agent service keeps the chain in the turn's state, not in anything the model writes.
- **Waiting:** the asker waits for the answer, which counts against its turn's time limit; the asked agent's tool calls count against its own cap. If the asked agent is already working in this chat, the call waits for its turn. Its answer comes back as the tool result, word for word, and its progress shows in the reasoning, each line under its name.
- **Private agents** are asked only where they may answer: an agent in `garrison`'s channel cannot hand work to one that is private in `todo`; the refusal is logged.
- **Finding them:** `find_agents(in_namespace)`, for the same agents, lists those it may ask for this person, as they work where the asker does (or in `in_namespace`): what each does (`description`), its tools, skills and grants there, and which namespace the channel belongs to. Agents it may not ask, the person may not use, or that may not answer here are left out.
- **Anyone:** `agents: any` lets an agent ask every agent the person may use. Root admins approve it, as every agent; it adds no rights, as the asked agent still acts as the person.
- **The feed** has each hand-off, for the person and the asked agent's namespace, with its answer; the asked agent's calls are threaded under it.
- **Background:** in a channel, `background: true` returns at once; the asked agent works on and its answer is posted in the channel when done. It still acts with the person's Keycloak token, which lasts five minutes, so a long background run fails at its later tool calls.

## The commander

The commander is for when someone does not know which agent to ask. It is written into `garrison/config` at install (`agents: any`, no tools), is the default model of new chats in Open WebUI (`garrison/commander`), and every channel's pinned message names it. Like every agent, it goes by its name, `@commander`, in any channel, and works in the channel's namespace. It does no work itself: it calls `find_agents`, then

- hands a clear, one-off task (a screenshot, a new repo, why an app does not start) to the agent that fits, with `ask_agent`, working in the same namespace (or the one named, as `<namespace>/<agent>`); in a channel in the background, so that agent posts its answer there itself;
- for work with back and forth (a feature, a new app), tells the person which agent to talk to, `@<agent>` in the channel of the namespace it is about, and why;
- asks one question when it cannot tell, and says when no agent fits and what one would need.

Root admins change it like any agent of `garrison`, by a pull request to its folder.

## Runs without a person

Routines: work one of garrison's agents does in a namespace by itself, in `routines.yaml` of the namespace's config. A routine is a one-shot task: the agent service starts the agent, as it works in that namespace, and gives it the task as its prompt.

```yaml
- agent: dev                           # one of garrison's agents
  name: nightly
  every: 24h                           # 10m to 7 days
  owner: bob                           # a person of the namespace
  task: Update the dependencies of todo and propose it.
- agent: operator
  name: morning
  schedule: "30 6 * * mon-fri"         # cron, in Garrison's time zone
  owner: alice
  task: Sum up what happened in todo yesterday.
- agent: dev
  name: main-moved
  on_push: {repo: todo, branch: main}  # when the branch moves
  owner: bob
  task: Check what changed and say whether it needs a follow-up.
- agent: operator
  name: on-failure
  on_alert: {alertname: ServiceFailing}  # when one of this namespace's alerts fires
  owner: bob
  task: Find out why it fails and say what to change.
```

- **Asking for one:** people ask the operator in words ("every morning at 9, take a screenshot of todo"); it proposes the routine with `add_routine`, which writes the YAML, keeps the routines there and adds the grant a run needs. Someone allowed approves it, on its card or in Gitea.
- **Who:** a routine acts with the agent's own account in the namespace, for its owner, who must be in the namespace when it runs (else it posts that it did not run). It uses only grants that say `unattended: true` and have the owner in their `for`; otherwise it reads and reports. Its browser signs in with the agent's account in Keycloak, so `screenshot` and `browse` work, and a screenshot is proposed with the agent's account through such a grant. `propose_change`, `create_namespace` and `create_repo` need a person and are refused.
- **Asking:** a routine that needs a choice, a fact or an approval from its owner calls `ask_person`; it ends its turn and goes on, in its sandbox, when the owner replies in the question's thread (see Notifications in [spec.md](spec.md)).
- **When:** each routine has one of `schedule`, `every`, `on_push` and `on_alert`. The agent service looks every minute.
  - `on_alert` lists label values an alert of this namespace must have, e.g. `{alertname: ServiceFailing}` or `{alertname: ServiceDown, app: todo}`; `{}` takes every alert of the namespace. The platform's own alerts belong to `garrison`. The routine starts once per firing, and its task is told the alert's summary, labels and description. If it is still running from an earlier one, Alertmanager's next reminder (every 4 hours) starts it.
  - `schedule` is cron's five fields, minute hour day-of-month month day-of-week (names like `mon-fri` and `dec` work), or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`. It is read in Garrison's time zone, `TIMEZONE` in `install.env` (the machine's at install, else UTC). A time a clock change skips runs right after the change. The approval check refuses a schedule that never comes or runs more often than every 10 minutes.
  - A routine with `schedule` first runs at the next time it names after it is first seen; one with `every` one interval after; one with `on_push` when the branch moves after that.
  - When each last ran, and the commit each `on_push` routine last saw, are kept in Postgres (`routine_state` in `garrison`). After a restart, a schedule missed meanwhile runs once, and a push made meanwhile starts its routine. Until the agent service can read that state, no routine runs.
  - One run of a routine at a time.
- **In the feed:** each run is in the dashboard's feed with its answer, for the namespace's people and the owner, and its thread shows what it did.
- **Where the answer goes:** posted in the namespace's channel, which only its people read, under the agent's name, by the gateway's Open WebUI account.
- **How the gateway knows:** the agent service and the gateway derive a key from the admin password, which both hold. With it, the agent service tells the gateway what started each run (`person`, `agent <id>`, `routine <id>/<name>`) and, for a routine, its owner instead of a person's token. The gateway logs such calls as `bob (nobody watching, as agent-todo-dev)`.

## Previews and browsing

- **Previews:** a stack with an environment `preview` gets one per open pull request of its source repo, from a branch of the repo itself: `pr-<n>`, at `pr-<n>-<stack>.<domain>`, deployed like `preview` from the pull request's branch once its pipeline has built the image, and taken down when the pull request closes. At most the three newest pull requests of a repo have one. Branches named `pr-<number>` cannot be environments.
- **`browse`:** an agent uses a page as the person would, in the gateway's Chrome, signed in as them: open a page, click an element, type into a field (and press Enter), each call answered with the page's text and its links, buttons and fields, numbered. Each person and agent has a tab of its own, in a browser context of its own, kept for five minutes between calls. Like `screenshot`, it loads only the installation's own names.
- Together: an agent proposes a change, waits for its preview, and tries it with `browse`, or asks a tester agent to.

## Skills

Skills use the Agent Skills format: a folder with `SKILL.md` (frontmatter `name` and `description`, then instructions) and any files it needs. Pi loads them natively.

- An agent sees each of its skills' name and description in its prompt, and reads a skill's instructions and files only when a task needs it.
- Tools are abilities, enforced by the gateway. Skills are know-how: how to use the tools for a job. A skill grants nothing; a call to a tool the agent lacks is refused.

## Agent service

An OpenAI-compatible API, reachable only from Open WebUI on the system network.

- `GET /v1/models` lists every agent folder of `garrison/config` as a model, `garrison/<agent>`, named `<agent>`. A chat in a namespace's channel runs the agent's instance there, `<namespace>/<agent>`. Open WebUI shows them in its model picker; the gateway no longer syncs model presets.
- `POST /v1/chat/completions` streams the agent's answer. Tool calls and progress show as reasoning, so people see what the agent does.
- Open WebUI's connection sends, as custom headers, `X-Garrison-Chat: {{CHAT_ID}}` and `X-Garrison-Task: {{TASK}}`, and the person's Keycloak token (`system_oauth`), which the agent service verifies.
- Requests with a task header (titles, tags, follow-up suggestions) go straight to the LLM without a sandbox.
- Open WebUI sends the whole conversation each time. The agent service passes on only the new message, and Open WebUI's own system messages (a channel thread's history): the harness keeps its own session, including tool calls and results. A fresh session, e.g. of a chat begun before sandboxes, gets the earlier messages as context. Branching the session when a message is edited or regenerated is to come.
- The answer streams as Pi writes it: its thinking, tool calls and failed tool results as reasoning, its text as content, piece by piece. Text before a tool call stays in the answer as a step ("Let me look at the logs."), as only a message's end says whether it was the answer. For an agent asked by another, only its last message is the answer; earlier text shows as progress.

## Sessions

A container belongs to a chat, not to an agent or a person.

| Situation | Containers |
| --- | --- |
| Alice and Bob each chat with `todo/marketing` | 2 |
| Alice opens a second chat with it | 3 |
| Alice returns to her first chat the next day | Hers starts again where it left off |

- **Workspaces are separate.** Each chat has its own volume (built: a Docker volume `garrison-agent-<hash>`; planned: a ZFS dataset, for snapshots and quotas) holding the workspace and the session, so drafts of one chat never show in another. Agents share work through Git (branches and pull requests) and the namespace's media library, like people do: the share `media`, mounted at `/data/media` in the sandboxes of the namespace's channel and routines once a stack mounts it, never in private chats or another namespace's. The sandbox guard binds that one host folder, `/garrison/ns/<ns>/shares/media`, and only for the namespace the container is labelled with.
- **Rights are separate.** Every tool call acts as the person whose message started the turn, or with the agent's account where only a grant allows it (Accounts and grants).
- **Team channels:** a namespace channel is one conversation shared by its members. Open WebUI names the channel (`channel:<id>`), not the thread, in its requests, so a channel gets one container per agent for all its threads; each thread's history still comes along with each message. Each turn acts as the person who mentioned the agent in that message. The workspace is visible to everyone in the thread; private work belongs in a private chat.
- **One turn at a time per chat;** a message sent while the agent works waits for the turn to end.

## Sandbox

- One container per chat and agent from the sandbox image, its main process `pi --mode rpc`: the agent service attaches to its stdin and stdout, sends commands and reads events, so the sandbox opens no port. Built.
- Pi gets the agent's prompt with the platform's rules and a note on its sandbox; its own tools (read, write, edit, bash) work in `/data/work`; a Pi extension (`sandbox/garrison.ts`) adds the agent's platform tools.
- Mounted: the chat's volume at `/data` (workspace and Pi's session). Skills are still read through the gateway's `load_skill`; mounting the agent's folder and skills for Pi to load natively is to come.
- Git: the agent service passes fetches (never pushes) on to Gitea with its admin account, after asking Gitea, as the person, whether they may read the repo; Gitea's Git protocol knows no Sudo. Changes leave only as patches through `propose_patch`, which Gitea applies as the person, so branch protection and authorship hold.
- Network: `garrison-sandboxes`, internal, shared only with the agent service. On a second port (8071) the agent service passes on the LLM, adding the API key and enforcing the agent's model, and the gateway's tools, adding the Keycloak token of the person whose turn runs. A sandbox names itself with a random token given at its start, which counts only during a turn. The sandbox holds no key, no person's token and no Docker access.
- Runtime: gVisor (`runsc`, installed by `install.sh`). A sandbox's processes run as root, with Docker's default capabilities, against gVisor's kernel in user space, which in turn uses few system calls of the host's; code that breaks out of the container meets gVisor before Linux. Root lets agents install what a task needs (`apk add`, `pip install`, `npm install -g`); it lasts until the container stops, and only `/data` is kept. gVisor has no Docker DNS, so the agent service gives sandboxes its own address on the network rather than its name.
- Internet: on a third port (8072) the agent service is an HTTP proxy, set in the sandbox's `HTTP_PROXY` and `HTTPS_PROXY` with its token, which curl, apk, pip, npm and git follow. It lets a sandbox in a turn reach what its agent's `internet` allows, on ports 80 and 443, and never an address that is not public (the home network, the platform's services, loopback, link-local, shared CGNAT space), checked on the address it connects to, so a name that resolves into the home network is refused too. Each request is logged with the person and the agent. A sandbox has no other way out: its network is internal.
- Private repos: an agent that reaches more than the registries clones only public repos through the Git proxy, so a page that tricks it cannot get it to send a private repo out.
- Limits per container: 1 GB of memory, one CPU, 512 processes. Containers are started through the sandbox guard, like the user runner's, which allows only the sandbox image, under gVisor, with the label `garrison.sandbox`, these limits, that network and volumes named `garrison-agent-*`. It knows its containers by the label, so it still does after a restart.

## Lifecycle and updates

- **Start:** on the first message of a chat, or the next message after a stop. A new chat's first answer takes a few seconds more.
- **Stop:** after 15 minutes without a message. The volume stays. When the agent service restarts, it removes all sandbox containers; their next message starts them again.
- **Clean-up:** Open WebUI does not report deleted chats; datasets of chats unused for 30 days are removed, after a snapshot.
- **Updates:** an agent is built from three layers, each changed in its own place.

| Layer | Lives in | A change reaches running chats |
| --- | --- | --- |
| Sandbox image | `garrison/sandbox` | At their next turn |
| Agent definition | `garrison/config/agents/`; grants and routines in `<namespace>/config` | At their next turn |
| Tools and platform rules | Gateway, in the control plane | When the gateway is deployed |

Before each turn the agent service compares the container's image and agent commit with the current ones and, if either changed, restarts the container between turns; the session resumes from the dataset. A running turn is never interrupted.

A new namespace or repo starts nothing: the agents work in it at once, and containers start only when someone chats.

## Limits

- Per person: at most 3 running sandboxes; a new chat stops that person's oldest idle one, or is refused while all three work.
- Machine: a cap from free memory; idle sandboxes are stopped first.
- Per turn: a time limit (10 minutes) and a cap on tool calls (60 in a sandbox), after which the turn is aborted.

## Observability

- The Grafana dashboard **Agents** (`garrison/grafana/dashboards/agents.json`), built: running sandboxes, by agent and by person, their memory and CPU, and a table of each with its chat; from cAdvisor, which passes on the sandboxes' labels `garrison.agent`, `garrison.person` and `garrison.chat`. Below, what agents did: the agent service's log (turns with person, agent, chat and number of tool calls; Git fetches; agents asking agents; routines) and the gateway's (every tool call with person, agent, tool and what started the run, refusals with why).
- What was said stays out of it: a sandbox's output is Pi's conversation, with whatever the agent read, private repos included, so Alloy does not ship sandboxes' logs to Loki, which everyone signed in reads.
- To come: a `list_sessions` tool that shows the same to the operator agents; whether a sandbox is working or idle.

## Plan

1. Agent folders: parsing, checks on `config` pull requests, `garrison/config` with the operator and the commander, every agent working in every namespace with the namespace's grants and routines. Built. The gateway serves an agent its tools and skills (`load_skill`, `read_skill_file`) when a call names it in `X-Garrison-Agent`.
2. Agent service: models list, streaming, chat id, the person's Keycloak token (Open WebUI's `system_oauth`, so no forwarded-user JWT is needed), task requests to the LLM. Built with the loop in its own process: the LLM with the agent's prompt, and its tools from the gateway as the person; tool calls show as reasoning. Open WebUI has no LLM key and no tools any more.
3. Sandbox image with Pi; one container per chat with its volume, idle stop and resume. Built; requests without a chat id still run in the agent service's process. To come: a dataset per chat, removing unused ones.
4. LLM and tool proxies; per-agent tool lists enforced; skills. Proxies built, with the per-agent tool lists of step 1. Git built: the agent service serves Git's fetch protocol to sandboxes (`$GARRISON_GIT/<namespace>/<repo>.git`) for the repos the person may read, and the gateway's `propose_patch` applies a patch as the person on a `change/...` branch with a pull request; in Pi it is `propose_workspace_changes`, which proposes a checkout's changes and adds later ones to the same pull request. Several agents work on one repo like people do: each on its own branch, merged by a person. To come: Pi's native skills, Git LFS.
5. Move the operator agents over; remove the model preset sync.
6. Marketing agent: `screenshot` tool, built: headless Chrome in the control plane, signed in as the person, the PNG proposed as a pull request; tested with a marketing agent that bob asks for a screenshot of the todo app. The media library, built: `/data/media`, browsed in `stacks/files.yaml` (`garrison/files`). `generate_media`, built: the agent service reads a project's `styles/<style>.yaml` and its ComfyUI workflow as the person, fills in the nodes the style names (prompt, negative, seed, size, frames), runs it on the installation's ComfyUI (`COMFYUI_URL` in `install.env`, outside Garrison, one run at a time) and hands what it made to the turn's sandbox, which writes each file with a `.json` of how it was made into `/data/media/<project>/`. The agent `media` (with the skill `media-project`; shorts with ffmpeg in the sandbox). To come: Git LFS for larger media.
7. Accounts and grants, built: private agents for private repos, kept as Open WebUI groups and checked by the agent service and the gateway; an account per agent with grants or routines, writing only to its granted repos; grants checked on `config` pull requests, with the limits no grant lifts; proposals by the agent's account on `agent/<name>/...`; `merge_change`; outsiders' proposals. Tested in scenarios 4g, 4h, 5b and 5c.
8. `ask_agent`, built: waiting for the answer, or, in a channel, posting it later. Tested in 4i.
9. Runs without a person, built: routines (`schedule`, `every`, `on_push`, kept across restarts), posting into the namespace's channel, the record of what started a run; keeping proposals current when their base moves. Tested in 4j.
10. Previews of pull requests and `browse`, built. Tested in 4i.

## Open questions

- Branching Pi's session when a message is edited or regenerated in Open WebUI: today the newest message is simply sent on.
- Thread ids for channels, so each thread gets its own sandbox.
- GPU or a local LLM for agents that run many turns.
- Posts from routines and background answers come from the gateway's account with the agent's name in bold; Open WebUI has no way to post as a model. They go to the channel, not into a thread.
- A background `ask_agent` acts with the person's Keycloak token, which lasts five minutes.
- Previews' datasets and secrets stay after the preview is taken down, like a deleted branch's.
- Whether `merge` on `main` and `dev` becomes usable once the deployer holds deploys for approval.
