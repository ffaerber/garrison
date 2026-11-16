# Garrison

A home cloud you run by talking to it, with Git underneath. See [docs/spec.md](docs/spec.md) for the design and [docs/status.md](docs/status.md) for what is built.

| Path | Contents |
| --- | --- |
| `install.sh` | The one-command installer |
| `docs/` | Spec |
| `schemas/` | JSON Schema for stack files, namespace config, change records |
| `system/` | One directory per repo of the `system` organisation in Gitea, i.e. per system stack; pushed at install |
| `templates/` | Starting contents of new config repos: the operator, the commander, the model list's README. Compiled into the control plane |
| `control-plane/` | Go: the deployer (applies Git to the cluster), the gateway (MCP server for the chat agents, namespace members), the agent service (runs each chat in a sandbox) and the guards. Built into one image by Gitea Actions from `system/control-plane` |
| `ui/` | React: dashboard and team chat |
| `installer/` | Bootstrap helpers: pushing repos, assembling the control plane repo |
| `sandbox/` | The agents' sandbox image: Pi, the agent harness, and its tools |
| `host/` | Sanoid, restic, systemd units |
| `test/` | End-to-end tests on throwaway VMs |
