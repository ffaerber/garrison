# Templates

What new repos start with. Compiled into the control plane (`templates.go`), so a change here reaches installations with the control plane's next build.

| Path | Written into |
| --- | --- |
| `namespace-config/` | The `config` repo of every new namespace, by the members sync: its README (members, stacks, and the grants and routines it gives the agents) and `members.yaml`, whose header it is; the namespace's creators are added below it as the first admins. |
| `garrison-config/` | The `config` repo of `garrison`, the platform's namespace: the agents, which work in every namespace (the operator and the commander), and `llm/README.md`. `llm/providers.yaml` and `llm/models.yaml` are written by the agent service from the install's answers. |
| `app/` | New app repos, by `create_repo` with `template`: `static-web/`, a page with its Dockerfile and pipeline. |

Only new repos get these files; existing ones keep theirs and change by pull request.
