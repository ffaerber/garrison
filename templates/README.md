# Templates

What new repos start with. Compiled into the control plane (`templates.go`), so a change here reaches installations with the control plane's next build.

| Path | Written into |
| --- | --- |
| `namespace-config/` | The `config` repo of every new namespace, by the members sync. `members.yaml` is its header; the namespace's creators are added below it as the first admins. |
| `system-config/` | The `config` repo of `system`, on top of `namespace-config/` without `members.yaml`: the commander, and `llm/README.md`. `llm/providers.yaml` and `llm/models.yaml` are written by the agent service from the install's answers. |
| `app/` | New app repos, by `create_repo` with `template`: `static-web/`, a page with its Dockerfile and pipeline. |

Only new repos get these files; existing ones keep theirs and change by pull request.
