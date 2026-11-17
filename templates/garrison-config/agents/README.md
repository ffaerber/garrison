# Agents

The home cloud's agents, one folder per agent. Every agent works in every namespace: people mention it by its folder's name, as `@<folder>`, in a namespace's channel, and there it works in that namespace, with the rights of the person asking. Only root admins change this repo, so only they add or change agents.

- `agent.yaml`: `description`, `model`, `tools` (the gateway tools it may use; `all` for every one, now and later), `skills` (shared skills from `skills/` at the top of this repo), `agents` (the agents it may ask, by name, or `any`), `visible: members` (only for the people of the namespace it works in) and `internet` (what its sandbox reaches: `packages`, the package registries; `all`; or a list of `packages` and domains; none without it).
- `model`: empty for the home cloud's default, or one of the enabled models in `llm/models.yaml` of this repo, as `<provider>/<model>`, e.g. `openrouter/anthropic/claude-sonnet-5.5`. The agent tool `list_models` lists them too. A model that is retired or turned off later falls back to the default, and the feed says so.
- `prompt.md`: its instructions. The platform adds its own rules after them.
- `skills/<name>/SKILL.md`: skills of this agent only, in the Agent Skills format.

What an agent may do in a namespace beyond the person's rights, and the work it does there by itself, that namespace gives it in its own `config` repo: `grants.yaml` and `routines.yaml` (see the README there). garrison's own are in this repo, next to this folder.
