# Agents — Design

How agents are defined, run and updated. Status: steps 1 to 3 of the plan are built, and the proxies of step 4: agent folders and their checks; the agent service, which Open WebUI talks to; and a sandbox per chat, Pi in a container with its workspace in a volume, which gets the LLM and the agent's platform tools through the agent service. The gateway serves each agent only its own tools and skills. Agents can clone the repos the person may read into their workspace and propose their changes as pull requests by the person. Still design: datasets instead of volumes, clean-up of old chats, Pi's native skills; agent accounts and grants, agents asking agents, and runs without a person (steps 7 to 10). See Chat in [spec.md](spec.md).

## Goals

1. Agents are files in Git: a prompt, a model, tools and skills, changed by reviewed pull requests like `members.yaml`.
2. A namespace can have several agents, e.g. an operator and a marketing agent.
3. Every chat with an agent is a real agent with its own workspace: files, a shell, a browser, its own history, in a sandbox.
4. Open WebUI stays the chat; it talks to agents as if they were models.
5. An agent has the rights of the person talking to it, and holds no secrets. It has more only where its folder grants them, reviewed like a role change, and only for the people of its namespace the grant names.
6. Agents can ask each other for work, without anyone gaining rights by the detour.

## Overview

```
Open WebUI ──/v1/chat/completions──▶ agent service (control plane)
  model "homepage/marketing"          │ one container per chat
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
| Agent folders | `<namespace>/config/agents/<name>/` | What an agent is: prompt, model, tools, skills |
| Agent service | Control plane, service `agents` | OpenAI-compatible endpoint for Open WebUI; starts, routes to and stops sandboxes; proxies the LLM and the tools |
| Sandbox image | `sandbox/` in the control plane's repo, built with it by Gitea Actions and pushed as `system/sandbox` | The harness (Pi) and what agents work with: bash, git, ripgrep, curl, jq; later Chromium, ffmpeg |
| Gateway | Control plane, service `gateway` | The tools, as today, acting as the person |

## Agent folders

Each namespace keeps its agents in its `config` repo, next to `members.yaml`. `system` gets a `config` repo too, writable by root admins only, so system agents work the same way.

```
homepage/config/
  members.yaml
  skills/                       # shared by the namespace's agents
    homepage-screenshots/
      SKILL.md
      viewports.yaml
  agents/
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
```

`agent.yaml`:

```yaml
description: Makes screenshots and media and keeps the homepage current
model: gpt-oss:120b          # a model of the LLM endpoint; empty for the default
tools:                       # gateway tools this agent may use
  - list_repos
  - read_file
  - propose_change
  - screenshot
skills: [homepage-screenshots]   # shared skills; its own skills/ are always included
repos: [homepage]            # optional: repos checked out into the workspace
visible: members             # who sees and may talk to it: members (default) or everyone
agents: [homepage/tester]    # agents it may ask for work (see Agents asking agents)
grants:                      # rights of its own (see Accounts and grants); none by default
  - repo: todo
    branch: dev
    level: autonomous        # merges its pull requests once checks pass; dev-todo deploys
    for: members
```

- `prompt.md` is the system prompt. The platform appends fixed rules after it (act as the person, propose and never claim a change is merged, give the link); a prompt can add rules but not remove these.
- The model id, and the name people mention, is `<namespace>/<agent>`, e.g. `homepage/marketing`.
- A new namespace gets `agents/operator/` written into its `config` repo with today's operator prompt. A namespace without an `agents/` folder still gets that operator.
- The `homecloud/approval` check on `config` pull requests also checks agent files: known tools and skills, a model the endpoint offers, a prompt that is not empty. Adding or widening an agent needs an admin's approval like a change to `members.yaml`.

## Accounts and grants

By default an agent has no rights of its own: every tool call acts as the person whose message started the turn, and changes leave only as pull requests by that person. A namespace can give an agent more, per repo and branch, with the autonomy levels of [spec.md](spec.md) (Rights and autonomy).

### Who sees an agent

`visible` decides who sees an agent in Open WebUI and may talk to it:

- `members` (the default): the people in the namespace's `members.yaml`, admins included, and root admins.
- `everyone`: everyone signed in, for agents meant to serve all, e.g. a help desk or `system/operator`.

The gateway sets this as the agent's access in Open WebUI, by group, as it keeps the Gitea teams in line with `members.yaml`. The agent service checks it again on every request, since Open WebUI is not the only way in, and answers someone who may not use the agent with a refusal that says why. Today every agent is visible to everyone.

### The agent's account

Each agent with grants gets an account in Gitea, `agent-<namespace>-<name>` (e.g. `agent-homepage-dev`), made and removed by the gateway as agent folders come and go. It has no password. Its token is held by the agent service and never enters the sandbox; the sandbox keeps only its per-turn token, as now. A Keycloak account per agent comes with runs without a person (below), when the gateway's other tools need an identity that is not a person's.

### Grants

```yaml
grants:
  - repo: todo
    branch: dev
    level: autonomous     # merge and deploy
    for: members
  - repo: todo
    branch: main
    level: autonomous
    for: admins
    checks: [ci]          # must be green before it merges; Gitea's required checks always apply too
  - repo: todo
    level: propose        # outsiders' proposals, see below
    for: everyone
```

| Level | The agent may, with its own account |
| --- | --- |
| `propose` | Push its own branches, `agent/<name>/*`, and open pull requests from them |
| `merge` | Also merge its pull requests into `branch` once the checks pass; a person approves the deploy |
| `autonomous` | Also deploy: the merge deploys as any merge to `main` or `dev` does |

- **`for` says whose requests may use a grant**, and only names groups of the agent's own namespace: `members` (admins included) or `admins`. `everyone` is allowed for `propose` only. A grant naming another namespace's group fails the approval check. Without `for`, a member could get through the agent what they may not do themselves, e.g. merge to `main`.
- **Each action is decided at the gateway, in this order:** if the person may do it themselves, it is done as the person, as today. Otherwise, if a grant covers the repo, branch and level and the person is in its `for`, it is done with the agent's account. Otherwise it is refused, and the agent is told which grant would be needed and that it can propose instead.
- **Gitea enforces it too.** The gateway puts the agent's account into the push and merge allowlists of the branches it was granted, and protects `agent/<name>/*` for it alone. If the gateway erred, Gitea still refuses.
- **`merge` waits for the deployer.** Until the deployer can hold a deploy for a person's approval (not built), every merge to `main` or `dev` deploys, so the approval check refuses `merge` on those branches and asks for `autonomous`, which says what happens.
- **Pull requests and logs name who asked.** A pull request the agent opens or merges says "Asked by bob in <chat>", written by the gateway, not the model. Commits pushed from the sandbox are authored by the agent's account; their messages are the model's and prove nothing, so the agent service logs each push and merge with the person, the agent and what started the run.
- **A shared channel is one workspace,** so one person's turn may carry another's work: bob edits, alice says "merge it". A grant on `main` therefore merges only a pull request approved by someone other than its author. The request of the person whose grant is used counts as that approval if they did not write it and the agent showed them the diff in that turn.

### Pushing and merging

- **Push:** the Git proxy of the agent service serves `git-receive-pack` too, for agents with a grant on the repo. It reads the ref updates at the start of the push and refuses any ref other than `refs/heads/agent/<name>/...`, then forwards the push to Gitea with the agent's token. Granted branches such as `main` are reached only by merging a pull request, never by a push, so every change keeps its diff, its checks and an easy revert. `propose_workspace_changes` stays for agents without grants.
- **Merge:** a tool `merge_change(namespace, repo, number)`. The gateway checks the grant, `for`, the checks and the approval rule above, then merges as the agent's account.
- **Deploy and roll back:** no tool of their own. A merge deploys through the pipeline, as for people. Rolling back is a revert, merged the same way.

### Outsiders

With `propose` for `everyone`, someone who is not in the namespace can ask its agent for a change. The agent pushes the branch with its own account, since the person cannot write there, and opens the pull request "on behalf of bob (not in shop)". The namespace's people review and merge it as any pull request. The gateway caps such proposals per person and day. Without that grant, an outsider gets the agent with their own rights: they read what they may read, and proposing fails with the reason.

### What no grant can lift

The gateway refuses these whatever an agent folder says, and the approval check refuses grants that would need them:

- Writing to a `config` repo, so an agent never widens its own grants, and changing `members.yaml`.
- Changing branch protection, deleting repos, or anything in Gitea's or Open WebUI's settings.
- Grants on repos of another namespace. Grants in `system` need a root admin's approval.
- `enabled: false` in `agent.yaml`, or a root admin stopping the agent, revokes its token at once and ends its running turns.

## Agents asking agents

A tool, `ask_agent(agent, task, limits, done_when)`, hands work to another agent: what to do, what bounds it, and what a good answer looks like, as fields, not a paragraph the other agent has to guess from.

- **Whom:** only the agents in the asker's `agents` list, and only those the person may use themselves (`visible`). The approval check checks that the listed agents exist.
- **Rights:** the asked agent runs for the same person, with its own tools and grants. A grant is always checked against the person who started the chain, never against the asking agent, so no detour adds rights: bob asking `homepage/dev` to ask `shop/dev` is bob asking `shop/dev`.
- **Where:** in its own sandbox for the same chat (key: the chat and the asked agent), with its own workspace. The two share work through Git, e.g. the asker pushes `agent/dev/login` and asks the tester to check that branch.
- **Depth:** at most three hops. The agent service keeps the depth in the turn's state, not in anything the model writes, so A asking B asking A ends.
- **Waiting:** the asker waits for the answer, which counts against its turn's time limit; the asked agent's tool calls count against its own cap. If the asked agent is already working in this chat, the call waits for its turn. Its answer comes back as the tool result, word for word, and shows in the reasoning under its name.
- **Reporting back later** (the asked agent works on and posts in the channel when done, e.g. a tester after the preview is deployed) needs runs without a person, below.

## Runs without a person

Design to be worked out; it is the base for routines, reporting back, and keeping branches current when `main` moves.

- **What starts a run:** a person (today), another agent, a routine (a schedule, in the agent's folder), or an event (a push, a pull request, an alert).
- **Rights:** a routine names its owner, a member of the namespace, and runs for them; it uses a grant only if the owner is in its `for` and the grant says `unattended: true`. Otherwise an unattended run only reads and proposes, as the spec says.
- **Where the answer goes:** the agent service posts into the namespace's channel, or the chat that asked, under the agent's name.
- **The record:** every action logs the person it acts for, the account used (the person's or the agent's) and what started it. "Nobody watching" (a routine, an event or an agent) is one filter in Grafana.

## Skills

Skills use the Agent Skills format: a folder with `SKILL.md` (frontmatter `name` and `description`, then instructions) and any files it needs. Pi loads them natively.

- An agent sees each of its skills' name and description in its prompt, and reads a skill's instructions and files only when a task needs it.
- Tools are abilities, enforced by the gateway. Skills are know-how: how to use the tools for a job. A skill grants nothing; a call to a tool the agent lacks is refused.

## Agent service

An OpenAI-compatible API, reachable only from Open WebUI on the system network.

- `GET /v1/models` lists every agent folder as a model. Open WebUI shows them in its model picker; the gateway no longer syncs model presets.
- `POST /v1/chat/completions` streams the agent's answer. Tool calls and progress show as reasoning, so people see what the agent does.
- Open WebUI's connection sends, as custom headers, `X-Homecloud-Chat: {{CHAT_ID}}` and `X-Homecloud-Task: {{TASK}}`, and the person's Keycloak token (`system_oauth`), which the agent service verifies.
- Requests with a task header (titles, tags, follow-up suggestions) go straight to the LLM without a sandbox.
- Open WebUI sends the whole conversation each time. The agent service passes on only the new message, and Open WebUI's own system messages (a channel thread's history): the harness keeps its own session, including tool calls and results. A fresh session, e.g. of a chat begun before sandboxes, gets the earlier messages as context. Branching the session when a message is edited or regenerated is to come.
- In the answer, Pi's thinking, its tool calls, failed tool results and what it says between tool calls show as reasoning; its last message is the content.

## Sessions

A container belongs to a chat, not to an agent or a person.

| Situation | Containers |
| --- | --- |
| Alice and Bob each chat with `homepage/marketing` | 2 |
| Alice opens a second chat with it | 3 |
| Alice returns to her first chat the next day | Hers starts again where it left off |

- **Workspaces are separate.** Each chat has its own volume (built: a Docker volume `homecloud-agent-<hash>`; planned: a ZFS dataset, for snapshots and quotas) holding the workspace and the session, so drafts of one chat never show in another. Agents share work through Git (branches and pull requests) and the namespace's media library, like people do.
- **Rights are separate.** Every tool call acts as the person whose message started the turn.
- **Team channels:** a namespace channel is one conversation shared by its members. Open WebUI names the channel (`channel:<id>`), not the thread, in its requests, so a channel gets one container per agent for all its threads; each thread's history still comes along with each message. Each turn acts as the person who mentioned the agent in that message. The workspace is visible to everyone in the thread; private work belongs in a private chat.
- **One turn at a time per chat;** a message sent while the agent works waits for the turn to end.

## Sandbox

- One container per chat and agent from the sandbox image, its main process `pi --mode rpc`: the agent service attaches to its stdin and stdout, sends commands and reads events, so the sandbox opens no port. Built.
- Pi gets the agent's prompt with the platform's rules and a note on its sandbox; its own tools (read, write, edit, bash) work in `/data/work`; a Pi extension (`sandbox/homecloud.ts`) adds the agent's platform tools.
- Mounted: the chat's volume at `/data` (workspace and Pi's session). Skills are still read through the gateway's `load_skill`; mounting the agent's folder and skills for Pi to load natively is to come.
- Git: the agent service passes fetches (never pushes) on to Gitea with its admin account, after asking Gitea, as the person, whether they may read the repo; Gitea's Git protocol knows no Sudo. Changes leave only as patches through `propose_patch`, which Gitea applies as the person, so branch protection and authorship hold.
- Network: `homecloud-sandboxes`, internal, shared only with the agent service. On a second port (8071) the agent service passes on the LLM, adding the API key and enforcing the agent's model, and the gateway's tools, adding the Keycloak token of the person whose turn runs. A sandbox names itself with a random token given at its start, which counts only during a turn. The sandbox holds no key, no person's token and no Docker access.
- Limits per container: 1 GB of memory, one CPU, 512 processes, no capabilities. Containers are started through the sandbox guard, like the user runner's, which allows only the sandbox image, with the label `homecloud.sandbox`, these limits, that network and volumes named `homecloud-agent-*`. It knows its containers by the label, so it still does after a restart.

## Lifecycle and updates

- **Start:** on the first message of a chat, or the next message after a stop. A new chat's first answer takes a few seconds more.
- **Stop:** after 15 minutes without a message. The volume stays. When the agent service restarts, it removes all sandbox containers; their next message starts them again.
- **Clean-up:** Open WebUI does not report deleted chats; datasets of chats unused for 30 days are removed, after a snapshot.
- **Updates:** an agent is built from three layers, each changed in its own place.

| Layer | Lives in | A change reaches running chats |
| --- | --- | --- |
| Sandbox image | `system/sandbox` | At their next turn |
| Agent definition | `<namespace>/config/agents/` | At their next turn |
| Tools and platform rules | Gateway, in the control plane | When the gateway is deployed |

Before each turn the agent service compares the container's image and agent commit with the current ones and, if either changed, restarts the container between turns; the session resumes from the dataset. A running turn is never interrupted.

A new namespace or repo starts nothing: its agents appear in the model list, and containers start only when someone chats.

## Limits

- Per person: at most 3 running sandboxes; a new chat stops that person's oldest idle one, or is refused while all three work.
- Machine: a cap from free memory; idle sandboxes are stopped first.
- Per turn: a time limit (10 minutes) and a cap on tool calls (60 in a sandbox), after which the turn is aborted.

## Observability

- Running sandboxes are a Grafana dashboard: chat, person, agent, state (working, idle, stopped), CPU and memory.
- A `list_sessions` tool shows the same to the operator agents.
- Each turn logs the person, agent, tools called and outcome to Loki.

## Plan

1. Agent folders: parsing, checks on `config` pull requests, `system/config`, the operator written into new namespaces. Built. The gateway serves an agent its tools and skills (`load_skill`, `read_skill_file`) when a call names it in `X-Homecloud-Agent`.
2. Agent service: models list, streaming, chat id, the person's Keycloak token (Open WebUI's `system_oauth`, so no forwarded-user JWT is needed), task requests to the LLM. Built with the loop in its own process: the LLM with the agent's prompt, and its tools from the gateway as the person; tool calls show as reasoning. Open WebUI has no LLM key and no tools any more.
3. Sandbox image with Pi; one container per chat with its volume, idle stop and resume. Built; requests without a chat id still run in the agent service's process. To come: a dataset per chat, removing unused ones.
4. LLM and tool proxies; per-agent tool lists enforced; skills. Proxies built, with the per-agent tool lists of step 1. Git built: the agent service serves Git's fetch protocol to sandboxes (`$HOMECLOUD_GIT/<namespace>/<repo>.git`) for the repos the person may read, and the gateway's `propose_patch` applies a patch as the person on a `change/...` branch with a pull request; in Pi it is `propose_workspace_changes`, which proposes a checkout's changes and adds later ones to the same pull request. Several agents work on one repo like people do: each on its own branch, merged by a person. To come: Pi's native skills, Git LFS.
5. Move the operator agents over; remove the model preset sync.
6. Marketing agent: `screenshot` tool, built: headless Chrome in the control plane, signed in as the person, the PNG proposed as a pull request; tested with a media agent that bob asks for a screenshot of the todo app. To come: Git LFS for larger media, the media library, video.
7. Accounts and grants: `visible` set in Open WebUI and checked by the agent service; an account per agent with grants; grants parsed and checked on `config` pull requests, with the limits no grant lifts; Gitea's allowlists kept in line; pushes to `agent/<name>/*`; `merge_change`; outsiders' proposals.
8. `ask_agent`, waiting for the answer.
9. Runs without a person: routines and events, posting into channels, the record of what started a run; keeping pull requests current when their base moves.
10. Previews of `change/` and `agent/` branches, and a `browse` tool (click, type, read) on the gateway's Chrome, so an agent can try its change in the running app.

## Open questions

- Branching Pi's session when a message is edited or regenerated in Open WebUI: today the newest message is simply sent on.
- Thread ids for channels, so each thread gets its own sandbox.
- GPU or a local LLM for agents that run many turns.
- How Open WebUI shows an answer posted by the agent service without a person's message (runs without a person, reporting back).
- Whether `merge` on `main` and `dev` becomes usable once the deployer holds deploys for approval.
