# Agent sandbox

The image every chat with an agent runs in: one container per chat and agent, started by the agent service (`control-plane/internal/sandbox`) through the sandbox guard. Built with the control plane by its workflow and pushed as `system/sandbox`.

| File | Does |
| --- | --- |
| `Dockerfile` | Node, Pi (`@mariozechner/pi-coding-agent`, pinned in `package.json` and `package-lock.json`), bash, git, ripgrep, fd, curl, jq |
| `start.sh` | The main process: Pi in RPC mode, driven over stdin and stdout, with the agent's prompt and model, its session and workspace in the chat's volume at `/data` |
| `homecloud.ts` | Pi extension: the agent's platform tools, each call run by the agent service at the gateway as the person whose turn it is |

What a sandbox has: its own tools (read, write, edit, bash) in `/data/work`, the LLM and the platform tools through the agent service (`HOMECLOUD_API`), named by a token that counts only during a turn. What it has not: the LLM key, a Keycloak token, Docker, the internet, Gitea or any other service.

To try the image: `docker build -t homecloud-sandbox:dev sandbox/`, then `HOMECLOUD_SANDBOX_TEST` in `control-plane/internal/sandbox/integration_test.go`.
