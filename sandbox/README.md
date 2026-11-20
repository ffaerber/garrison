# Agent sandbox

The image every chat with an agent runs in: one container per chat and agent, started by the agent service (`control-plane/internal/sandbox`) through the sandbox guard. Built with the control plane by its workflow and pushed as `garrison/sandbox`.

| File | Does |
| --- | --- |
| `Dockerfile` | Node, Pi (`@mariozechner/pi-coding-agent`, pinned in `package.json` and `package-lock.json`), bash, git, ripgrep, fd, curl, jq |
| `start.sh` | The main process: Pi in RPC mode, driven over stdin and stdout, with the agent's prompt and model, its session and workspace in the chat's volume at `/data` |
| `garrison.ts` | Pi extension: the agent's platform tools, each call run by the agent service at the gateway as the person whose turn it is; with `propose_patch`, the tool `propose_workspace_changes`, which proposes a checkout's changes as a pull request and adds later ones to it |

What a sandbox has: its own tools (read, write, edit, bash) in `/data/work`, the LLM and the platform tools through the agent service (`GARRISON_API`), named by a token that counts only during a turn, read-only Git of the repos the person may read (`git clone $GARRISON_GIT/<unit>/<repo>.git`), root under gVisor, so it can install packages, and the agent service's proxy in `HTTP_PROXY` and `HTTPS_PROXY`, which lets through what the agent's `internet` allows. What it has not: the LLM key, a Keycloak token, Docker, Gitea or any other service, or a way out but the proxy.

To try the image: `docker build -t garrison-sandbox:dev sandbox/`, then `GARRISON_SANDBOX_TEST` in `control-plane/internal/sandbox/integration_test.go`.
