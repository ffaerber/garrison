# Comparison

How garrison compares with other tools for running AI agents, and what is worth borrowing from them. Written in November 2026 from each project's README, docs and third-party write-ups, not from running them; check the sources before relying on details.

## In short

The others are tools for one developer at one computer: they run several coding agents side by side and help pick or merge their work. Garrison is a server that a household or team shares: agents are files in Git, act with the rights of the person they work for, run in sandboxes, and their changes go out through reviewed pull requests and GitOps deploys. They complement each other more than they compete: someone can use Zed or Orca at their desk while the agents they talk to run in garrison.

## The tools

| | Kind | Agents | Main idea | People | License |
| --- | --- | --- | --- | --- | --- |
| **garrison** | Self-hosted platform (Swarm, Gitea, Keycloak) | Own agent loop (Pi) in a sandbox per chat; local or cloud models via Ollama | Agents and apps defined in Git, changed by reviewed pull requests, deployed by GitOps | Many: units, roles, SSO | — |
| **Claude Code** | CLI agent | Itself, with subagents | One strong agent with tools, worktrees, review and tests | One | Proprietary |
| **[multi-agents](https://github.com/BichengWang/multi-agents)** | Python CLI | Claude Code, Codex, an SDK agent (any model via litellm) | Teams of agents on one task (solo, reviewer, best-of-N, planner), measured against one agent | One | MIT |
| **[Herdr](https://github.com/ogulcancelik/herdr)** | Terminal multiplexer (Rust) | Detects Claude Code, Codex, Amp… in your terminals | Agent sessions that survive a dropped SSH connection; agents can drive it through a socket API | One | Apache-2.0 |
| **[Orca](https://github.com/stablyai/orca)** | Desktop app (Stably AI) | Claude Code, Codex, OpenCode, Pi and more | One prompt fanned out to several agents in their own worktrees; compare and merge the best | One | Open source |
| **[Zed](https://zed.dev)** | Code editor | Its own agent, agents over ACP, agent CLIs as terminal threads | Parallel agent threads in a sidebar, inside the editor | One (plus Zed's collaboration) | GPL / AGPL |

## Side by side

| | garrison | Claude Code | multi-agents | Herdr | Orca | Zed |
| --- | --- | --- | --- | --- | --- | --- |
| Runs | Always on, on a server | When you start it | One run per task | While its session lives | While the app is open | While the editor is open |
| Agents defined | `agent.yaml` + `prompt.md` in Git, by pull request | Settings, CLAUDE.md, subagent files | A spec string on the command line | By starting them | By starting them | Settings, ACP servers |
| Output | Pull requests, deploys | Edits in your checkout | A patch, `--apply` or a draft PR | Whatever the agents do | Worktrees to compare and merge | Edits in the editor |
| Rights | The person's, plus reviewed grants | Your account's | Your account's | Your account's | Your account's | Your account's |
| Sandbox | Container per chat, internet by allow-list | Optional OS sandbox | The CLIs' own; a bypass mode | None of its own | None of its own | None of its own |
| Secrets | SOPS/age, a key per unit, dashboard form | Your environment | Your logins and `.env` | Your environment | Your environment | Your environment |
| Without a person | Routines: schedule, push, alert | Headless `claude -p` from cron or CI | From CI | No | No | No |
| Watching | Event feed, Grafana, Loki | The terminal | JSON records with tokens and cost | Its panes | Its window, a phone app | The threads sidebar |
| Cost tracking | Not yet | Per session | Per agent call, with `--max-cost` | No | No | Per thread |
| Models | Local (Ollama) or cloud | Claude | Any | Whatever the agents use | Whatever the agents use | Many |

## Worth borrowing

1. **ACP, from Zed.** If garrison's agent service spoke the Agent Client Protocol, Zed and other ACP editors could talk to garrison's agents directly, not only through Open WebUI. Garrison keeps the rights, sandbox and review; people keep their editor. The most useful of these.
2. **Cost per agent call, from multi-agents.** Tokens and money per call in the event feed, with a budget per unit.
3. **Checks as the reviewer, from multi-agents.** Tests or a build gate a pull request before it is opened, rather than a second LLM.
4. **Attaching to a running agent, from Herdr.** Garrison's sandboxes already outlive a chat; `garrison attach <unit>/<agent>` from a terminal would be the equivalent of Herdr's sessions.
5. **Fan-out, from Orca.** One task to several agents, each pull request with its own preview to compare. Low priority: multi-agents' own benchmark (48 tasks: solo 27 solved, reviewer 29, best-of-3 28) found the gain not significant.

## Claude Code as a garrison agent

Instead of going through multi-agents, a unit could have agents that run `claude -p` in the sandbox for coding work and open a pull request like any other agent. It needs an Anthropic API key, or a subscription's token from `claude setup-token` (`CLAUDE_CODE_OAUTH_TOKEN`). A subscription is for one person: on a garrison several people use, each would store their own token as a personal secret, and the agent would run only for them. Garrison has no personal secrets yet; secrets belong to units. Check Anthropic's current terms before relying on this.

## Sources

- multi-agents: [GitHub](https://github.com/BichengWang/multi-agents)
- Herdr: [Tecmint](https://www.tecmint.com/herdr-run-ai-coding-agents-in-linux-terminal/), [README on docs.rs](https://docs.rs/crate/herdr/0.1.0/source/README.md), [DEV Community](https://dev.to/pvgomes/herdr-is-the-throughput-layer-for-coding-agents-1ied)
- Orca: [Better Stack guide](https://betterstack.com/community/guides/ai/orca-ai/), [PyShine](https://pyshine.com/Orca-Agent-Development-Environment-Parallel-AI-Coding/)
- Zed: [Parallel agents](https://zed.dev/blog/parallel-agents), [docs](https://zed.dev/docs/ai/parallel-agents.md), [Terminal threads](https://zed.dev/blog/terminal-threads)
