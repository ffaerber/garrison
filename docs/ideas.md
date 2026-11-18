# Ideas

Things worth building later, written down so they are not lost. Not planned yet; what is planned is in [status.md](status.md), what cannot be fixed in [unfixable.md](unfixable.md).

## Apps of private repos only for their namespace's people

**Problem:** an environment with `private: true` in `stack.yaml` sits behind the sign-in (`sso@file`, oauth2-proxy with Keycloak), which only asks who someone is. So the app of a private repo, its `dev-<stack>` and its previews `pr-<n>-<stack>` can be opened by everyone with an account, though only the namespace's people may read the repo.

**Idea:** a second check after the sign-in, which knows the namespace. Automatic, no setup per repo:

1. The deployer, which reads each repo from Gitea anyway, adds to every environment of a private repo a second forward-auth middleware after `sso@file`, pointing at the gateway: `http://control-plane_gateway:8080/access/<namespace>`. Making a repo private or public changes it at the next sync.
2. The gateway lets in the people of the namespace's `members.yaml` and root admins, as for private agents (`AgentPrivate`), reading `members.yaml` on each check (cached for seconds), so a change to it applies at once. Everyone else gets a 403 page that says whom to ask.
3. Previews and `dev-` get it too, being environments of the same repo. Agents' `screenshot` and `browse` keep working: the check sees the person behind the agent's token.

**To watch:**
- The check must trust only the username oauth2-proxy returned (Traefik's `authResponseHeaders`), never one a visitor sends; the router removes those headers from incoming requests.
- The gateway becomes part of the path to every private app: when it is down, those apps are refused, never opened.

**Maybe:** `private: team` in `stack.yaml`, for a public repo whose app should still be for its people only.

Basic auth in front of such apps was considered: SSO covers the same need, without passwords to hand around.

## A front end of our own, with Gitea's web UI only for root admins

**Problem:** people use Gitea's web UI directly, where they can click around in settings and delete things (see [unfixable.md](unfixable.md)). Garrison has no front end of its own that shows it as one thing: namespaces, repos, apps, agents.

**Idea:** the dashboard the spec plans becomes the main front end, and Gitea's web UI is for root admins only.

- **Gitea's UI closed to others:** Gitea cannot switch its UI off, but Traefik can put a check in front of the web pages of `git.<domain>` that lets only root admins in, keeping Git over HTTP (clone, fetch), the registry and the sign-in paths open. The control plane reaches Gitea internally anyway.
- **Namespaces and repos:** a list of namespaces, their repos, a repo's files and history, its pull requests and its apps and previews, all read and changed as the signed-in person through the gateway, which already has these as tools; it would need a plain API for a web page next to its MCP endpoint.
- **Agents in the page:** "start agent" on a namespace or a repo opens a chat with its agent, or a coding agent with the repo already set, talking to the agent service directly (it speaks the OpenAI streaming API that Open WebUI uses). Sandboxes, workspaces and pull requests stay as they are.
- **Routines:** per agent a list of its routines (schedule, owner, last run and its answer), with a button to run one now. Editing one opens a pull request on `agent.yaml`, so Git stays the truth and changes stay reviewed.
- **Inbox:** pull requests to approve, previews to look at, failed deploys and pipelines, as the spec describes.

**Before Gitea's UI can close,** the front end must cover what people do there: pull requests (diff, approve, merge, including the approvals of `members.yaml`), files and history, and pipeline status and logs.

**Order:** a read-only overview with the inbox; pull requests; the agent chat with "start agent"; routines; then close Gitea's UI. Whether Open WebUI stays for team channels and free chat is decided later.

## Metrics and logs by namespace and repo

**Problem:** Prometheus (through cAdvisor) and Loki have every container's metrics and logs, but know them only by Swarm stack and service, e.g. `pr-7-todo` and `pr-7-todo_web`. Which namespace, repo and branch a stack belongs to only the deployer knows: the services it renders carry Traefik labels and nothing more.

**Idea:**

1. The deployer labels every container it starts with `garrison.namespace`, `garrison.repo`, `garrison.environment` (`main`, `dev`, `pr-7`) and `garrison.commit`; the `garrison` stacks too, with namespace `garrison` and the stack as repo.
2. cAdvisor passes these labels on, so Prometheus has them as they are; one rule in Alloy makes them Loki labels, as `stack` and `service` are today.
3. Then everything of a namespace, of a repo or of its previews is one query. Grafana's Stack dashboard can offer namespace, repo and environment instead of the stack's name; the front end (above) shows a namespace's apps (CPU, memory, errors, restarts) on its page, and a repo's per environment, with their logs next to its pipeline logs and its agents' activity.

**To watch:** everyone signed in reads everything in Grafana, the logs of private repos' apps too. The front end would read metrics and logs through the gateway as the person, and show a private repo's only to its namespace's people; Grafana itself would need the same, e.g. data per namespace, together with the idea on private repos' apps above.
