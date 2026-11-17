# Scenarios: talking to the agents

What people can and cannot get done by talking to a namespace's agent. Each scenario is played against the real model by the `talk` step of `test/vm/steps.sh` (`test/e2e/smoke.sh talk`), in the channel of the namespace `todo`, which holds the todo app (its code in the repo `todo/app`, its stack in `todo/config/stacks/todo.yaml`). The deploy step also deploys the namespace `homepage`: a hello-world page from `homepage/app`, at `https://homepage.<domain>`, open to everyone. The checks look at what happened (pull requests, their author, the approval check, team membership, push rights, deployments), not at what the agent said.

People: **alice** is a root admin and created `todo`; **bob** and **carol** are regular users. Who may change `todo` is in `todo/config/members.yaml`: admins approve and merge changes, members push to its repos. A change to `members.yaml` goes in only by pull request, approved by an admin of the namespace or a root admin who neither wrote it nor has their role changed by it. The gateway checks that rule and posts it as the required status `garrison/approval`.

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
- Carol approves it, then merges it: with three people in `todo`, `main` of its app repos takes only pull requests another of them approved. The pipeline tests, builds and pushes the image, and the deployer deploys it: the todo page shows *Our todos*.
- Carol's dashboard feed has bob's change through the operator; its thread shows how it was handled: carol's merge and the deploy of the merge commit.

## 4b. Bob asks the media agent for a screenshot

Alice adds the agent `todo/media` (folder `agents/media` in `todo/config`, tools `list_stacks` and `screenshot`, the shared skill `homepage-screenshots`); carol approves it. Bob: *@todo/media Take a screenshot of https://todo.<domain>/ and put it into the repo app as docs/screenshots/todo.png.*

- The gateway's browser loads the todo app signed in as bob and the agent opens a pull request on `todo/app`, as bob, holding the PNG.
- The pull request says it shows the todo app as bob, with the page title *Our todos*, not the sign-in page.

## 4c. The media agent follows its skill

Bob: *@todo/media Make the homepage screenshots of the todo app.*

- The agent loads the skill `homepage-screenshots` and follows it: two pull requests, a 1440×900 desktop screenshot and a 390×844 phone screenshot, at the paths the skill names.

## 4d. What the media agent may not do

Bob: *Take a screenshot of the todo app and put it into the namespace garrison, repo platform.* Then: *Take a screenshot of https://example.org/.*

- Bob may not change `garrison/platform`: the gateway refuses before the browser runs, and no pull request appears.
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
- Carol mentions the keeper in `garrison`'s channel, which everyone reads: the agent service refuses it there. In todo's channel, which only todo's people read, it answers her from the repo.

## 6. Alice adds an agent that may only read

Alice proposes `agents/reader` with a tool that does not exist: the approval check fails and names it. With `read_file` alone, carol approves it and alice merges it.

- `todo/reader` appears in the chat. Asked to propose a change, it cannot: the gateway serves it no `propose_change`.

## 7. The operator finds out why an app does not run

First alice gives the operator a routine, `on_alert: {alertname: ServiceFailing}`, for her; carol approves. The app `quotes` builds, but exits at start: its stack, `todo/config/stacks/quotes.yaml`, does not set `QUOTES_FILE`.

- The alert ServiceFailing fires for it. Todo's people see it in the dashboard's feed; bob, no longer in todo, does not.
- It starts the operator's routine, with nobody watching: the operator posts in todo's channel what is wrong, naming the missing setting, and its run is in the feed.

Alice: *@todo/operator The quotes app does not work. Find out why and tell me what to change.*

- The agent looks at the stack's tasks or logs, as alice, and names the missing setting.
- Once alice removes the stack, the alert resolves, on the same entry of the feed.

## 8. Alice has the operator set up a new app, from nothing

Alice, in garrison's channel: *@garrison/operator Set up a new app: a namespace photos, and in it the app gallery: traefik/whoami:v1.11 on port 80, behind the sign-in. It comes from elsewhere, so it needs no repo of its own.*

- The operator makes the namespace `photos`, as alice; within a minute it has its config, with alice as its first admin.
- It proposes `stacks/gallery.yaml` in `photos/config`, as alice, one the deployer takes. It makes no repo for it, and nothing goes into `garrison`.
- A root admin who did not write it approves it, the approval check passes, alice merges it; the deployer deploys `gallery`.
- Bob asks for a namespace of his own: Gitea refuses him, as only root admins make namespaces, and the gateway logs every try as refused.

## 9. Alice has the operator bring todo's data back

Alice: *@todo/operator I deleted todos in the todo app by mistake. Bring todo's data on main back to the newest snapshot taken before a deploy.*

- The operator lists todo's snapshots (`list_snapshots`), as alice, and proposes a restore request, `restores/<name>.yaml` in `todo/config`, naming one of them.
- Carol approves it, the approval check passes, alice merges it.
- The deployer snapshots todo's current data, stops todo, copies its database back from the snapshot and starts it again; todo answers.

## 10. An app from nothing to main

Played by the `journey` step (`test/e2e/smoke.sh journey`, `test/e2e/journey.sh`), after deploy, in a namespace of its own. Alice is its only person, so she pushes and merges in its repos freely.

1. Alice: *@garrison/operator Make a namespace lab, and in it the repo example from the template static-web.* The namespace and the repo appear, written as alice; lab gets its channel and operator; the pipeline builds main's image.
2. Alice: *@lab/operator Deploy the app example: propose stacks/example.yaml as its README says, with main and dev, both public.* A pull request on `lab/config` that the deployer takes; a root admin approves it, alice merges it in Gitea, and `https://example.<domain>` shows the page on white.
3. Alice makes the branch `dev` in Gitea. Nobody merges anything: the stack names `dev`, so the deployer deploys it at `https://dev-example.<domain>`, from the image main's build made of the same commit.
4. Alice: *@lab/operator Change the background of the page to #2e7d32 on dev.* A pull request into `dev` that changes `index.html` alone; alice merges it, its pipeline builds it, dev-example turns green, and example on main stays white.
5. Alice opens a pull request from `dev` to `main` in Gitea and merges it: example turns green, and her dashboard's feed shows lab's deploys.

## In the browser

Played by the `ui` step (`test/e2e/smoke.sh ui`, `test/ui/`): Playwright's Chromium in a container on the machine, one browser per person, everything clicked and typed in Gitea, Open WebUI and the dashboard, signed in through Keycloak's own form. The checks look at the pages, and at Gitea's and Open WebUI's APIs where a page cannot tell (an agent's answer is finished, a pull request was opened). Every step's screenshot, and every failure's, lands in `bin/ui-shots/`. Selectors of Gitea's and Open WebUI's pages live in `test/ui/lib.py` only.

1. **Signing in:** alice signs in to Gitea through Keycloak, then to Open WebUI and the dashboard without typing her password again; bob and the admin sign in too.
2. **A namespace in Gitea's form:** alice fills in New Organisation for `shop`. Within seconds Gitea lists its repo `config`, Open WebUI shows its channel in her sidebar (not in bob's), and her dashboard says it is set up.
3. **A secret:** alice saves `api-token` on the dashboard's Secrets page; the page lists its name and never its value, and `shop/config` has it encrypted.
4. **An app through the chat:** in the channel she types `@shop/operator`, picks it from the list and asks for the repo `web` from the template and its stack. Her pull request page offers no merge while the approval check waits; the admin approves on the Files changed tab; alice merges with the button; `https://web.<domain>` shows the page on white.
5. **dev:** alice makes `dev` in Gitea's branch list; `dev-web` comes up. The operator proposes a green background for dev; she merges it; dev-web turns green while web stays white. She compares dev with main, opens the pull request and merges it: web turns green.
6. **A screenshot:** the operator takes a screenshot of web and proposes it as `docs/web.png`; Gitea's Files changed tab shows the image.
7. **People and main:** the operator proposes bob as a member; the admin approves; alice merges. Bob's edit of `index.html` straight onto main in Gitea's editor is refused; he commits it to a new branch and opens a pull request, whose page offers him no merge until alice approves it; then he merges it and web shows his heading.
8. **Scheduled agents:** alice adds two routines to the operator's `agent.yaml` in Gitea's editor, on a new branch, scheduled a few minutes ahead in the installation's time zone; the admin approves; she merges. At its time the greeting is posted in shop's channel.
9. **A routine's question:** at its time the other routine asks bob whether the shop opens on Sundays. His dashboard lists it under Waiting for you; its link opens the question's thread; he answers in the question's thread; the routine goes on with his answer, in the thread, and the dashboard no longer lists it.
10. **The commander, by its short name:** in shop's channel bob types `@commander`, picks it from the list and asks which agent changes the page of web; it answers in the thread, naming `shop/operator`. It is `garrison/commander`, the same agent in every channel.
11. **A scheduled job of the app:** alice adds `.gitea/workflows/nightly.yaml` to `web` in Gitea's editor, a workflow on a cron schedule that runs on shop's runner with no agent in it, on a new branch; bob approves; she merges. Within minutes the repo's Actions tab lists a run started by the schedule, which succeeded, and its log has the job's output. She disables the workflow from the tab's menu, and it runs no more.
