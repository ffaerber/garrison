# Home Cloud Platform

A home cloud you run by talking to it, with Git underneath. See [docs/spec.md](docs/spec.md) for the design and [docs/status.md](docs/status.md) for what is built.

| Path | Contents |
| --- | --- |
| `install.sh` | The one-command installer |
| `docs/` | Spec |
| `schemas/` | JSON Schema for stack files, namespace config, change records |
| `system/` | One directory per repo of the `system` organisation in Gitea, i.e. per system stack; pushed at install |
| `templates/` | Starting contents for namespace config repos and app repos |
| `controller/` | Go: applies Git to the cluster. Built into an image by Gitea Actions from `system/controller` |
| `orchestrator/` | TypeScript: runs the agents |
| `ui/` | React: dashboard and team chat |
| `installer/` | Bootstrap helpers: pushing repos, assembling the controller repo |
| `sandbox/` | Coding-agent container image |
| `host/` | Sanoid, restic, systemd units |
| `test/` | End-to-end tests on throwaway VMs |
