# Home Cloud Platform

A home cloud you run by talking to it, with Git underneath. See [docs/spec.md](docs/spec.md).

| Path | Contents |
| --- | --- |
| `install.sh` | The one-command installer |
| `docs/` | Spec |
| `schemas/` | JSON Schema for stack files, namespace config, change records |
| `platform/` | Seed of the platform repo pushed into Gitea at install |
| `templates/` | Starting contents for namespace config repos and app repos |
| `controller/` | Go: applies Git to the cluster |
| `orchestrator/` | TypeScript: runs the agents |
| `ui/` | React: dashboard and team chat |
| `installer/` | Bootstrap container and setup wizard |
| `sandbox/` | Coding-agent container image |
| `host/` | Sanoid, restic, systemd units |
| `test/` | End-to-end tests on throwaway VMs |
