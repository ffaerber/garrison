# Working on Garrison

A guide for people and coding agents changing this repository: how the system fits together, where things live, how to test, and the traps already found. Read [README.md](README.md) first for what Garrison is. [docs/spec.md](docs/spec.md) is the design and [docs/status.md](docs/status.md) is what is built; when they disagree, status is the truth and its "Differs from the spec" table says why.

## The system in one page

Garrison runs on one Ubuntu machine with Kubernetes (k3s) and ZFS. **Gitea is the source of truth**: everything that runs is described in a repo, and the control plane makes the machine match `main`. How it uses Kubernetes, for learning: [docs/kubernetes.md](docs/kubernetes.md).

Garrison moved from Docker Swarm to k3s: the platform, the agents' sandboxes (pods under gVisor) and the apps (a namespace per environment) run on it; more machines come next (docs/kubernetes.md, "Still to move").

- **Units** are Gitea organisations. `garrison` is the platform's own: one repo per system stack (`garrison/<stack>` in this repo, with a `kubernetes.yaml` each: Kubernetes manifests, applied in the namespace `garrison`), plus `garrison/config`, which holds **the agents** (`agents/<name>/agent.yaml`, `prompt.md`, skills), the chains of command (`chains/`) and the LLM providers and models (`llm/`). Every other unit has a `config` repo (`members.yaml`, `stacks/<name>.yaml`, `grants.yaml`, `routines.yaml`, `secrets/`, `restores/`) and app repos holding only code.
- **The control plane** (`control-plane/`) is one Go binary, `garrison`, with one role per subcommand (`control-plane/cmd/garrison/main.go`):
  - `deploy`: the **deployer**, which applies Git to the cluster on each webhook and every 15 s. System stacks come from `garrison/*/kubernetes.yaml` (`internal/system`: `${VAR}`s, the label `garrison.io/stack`, ConfigMaps from files, `kubectl apply --prune`); apps come from `<ns>/config/stacks/*.yaml`, rendered by `internal/render`, with environments per branch, previews, snapshots before deploys, restores and secrets.
  - `gateway`: MCP tools for the agents (`internal/gateway/tools.go`), acting with each person's own token. It also syncs members to Gitea teams (`members.go`), and units, groups, channels and models to Open WebUI (`openwebui.go`). It runs the approval check `garrison/approval` on config pull requests.
  - `agents`: the **agent service**. Open WebUI calls it as an OpenAI-style API. It runs each chat in a sandbox container (`internal/sandbox`, image `sandbox/`: Pi, under gVisor), holds the LLM key, proxies the internet per agent, and runs routines and `ask_agent` (`internal/agentapi`).
  - `home`: the dashboard at `https://<domain>/` (`internal/home`), with the feed, cards, secrets and Models page.
  - `guard`, `gate`: Docker access for pipelines, and the sign-in gate in front of apps.
  - `validate`, `render`, `apply`, `applied`, `claims`: the CLI on the host.
- **Agents** are defined only in `garrison/config` and work in every unit. A unit gives them rights (`grants.yaml`, which gives an agent account of its own in Gitea) and routines (`routines.yaml`). The defaults come from `templates/garrison-config/agents/`: operator, commander, coder, sentry, scout, media. Design: [docs/agents.md](docs/agents.md).
- **People** sign in through Keycloak (realm `garrison`). The group `root-admins` may change the platform. Unit roles come from `members.yaml`, applied to Gitea teams `admins` and `members`; nobody approves their own role change.
- **Data**: Postgres (`garrison` database: events, routine state), ZFS datasets per app environment, Sanoid snapshots, and SOPS-encrypted secrets in Git.

How a change travels: chat → agent (sandbox) → gateway tool, as the person → a pull request in Gitea → a person approves and merges → webhook → deployer → the cluster. The dashboard feed records each step.

## Where to change what

| To change | Edit | Notes |
| --- | --- | --- |
| An agent's prompt, tools, skills | `templates/garrison-config/agents/<name>/` | Compiled into the control plane (`templates/templates.go`). The agent list is tested in `control-plane/internal/agents/agents_test.go` |
| A tool agents can call | `control-plane/internal/gateway/tools.go` plus its handler | Also list it in the agent's `agent.yaml` `tools:` |
| How a stack file becomes Kubernetes objects | `control-plane/internal/render/kubernetes.go`, `schemas/stack.schema.json` | Golden files: `make golden` |
| Members, teams, branch protection | `control-plane/internal/gateway/members.go` | |
| What Open WebUI shows | `control-plane/internal/gateway/openwebui.go` | Groups, channels, the pinned how-to, models |
| Routines, `ask_agent`, sandboxes | `control-plane/internal/agentapi`, `internal/sandbox`, `sandbox/` | |
| A system stack | `garrison/<stack>/kubernetes.yaml` | Pushed at install; on a running machine, `test/vm/push-repos.sh`. `garrison system -render DIR` shows what is applied |
| Who may reach which pod | `garrison/platform/kubernetes.yaml` | NetworkPolicies, deny by default; checked by the scenario `system` |
| New repos' starting contents | `templates/` | `unit-config`, `garrison-config`, `app/todo` |
| The installer | `install.sh`, `installer/` | Safe to run again: every step checks what exists |

## Build and test

```
make test     # go vet and go test ./..., in a golang container (GO=go for a local toolchain)
make build    # bin/garrison
make golden   # regenerate render's golden files
```

The end-to-end tests run on throwaway Multipass VMs; see [test/README.md](test/README.md).
- `test/vm/steps.sh` runs a tree: **fixtures** (base, install, login, agent, deploy, team), saved as VM snapshots, and **scenarios**, each in a fresh clone of its fixture, up to `JOBS` (4) at once.
- `test/vm/steps.sh --list` shows what is green or stale, and `test/vm/steps.sh <scenario>` reruns one.
- Each step is `test/e2e/scenarios/<step>.sh` on `test/e2e/lib.sh`. The browser scenarios are `test/ui/<step>.py` (Playwright) on `test/ui/lib.py`, which holds every selector of Gitea's and Open WebUI's pages.
- The agents use a real LLM: local Ollama by default, `LLM=cloud` for Ollama's cloud. So agent checks look at what happened (pull requests, authors, approvals, deploys), not at wording, and retry once with a plainer request.
- Logs: `bin/steps-<step>.log`. A failed scenario's VM is kept as `garrison-test-<step>`: `multipass exec garrison-test-<step> -- sudo ...` to look around.

What reruns: a scenario's own file reruns only it; `test/e2e/lib.sh` or `smoke.sh` everything from login; anything the installer uses (`control-plane/`, `templates/`, `garrison/`, `sandbox/`, `files/`, `installer/`) everything from install, about 45 minutes in all.

**To add a scenario:**
1. Write `test/e2e/scenarios/<name>.sh` defining `section_<name>`. Use `talk_setup || return 0` first if it talks to the agents of todo.
2. Add it to `STEPS`, `PARENT` and `NUMBER` in `test/vm/steps.sh`, with a `step_<name>`.
3. Describe it in `docs/scenarios.md`.

## Conventions

- **Everything through Git.** Platform state lives in a repo and changes by pull request. Code that pushes to `main` directly is the exception (secrets, by the platform's account) and says why.
- **Agents never merge their own way past people.** They propose; people approve. Rights beyond the person's come only from a reviewed grant.
- **Comments and docs** say what and why in plain sentences, present tense. A comment above a function starts with its name and arguments, e.g. `# set_config NS PATH FILE [APPROVER]: ...`. Keep `docs/status.md` and the relevant doc in step with a change.
- **Tests** go with changes: a Go test for logic, a scenario check for behaviour on a real install.
- Never put secrets, API keys or personal data in the repo, in prompts, or in requests to outside services. The LLM key reaches a VM only as a file (`put_llm_key` in `test/vm/steps.sh`).

## Traps already found

- **Gitea:**
  - A team made to include all repos misses a repo created at the same moment. The members sync now adds missing repos (`teamRepos`).
  - Gitea drops a team without access from a branch rule. A rule that differs is written again at every sync, and each write makes Gitea re-check pull requests and refuse merges for a while.
  - Merges right after an approval can get "Please try again later": retry, as `merges` and `to_main` in `test/e2e/lib.sh` do.
  - Gitea counts an approval only if its author was in an approving team when they gave it (`official`). A new admin's very first visit to Gitea makes their account a moment before the members sync puts it in `admins`, so an approval given then never counts. The `garrison/approval` check skips such approvals and says to approve again.
  - Right after an approval, a pull request's page can offer only "merge when all checks succeed". That schedule waits for a check to change; if they all passed already, it never merges. `Person.merge` in `test/ui/lib.py` cancels it and merges again.
- **gVisor:** a sandbox has no Docker DNS. Reach containers by IP, or through the agent service's proxy, which looks names up.
- **Multipass:**
  - It is a snap and cannot read files under `/tmp`. Put anything it transfers under the repo (`bin/` is ignored).
  - Its daemon (1.17) has crashed when several VMs were changed at once, and also stopping one VM with nothing else running, so `steps.sh` runs commands that change a VM one at a time. After a crash it does not come back by itself (its old dnsmasq holds the address): `steps.sh` runs `test/vm/revive-multipass.sh` after half a minute when sudo needs no password; otherwise run it by hand. A `multipass exec` running when the daemon crashes never returns: `steps.sh` kills it when the daemon's process changes, and fails that step instead of waiting forever. `JOBS=1` runs one VM at a time.
- **A cloned VM** gets a new address and hostname. k3s keeps its node name (`node-name` in `/etc/rancher/k3s/config.yaml`, set by the installer); `settle` in `steps.sh` waits for the rollouts and restarts BuildKit, which runs in Docker, until it is reachable by name again.
- **k3s:**
  - Its own runc puts containers under a stacked AppArmor label on Ubuntu 26.04 that cannot signal its own processes (Postgres never started). Pods run with crun (`default-runtime: crun`).
  - A pod's API token is mounted below `/var/run/secrets`, which is our read-only `/run/secrets`: pods set `automountServiceAccountToken: false`, or get the token inside their projected secrets volume (docs/kubernetes.md, Secrets).
  - A StatefulSet does not replace a pod that never became ready; one pod on a host directory is a Deployment with `strategy: Recreate`.
  - Liveness probes kill slow starters after a reboot: Gitea, Keycloak and Postgres have startup probes. Containers that exit while what they need is away fall into a growing crash-loop backoff: they wait for it in an init container.
  - Kubernetes's container metrics name only namespace, pod and container: join with `garrison_pod_info` (the deployer's) for stack and service.
- **Bash:** `set -e` is ignored inside anything run after `&&`, `||` or in an `if`, subshells included. `steps.sh` runs steps as plain statements and checks `$?` after.
- **Open WebUI:**
  - It limits sign-ins, so the gateway keeps its token.
  - A unit's group holds only people who have signed in to Open WebUI at least once. It finds them by the email of their Gitea account, or Keycloak's before their first Gitea sign-in (`OpenWebUI.email`); without that, someone added who had not opened Gitea did not see the channel.
  - In a channel, Open WebUI calls an agent only for a message that mentions it, or that is a Reply to one of its messages (`reply_to_id`). A plain message in its thread goes unanswered. The pinned how-to tells people to use Reply; the browser tests' `Person.reply(..., agent)` mentions the agent, and `commander` checks a Reply. The agent service cannot answer such messages itself: it acts with the person's token, which only Open WebUI's call carries.
  - Right after it starts, its sidebar can show no channels for a while; `Person.sidebar_has` waits for garrison's channel, which everyone has, before saying one is missing.
  - The models are Garrison's agents, `garrison/<agent>`, for everyone. What membership changes is the unit's channel: whether it shows in the sidebar and whether its messages can be read.
- **The LLM:** a check that needs a vague request to be clarified first depends on the prompts' rule to ask back (commander, operator, coder). Keep those rules when editing prompts.
