# Agents — Design

How agents are defined, run and updated. Status: design, not built. Today each namespace's agent is a model preset in Open WebUI with a generated prompt and the gateway's MCP tools (see Chat in [spec.md](spec.md)); this replaces that.

## Goals

1. Agents are files in Git: a prompt, a model, tools and skills, changed by reviewed pull requests like `members.yaml`.
2. A namespace can have several agents, e.g. an operator and a marketing agent.
3. Every chat with an agent is a real agent with its own workspace: files, a shell, a browser, its own history, in a sandbox.
4. Open WebUI stays the chat; it talks to agents as if they were models.
5. An agent never has more rights than the person talking to it, and holds no secrets.

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
| Sandbox image | System repo `system/sandbox`, built by Gitea Actions | The harness (Pi) and what agents work with: git, Chromium, ffmpeg |
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
```

- `prompt.md` is the system prompt. The platform appends fixed rules after it (act as the person, propose and never claim a change is merged, give the link); a prompt can add rules but not remove these.
- The model id, and the name people mention, is `<namespace>/<agent>`, e.g. `homepage/marketing`.
- A new namespace gets `agents/operator/` written into its `config` repo with today's operator prompt. A namespace without an `agents/` folder still gets that operator.
- The `homecloud/approval` check on `config` pull requests also checks agent files: known tools and skills, a model the endpoint offers, a prompt that is not empty. Adding or widening an agent needs an admin's approval like a change to `members.yaml`.

## Skills

Skills use the Agent Skills format: a folder with `SKILL.md` (frontmatter `name` and `description`, then instructions) and any files it needs. Pi loads them natively.

- An agent sees each of its skills' name and description in its prompt, and reads a skill's instructions and files only when a task needs it.
- Tools are abilities, enforced by the gateway. Skills are know-how: how to use the tools for a job. A skill grants nothing; a call to a tool the agent lacks is refused.

## Agent service

An OpenAI-compatible API, reachable only from Open WebUI on the system network.

- `GET /v1/models` lists every agent folder as a model. Open WebUI shows them in its model picker; the gateway no longer syncs model presets.
- `POST /v1/chat/completions` streams the agent's answer. Tool calls and progress show as reasoning, so people see what the agent does.
- Open WebUI's connection sends, as custom headers, `X-OpenWebUI-Chat-Id: {{CHAT_ID}}`, the message id, the user message's id and its parent's id, and `X-OpenWebUI-Task: {{TASK}}`. The user comes as a signed JWT (`ENABLE_FORWARD_USER_INFO_HEADERS` with `FORWARD_USER_INFO_HEADER_JWT_SECRET`); the agent service checks the signature.
- Requests with a task header (titles, tags, follow-up suggestions) go straight to the LLM without a sandbox.
- Open WebUI sends the whole conversation each time. The agent service passes on only the new message: the harness keeps its own session, including tool calls and results. When the parent id shows a message was edited or regenerated, the session is branched from that point.

## Sessions

A container belongs to a chat, not to an agent or a person.

| Situation | Containers |
| --- | --- |
| Alice and Bob each chat with `homepage/marketing` | 2 |
| Alice opens a second chat with it | 3 |
| Alice returns to her first chat the next day | Hers starts again where it left off |

- **Workspaces are separate.** Each chat has its own ZFS dataset holding the workspace and the session, so drafts of one chat never show in another. Agents share work through Git (branches and pull requests) and the namespace's media library, like people do.
- **Rights are separate.** Every tool call acts as the person whose message started the turn.
- **Team channels:** a namespace channel is one conversation shared by its members, so it gets one container per thread. Each turn acts as the person who mentioned the agent in that message. The workspace is visible to everyone in the thread; private work belongs in a private chat.
- **One turn at a time per chat;** a message sent while the agent works waits for the turn to end.

## Sandbox

- One container per chat from the sandbox image, its main process `pi --mode rpc`: the agent service sends commands on stdin and reads events from stdout, so the sandbox opens no port.
- Mounted: the chat's dataset as the workspace, and the agent's folder and skills read-only, at the commit the turn started with.
- Network: only the agent service. The agent service proxies the LLM, adding the API key, and the gateway's tools, adding the person of the current turn. The sandbox holds no key, no token and no Docker access.
- CPU and memory limits per container. Containers are started through a guard like the user runner's, which allows only sandbox containers with these settings.

## Lifecycle and updates

- **Start:** on the first message of a chat, or the next message after a stop. A new chat's first answer takes a few seconds more.
- **Stop:** after 15 minutes without a message. The dataset stays.
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

- Per person: at most 3 running sandboxes; a new chat stops that person's oldest idle one, or waits.
- Machine: a cap from free memory; idle sandboxes are stopped first.
- Per turn: a time limit and a cap on tool calls.

## Observability

- Running sandboxes are a Grafana dashboard: chat, person, agent, state (working, idle, stopped), CPU and memory.
- A `list_sessions` tool shows the same to the operator agents.
- Each turn logs the person, agent, tools called and outcome to Loki.

## Plan

1. Agent folders: parsing, checks on `config` pull requests, `system/config`, the operator written into new namespaces.
2. Agent service with a stub harness: models list, streaming, chat id and signed user, task requests to the LLM.
3. Sandbox image with Pi; one container per chat with its dataset, idle stop and resume.
4. LLM and tool proxies; per-agent tool lists enforced; skills.
5. Move the operator agents over; remove the model preset sync.
6. Marketing agent: `screenshot` tool (Chromium in the sandbox, signed in as the person), Git LFS, the media library.

## Open questions

- Which chat id Open WebUI sends for a mention in a channel thread.
- Whether the forwarded-user JWT carries what the gateway needs (username, root-admin group), or the agent service looks the person up in Keycloak.
- Pi's RPC protocol in detail: session files, branching, cancelling a turn.
- Whether the agent service is Go in the control plane, as assumed here, or the TypeScript orchestrator in `orchestrator/`.
- GPU or a local LLM for agents that run many turns.
