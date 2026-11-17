# Agents

One folder per agent. People mention an agent in the chat as `@<namespace>/<folder>`.

- `agent.yaml`: `description`, `model`, `tools` (the gateway tools it may use; `all` for every one, now and later), `skills` (shared skills from `skills/` at the top of this repo), `repos` and `internet` (what its sandbox reaches: `packages`, the package registries; `all`; or a list of `packages` and domains; none without it).
- `model`: empty for the home cloud's default, or one of the enabled models in `llm/models.yaml` of the repo `garrison/config`, as `<provider>/<model>`, e.g. `openrouter/anthropic/claude-sonnet-5.5`. The agent tool `list_models` lists them too. A model that is retired or turned off later falls back to the default, and the feed says so.
- `routines`: work it does by itself, for an `owner`, with a `task`: on a `schedule` (cron, e.g. `"30 6 * * mon-fri"`, in the home cloud's time zone), `every` (e.g. `6h`), `on_push` (`{repo, branch}`) or `on_alert` (label values of this namespace's alerts, e.g. `{alertname: ServiceFailing}`). Its answer goes to the namespace's channel.
- `prompt.md`: its instructions. The platform adds its own rules after them.
- `skills/<name>/SKILL.md`: skills of this agent only, in the Agent Skills format.

An agent acts with the rights of the person talking to it. Changes here need an admin's approval, like members.yaml.
