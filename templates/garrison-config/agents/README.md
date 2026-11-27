# Agents

Garrison's agents, one folder per agent. Every agent works in every unit: people mention it by its folder's name, as `@<folder>`, in a unit's channel, and there it works in that unit, with the rights of the person asking. Only root admins change this repo, so only they add or change agents.

An installation starts with these, each with only the tools its role needs:

| Agent | Role |
| --- | --- |
| `operator` | Sets up and changes units, repos and apps; every tool |
| `commander` | Finds the agent for a task and hands it over; its tools, `list_units`, `read_unit` and `list_models`, only read which units the person is in, what each is, and the models |
| `coder` | Changes code as pull requests (`propose_patch`; `merge_change` where a grant allows) and reviews pull requests; reaches the package registries |
| `sentry` | Reads state, logs and metrics, tries the apps in the browser, and explains what is wrong; takes screenshots it proposes into a repo, and changes nothing else; asks the scout about the web; fits `on_alert` routines |
| `scout` | Researches on the public web, with a browser of its own (the command `web`) and no sign-in; no access to Garrison's repos or apps |
| `designer` | Makes pictures and videos with ComfyUI in a project's styles (`generate_media`, with `media_models` for the models ComfyUI has), its shorts made of video clips only and its turns up to 30 minutes (`turn: 30m`), speaks narrations in the project's voice (`generate_speech`, with a text-to-speech service such as Kokoro), and cuts short vertical videos from them with ffmpeg, into the unit's media library; its skill `media-project` says how a project is laid out |

Change them, switch one off (`enabled: false`) or add your own.

- `agent.yaml`: `description`, `model`, `tools` (the gateway tools it may use; `all` for every one, now and later), `skills` (shared skills from `skills/` at the top of this repo), `agents` (the agents it may ask, by name, or `any`), `visible: members` (only for the people of the unit it works in) and `internet` (what its sandbox reaches: `packages`, the package registries; `all`; or a list of `packages` and domains; none without it).
- `model`: empty for Garrison's default, or one of the enabled models in `llm/models.yaml` of this repo, as `<provider>/<model>`, e.g. `openrouter/anthropic/claude-sonnet-5.5`. The agent tool `list_models` lists them too. A model that is retired or turned off later falls back to the default, and the feed says so.
- `prompt.md`: its instructions. The platform adds its own rules after them.
- `skills/<name>/SKILL.md`: skills of this agent only, in the Agent Skills format.

Several agents doing a job in order, such as software updates, is a chain of command: `chains/` at the top of this repo (see its README).

What an agent may do in a unit beyond the person's rights, and the work it does there by itself, that unit gives it in its own `config` repo: `grants.yaml` and `routines.yaml` (see the README there). garrison's own are in this repo, next to this folder.
