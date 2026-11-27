# Garrison

A home cloud you run by talking to it, with Git underneath.

Garrison turns one Ubuntu machine into a small cloud for a household or a team: apps on their own addresses with TLS, single sign-on, monitoring, backups, and a team chat where AI agents do the work with you. You ask in the chat, *"deploy Jellyfin"*, *"why is the todo app down?"*, *"make the background blue"*, and the agents look things up and propose the change as a pull request. A person reviews it and merges it, and the platform deploys what is in Git. Nothing changes behind Git's back.

## What you get

- **Apps by conversation.** Each app is a file in Git (`config/stacks/<name>.yaml`) and answers at `https://<name>.<your domain>`. Branches become environments: `dev` runs at `dev-<name>.<domain>`, and pull requests can get previews.
- **Agents that act with your rights.** In a unit's channel in Open WebUI, `@operator`, `@coder`, `@sentry`, `@scout`, `@designer` and `@commander` (which finds the right one) work for whoever mentions them. They read stacks, logs, metrics and code, and propose changes; only people merge. Each chat runs in a gVisor sandbox, and the internet is reachable only through a proxy, as far as each agent is allowed.
- **Units for people and projects.** A unit (a Gitea organisation) has its own people, repos, apps, chat channel and secrets. Who may change it is in `config/members.yaml`, and changing that file needs someone else's approval.
- **Routines and chains.** Agents run on a schedule, when a branch moves, or when an alert fires. They ask a person when they need an answer. A chain of command writes down who does what in a job, e.g. weekly software updates: the sentry lists the versions, the scout finds the releases, the coder proposes the updates.
- **Safety nets.** ZFS snapshots of all data (hourly, daily, monthly), and a snapshot before every deploy. A restore is a pull request. Secrets are encrypted in Git with SOPS. A dashboard feed shows everything that happened and how it was handled.
- **Monitoring.** Grafana with Prometheus and Loki. Alerts go to the feed and can start an agent's routine.

## Install

On a fresh Ubuntu machine (26.04 is what the tests use), as root:

```
sudo ./install.sh --answers my.env
```

The answers file sets at least `DOMAIN`. See `test/answers/selfsigned.env` for an example, and `install.sh` for every setting (TLS mode, the platform's account, ZFS pool size, the time zone). The installer is safe to run again. It deploys Postgres, Traefik, Keycloak, Gitea and its runner, builds the control plane in Gitea, and hands over to the deployer, which keeps everything as Git says from then on. Let's Encrypt is not built yet: installs use a self-signed certificate.

The installer ends with a link: open it to complete the setup in the browser.

1. **The first root admin:** your username, email and password. You sign in with them everywhere.
2. **The AI providers:** each OpenAI-compatible API the agents think with, such as an inference server on your network, Ollama's cloud or OpenRouter, with its key. Each is tested as you add it: it must list its models.
3. **The default model:** every agent uses it, in every unit. It is tested with a question, then saved to `llm/` of `garrison/config`.

The link works only until the setup is complete (it is also in `/etc/garrison/setup-link`, root only). Later, on the dashboard's Models page, a root admin adds providers and puts single agents on another model, e.g. the coder on a model for code: one model per agent, the same in every unit.

Then open `https://<domain>/` (the dashboard), `https://chat.<domain>` (the team chat) and `https://git.<domain>` (Gitea), and sign in through Keycloak. Add people on the dashboard's People page: it makes a link you send them yourself (Garrison sends no mail), with which they choose their username and password; a link into a unit proposes them in its `members.yaml`. Forgotten passwords work the same way: a root admin makes a link to set a new one.

## How it works, in one picture

```
 you ──chat──> Open WebUI ──> agent service ──> sandbox (Pi agent, gVisor)
                                   │                    │ tools (MCP)
                                   v                    v
                               LLM provider          gateway ──acts as you──> Gitea: pull requests
                                                                                 │ you merge
                                                                                 v
                          Kubernetes (k3s) <──deploys── deployer <──webhook── Gitea (main)
```

The control plane (Go, `control-plane/`) is one binary with several roles: the **deployer** applies Git to Kubernetes (k3s), the **gateway** gives agents their tools and keeps units, members and chat in sync, the **agent service** runs each chat in its sandbox, and **home** serves the dashboard.

## Learn more

- [docs/walkthrough](docs/walkthrough/README.md): how it is used, step by step with screenshots.
- [docs/spec.md](docs/spec.md): the design. [docs/status.md](docs/status.md): what is built.
- [docs/agents.md](docs/agents.md): the agents, their tools, grants, routines and chains.
- [docs/scenarios.md](docs/scenarios.md): what people can and cannot get done by talking to the agents, as the tests play it.
- [test/README.md](test/README.md): the end-to-end tests on throwaway VMs.
- [AGENTS.md](AGENTS.md): a guide to this repository for people and coding agents working on it.

## Repository

| Path | Contents |
| --- | --- |
| `install.sh`, `installer/` | The installer and its helpers |
| `garrison/` | The system stacks, one directory per repo of the `garrison` organisation in Gitea, pushed at install |
| `control-plane/` | Go: deployer, gateway, agent service, dashboard, guards; the `garrison` CLI |
| `templates/` | Starting contents of new config repos and app repos, the agents among them; compiled into the control plane |
| `sandbox/` | The agents' sandbox image: Pi, its tools, a headless browser |
| `files/` | The media library's file browser image |
| `schemas/` | JSON Schema for stack files, unit config, change records |
| `host/` | Host-side scripts: BuildKit, repairing services after a reboot |
| `docs/` | Spec, status, agents, scenarios, walkthrough |
| `test/` | End-to-end tests on throwaway Multipass VMs |
