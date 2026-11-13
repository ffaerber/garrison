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
