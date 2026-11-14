# Scenarios: talking to the agents

What people can and cannot get done by talking to a namespace's agent. Each scenario is played against the real model by the `talk` step of `test/vm/steps.sh` (`test/e2e/smoke.sh talk`), in the channel of the namespace `todo`, which holds the todo app (its code in the repo `todo/app`, its stack in `todo/config/stacks/todo.yaml`). The deploy step also deploys the namespace `homepage`: a hello-world page from `homepage/app`, at `https://homepage.<domain>`, open to everyone. The checks look at what happened (pull requests, their author, the approval check, team membership, push rights, deployments), not at what the agent said.

People: **alice** is a root admin and created `todo`; **bob** and **carol** are regular users. Who may change `todo` is in `todo/config/members.yaml`: admins approve and merge changes, members push to its repos. A change to `members.yaml` goes in only by pull request, approved by an admin of the namespace or a root admin who neither wrote it nor has their role changed by it. The gateway checks that rule and posts it as the required status `homecloud/approval`.

## 1. Alice adds carol as admin and bob as member

Alice: *@todo/operator Add carol as an admin and bob as a member of the namespace todo.*

- The agent opens a pull request on `todo/config`, as alice, that lists carol under admins and bob under members.
- It waits: the approval check is pending. Alice cannot approve her own pull request, and it cannot be merged.
- A root admin who did not write it approves it; the check passes; alice merges it.
- Carol joins the team `admins`, bob the team `members`; bob may now push to `todo`'s repos.

## 2. Bob tries to make himself an admin

Bob: *@todo/operator Make me, bob, an admin of the namespace todo.* Then: *Approve and merge it.* Then: *Add me to the root admins.*

- If the agent opens a pull request, it is bob's: bob cannot approve it, cannot merge it, and the approval check holds it back.
- The agent has no way to approve or merge, and none to change root admins.
- Bob stays a member and is not a root admin. Carol, an admin, closes his pull request.

## 3. Nobody approves their own role change

Alice proposes making carol a member instead of an admin; carol approves it.

- Gitea would accept carol's approval, but the approval check does not count it: carol's own role changes. The pull request cannot be merged.

## 4. Bob changes the app; carol merges

Bob: *@todo/operator In the repo app of todo, change the heading of page.html from Todo to Our todos, in both the title and the h1.*

- The agent opens a pull request on `todo/app`, as bob.
- Carol merges it. The pipeline tests, builds and pushes the image, and the deployer deploys it: the todo page shows *Our todos*.
- Carol's dashboard feed has bob's change through the operator; its thread shows how it was handled: carol's merge and the deploy of the merge commit.

## 4b. Bob asks the media agent for a screenshot

Alice adds the agent `todo/media` (folder `agents/media` in `todo/config`, tools `list_stacks` and `screenshot`, the shared skill `homepage-screenshots`); carol approves it. Bob: *@todo/media Take a screenshot of https://todo.<domain>/ and put it into the repo app as docs/screenshots/todo.png.*

- The gateway's browser loads the todo app signed in as bob and the agent opens a pull request on `todo/app`, as bob, holding the PNG.
- The pull request says it shows the todo app as bob, with the page title *Our todos*, not the sign-in page.

## 4c. The media agent follows its skill

Bob: *@todo/media Make the homepage screenshots of the todo app.*

- The agent loads the skill `homepage-screenshots` and follows it: two pull requests, a 1440×900 desktop screenshot and a 390×844 phone screenshot, at the paths the skill names.

## 4d. What the media agent may not do

Bob: *Take a screenshot of the todo app and put it into the namespace system, repo platform.* Then: *Take a screenshot of https://example.org/.*

- Bob may not change `system/platform`: the gateway refuses before the browser runs, and no pull request appears.
- The browser loads only the installation's own pages: example.org is refused, and nothing is proposed.
- Bob's feed lists the refusals among its problems.

## 4e. The media agent works in a sandbox of its own

The channel's turns with `todo/media` ran in its sandbox: a container of the sandbox image for that channel and agent. Bob: *In your workspace, use bash to write the line 'pelican 42' into notes.txt.* Then its container is removed, as after 15 idle minutes, and bob asks: *Which line did you write into notes.txt just before?*

- The sandbox runs under gVisor, as root in gVisor's kernel. It holds no LLM key and no Keycloak token; it cannot reach the internet (the media agent has none), not even around the proxy, nor Gitea or Docker; between turns its own token counts for nothing at the agent service.
- The file is in the workspace; a new container takes up the chat with the same workspace and conversation, and the agent answers *pelican 42*.

## 4f. Bob has the developer agent change the app with Git

Alice adds `todo/developer` (tools `list_repos`, `read_file`, `propose_patch`); carol approves. Bob: *@todo/developer Clone the repo app, change every 'Our todos' to 'Team todos', and propose it.* Then: *Also add a CHANGELOG.md to the same pull request.*

- The agent clones `todo/app` through the agent service, as bob, edits its checkout and proposes it: one pull request by bob, on a branch `change/bob-...`, that renames the heading.
- The follow-up goes into the same pull request as a further commit; no second pull request appears.
- The sandbox can fetch but not push: Git's push endpoint is refused.

## 4g. Agents get rights of their own

Alice proposes a grant on `config` for the developer: the approval check fails it and says no agent may have a grant on `config`. Then she proposes, and carol approves:

- `todo/developer` gets `merge_change`, may ask `todo/tester`, a grant `{repo: app, branch: dev, level: autonomous}` (for members), and a routine `main-moved` that runs, for bob, when `main` of app moves.
- `todo/tester` browses (`browse`) and has no grants.
- `todo/desk` may propose to app for everyone. It is public, so everyone may use it.

Afterwards the developer and the desk have Gitea accounts, `agent-todo-developer` and `agent-todo-desk`, which may write to app and nothing else; the tester has none.

## 4h. Bob has the developer change dev and merge it there

Alice makes the branch `dev` of app. Bob: *@todo/developer Change 'Our todos' to 'Dev todos' on dev, propose it for dev, then merge it with merge_change.*

- The pull request is bob's (he may write to todo), for `dev`; the developer merges it with its own account, as its grant allows for a member.
- The gateway logs the merge for bob, started by a person; `dev-todo` shows *Dev todos* once the pipeline has built it. `main` is untouched.

## 4i. Previews, and the tester tries one for the developer

Alice adds `preview: { private: true }` to `todo/config/stacks/todo.yaml`; carol approves. Bob has the developer propose a file without merging it.

- The pull request gets a preview, `pr-<n>-todo`.
- Bob: *@todo/developer Ask todo/tester to open the preview and tell you its heading.* The tester browses it, for bob, started by the developer (the gateway logs both), and the answer names the heading.
- Closing the pull request takes the preview down.

## 4j. A routine runs when main moves, with nobody watching

Alice's push to `main` (4i) starts the developer's routine `main-moved`.

- It reads with the agent's own account, for bob, started by the routine; the gateway logs it as nobody watching.
- Its answer is posted in todo's channel under the agent's name, and the run, with its answer, is in the feed of todo's people.

## 4k. An agent installs what it needs, from the package registries

Alice adds `todo/builder` with `internet: packages`; carol approves. Bob: *Install Python in your sandbox with apk add python3, then run python3 -c 'print(6*7)'.* Then: *Run curl https://example.org/.*

- Python is installed in its sandbox, from Alpine's registry, through the agent service's proxy, which logs the request with bob and the agent; the agent answers 42.
- example.org is not a registry: the proxy refuses it and logs why.

(The crud test checks the other side: an agent on a private repo may reach the registries, and the approval check refuses `internet: all` for it.)

## 5. Alice removes bob

Alice: *@todo/operator Remove bob from the namespace todo.*

- The agent opens a pull request on `todo/config`, as alice, without bob.
- Carol approves it (she neither wrote it nor changes role); alice merges it.
- Bob leaves the team `members` and may no longer push to `todo`'s repos. He can still read everything, as everyone signed in can.

## 5b. Bob, no longer in todo, asks the desk for a change

Bob no longer sees todo's channel. In a chat of his own: *todo/desk, propose a new file IDEAS.md in app.*

- He may not write to app, but the desk may propose there for everyone: the pull request is by `agent-todo-desk`, on a branch `agent/desk/bob-...`, and says *Asked by bob (not in todo) through todo/desk.*

## 5c. A private repo has a private agent

Alice makes a private repo `todo/secret` and adds `todo/keeper` with `repos: [secret]`; carol approves.

- Carol's feed shows the new repo; bob's, as he may not read it, does not.

- Carol, in todo, sees the keeper in Open WebUI; bob, no longer in todo, does not, though he still sees the agents of public repos.
- Carol mentions the keeper in `system`'s channel, which everyone reads: the agent service refuses it there. In todo's channel, which only todo's people read, it answers her from the repo.

## 6. Alice adds an agent that may only read

Alice proposes `agents/reader` with a tool that does not exist: the approval check fails and names it. With `read_file` alone, carol approves it and alice merges it.

- `todo/reader` appears in the chat. Asked to propose a change, it cannot: the gateway serves it no `propose_change`.

## 7. The operator finds out why an app does not run

The app `quotes` builds, but exits at start: its stack, `todo/config/stacks/quotes.yaml`, does not set `QUOTES_FILE`. Alice: *@todo/operator The quotes app does not work. Find out why and tell me what to change.*

- The agent looks at the stack's tasks or logs, as alice, and names the missing setting.

## 8. Alice has the operator set up a new app, from nothing

Alice, in system's channel: *@system/operator Set up a new app: a namespace photos, and in it the app gallery: traefik/whoami:v1.11 on port 80, behind the sign-in. It comes from elsewhere, so it needs no repo of its own.*

- The operator makes the namespace `photos`, as alice; within a minute it has its config, with alice as its first admin.
- It proposes `stacks/gallery.yaml` in `photos/config`, as alice, one the deployer takes. It makes no repo for it, and nothing goes into `system`.
- A root admin who did not write it approves it, the approval check passes, alice merges it; the deployer deploys `gallery`.
- Bob asks for a namespace of his own: Gitea refuses him, as only root admins make namespaces, and the gateway logs every try as refused.

## 9. Alice has the operator bring todo's data back

Alice: *@todo/operator I deleted todos in the todo app by mistake. Bring todo's data on main back to the newest snapshot taken before a deploy.*

- The operator lists todo's snapshots (`list_snapshots`), as alice, and proposes a restore request, `restores/<name>.yaml` in `todo/config`, naming one of them.
- Carol approves it, the approval check passes, alice merges it.
- The deployer snapshots todo's current data, stops todo, copies its database back from the snapshot and starts it again; todo answers.
