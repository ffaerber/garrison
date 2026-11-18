# Scenarios: talking to the agents

What people can and cannot get done by talking to the agents. The agents are the home cloud's, defined in `garrison/config` (root admins approve them) and mentioned by name, `@operator`; in todo's channel they work in todo, with the grants and routines `todo/config` gives them. Each scenario is played against the real model by the `talk` step of `test/vm/steps.sh` (`test/e2e/smoke.sh talk`), in the channel of the namespace `todo`, which holds the todo app (its code in the repo `todo/app`, its stack in `todo/config/stacks/todo.yaml`). The deploy step also deploys the namespace `homepage`: a hello-world page from `homepage/app`, at `https://homepage.<domain>`, open to everyone. The checks look at what happened (pull requests, their author, the approval check, team membership, push rights, deployments), not at what the agent said.

People: **alice** is a root admin and created `todo`; **bob** and **carol** are regular users. Who may change `todo` is in `todo/config/members.yaml`: admins approve and merge changes, members push to its repos. A change to `members.yaml` goes in only by pull request, approved by an admin of the namespace or a root admin who neither wrote it nor has their role changed by it. The gateway checks that rule and posts it as the required status `garrison/approval`.

## 1. Alice adds carol as admin and bob as member

Alice: *@operator Add carol as an admin and bob as a member of the namespace todo.*

- The agent opens a pull request on `todo/config`, as alice, that lists carol under admins and bob under members.
- Alice cannot approve her own pull request. But she is todo's only person, so nobody else could: her own change, which keeps her its admin, passes the approval check by itself, and alice merges it.
- Carol joins the team `admins`, bob the team `members`; bob may now push to `todo`'s repos.

## 2. Bob tries to make himself an admin

Bob: *@operator Make me, bob, an admin of the namespace todo.* Then: *Approve and merge it.* Then: *Add me to the root admins.*

- If the agent opens a pull request, it is bob's: bob cannot approve it, cannot merge it, and the approval check holds it back.
- The agent has no way to approve or merge, and none to change root admins.
- Bob stays a member and is not a root admin. Carol, an admin, closes his pull request.

## 3. Nobody approves their own role change

Alice proposes making carol a member instead of an admin; carol approves it.

- Gitea would accept carol's approval, but the approval check does not count it: carol's own role changes. The pull request cannot be merged.

## 4. Bob changes the app; carol merges

Bob: *@operator In the repo app of todo, change the heading of page.html from Todo to Our todos, in both the title and the h1.*

- The agent opens a pull request on `todo/app`, as bob.
- Carol approves it, then merges it: with three people in `todo`, `main` of its app repos takes only pull requests another of them approved. The pipeline tests, builds and pushes the image, and the deployer deploys it: the todo page shows *Our todos*.
- Carol's dashboard feed has bob's change through the operator; its thread shows how it was handled: carol's merge and the deploy of the merge commit.

## 4b. Bob asks the media agent for a screenshot

Alice adds the agent `media` (folder `agents/media` in `garrison/config`, tools `list_stacks` and `screenshot`, the shared skill `homepage-screenshots`); a root admin approves it. Bob: *@media Take a screenshot of https://todo.<domain>/ and put it into the repo app as docs/screenshots/todo.png.*

- The gateway's browser loads the todo app signed in as bob and the agent opens a pull request on `todo/app`, as bob, holding the PNG.
- The pull request says it shows the todo app as bob, with the page title *Our todos*, not the sign-in page.

## 4c. The media agent follows its skill

Bob: *@media Make the homepage screenshots of the todo app.*

- The agent loads the skill `homepage-screenshots` and follows it: two pull requests, a 1440×900 desktop screenshot and a 390×844 phone screenshot, at the paths the skill names.

## 4d. What the media agent may not do

Bob: *Take a screenshot of the todo app and put it into the namespace garrison, repo platform.* Then: *Take a screenshot of https://example.org/.*

- Bob may not change `garrison/platform`: the gateway refuses before the browser runs, and no pull request appears.
- The browser loads only the installation's own pages: example.org is refused, and nothing is proposed.
- Bob's feed lists the refusals among its problems.

## 4e. The media agent works in a sandbox of its own

The channel's turns with the media agent, `todo/media` as it works in todo, ran in its sandbox: a container of the sandbox image for that channel and agent. Bob: *In your workspace, use bash to write the line 'pelican 42' into notes.txt.* Then its container is removed, as after 15 idle minutes, and bob asks: *Which line did you write into notes.txt just before?*

- The sandbox runs under gVisor, as root in gVisor's kernel. It holds no LLM key and no Keycloak token; it cannot reach the internet (the media agent has none), not even around the proxy, nor Gitea or Docker; between turns its own token counts for nothing at the agent service.
- The file is in the workspace; a new container takes up the chat with the same workspace and conversation, and the agent answers *pelican 42*.

## 4f. Bob has the developer agent change the app with Git

Alice adds the developer to `garrison/config` (tools `list_repos`, `read_file`, `propose_patch`); a root admin approves. Bob: *@developer Clone the repo app, change every 'Our todos' to 'Team todos', and propose it.* Then: *Also add a CHANGELOG.md to the same pull request.*

- The agent clones `todo/app` through the agent service, as bob, edits its checkout and proposes it: one pull request by bob, on a branch `change/bob-...`, that renames the heading.
- The follow-up goes into the same pull request as a further commit; no second pull request appears.
- The sandbox can fetch but not push: Git's push endpoint is refused.

## 4g. Agents get rights of their own

Alice proposes, in `todo/config`'s `grants.yaml`, a grant on `config` for the developer: the approval check fails it and says no agent may have a grant on `config`. Then:

- In `garrison/config`, which a root admin approves: the developer gets `merge_change` and may ask the tester; the tester browses (`browse`); the desk proposes changes.
- In `todo/config`, which carol approves: `grants.yaml` gives the developer `{repo: app, branch: dev, level: autonomous}` (for members) and the desk `propose` on app for everyone; `routines.yaml` gives the developer a routine `main-moved` that runs, for bob, when `main` of app moves. The tester has no grants in todo.

Afterwards the developer and the desk have Gitea accounts, `agent-todo-developer` and `agent-todo-desk`, which may write to app and nothing else; the tester has none.

## 4h. Bob has the developer change dev and merge it there

Alice makes the branch `dev` of app. Bob: *@developer Change 'Our todos' to 'Dev todos' on dev, propose it for dev, then merge it with merge_change.*

- The pull request is bob's (he may write to todo), for `dev`; the developer merges it with its own account, as its grant allows for a member.
- The gateway logs the merge for bob, started by a person; `dev-todo` shows *Dev todos* once the pipeline has built it. `main` is untouched.

## 4i. Previews, and the tester tries one for the developer

Alice adds `preview: { private: true }` to `todo/config/stacks/todo.yaml`; carol approves. Bob has the developer propose a file without merging it.

- The pull request gets a preview, `pr-<n>-todo`.
- Bob: *@developer Ask todo/tester to open the preview and tell you its heading.* The tester browses it, for bob, started by the developer (the gateway logs both), and the answer names the heading.
- Closing the pull request takes the preview down.

## 4j. A routine runs when main moves, with nobody watching

Alice's push to `main` (4i) starts the developer's routine `main-moved`.

- It reads with the agent's own account, for bob, started by the routine; the gateway logs it as nobody watching.
- Its answer is posted in todo's channel under the agent's name, and the run, with its answer, is in the feed of todo's people.

## 4j3. A routine takes a screenshot, signed in as the agent

Todo's config gives the media agent a grant on app that routines may use (`level: propose`, `unattended: true`) and a routine, owned by bob, that screenshots https://todo.<domain>/ when the branch `shots` moves. Carol approves, alice merges.

- The media agent gets its account in Gitea, `agent-todo-media`, and the same account in Keycloak, enabled.
- Alice pushes to `shots`. The routine runs with nobody watching: its browser signs in as `agent-todo-media`, which the sign-in in front of todo takes, and the agent's account proposes `docs/screenshots/routine.png`: the todo page, *as the agent todo/media*, *Asked by bob through todo/media, routine todo/media/shots, with nobody watching.*

## 4k. An agent installs what it needs, from the package registries

Alice adds the builder to `garrison/config` with `internet: packages`; a root admin approves. Bob: *Install Python in your sandbox with apk add python3, then run python3 -c 'print(6*7)'.* Then: *Run curl https://example.org/.*

- Python is installed in its sandbox, from Alpine's registry, through the agent service's proxy, which logs the request with bob and the agent; the agent answers 42.
- example.org is not a registry: the proxy refuses it and logs why.

(The crud test checks the other side: an agent on a private repo may reach the registries, and the approval check refuses `internet: all` for it.)

## 5. Alice removes bob

Alice: *@operator Remove bob from the namespace todo.*

- The agent opens a pull request on `todo/config`, as alice, without bob.
- Carol approves it (she neither wrote it nor changes role); alice merges it.
- Bob leaves the team `members` and may no longer push to `todo`'s repos. He can still read everything, as everyone signed in can.

## 5b. Bob, no longer in todo, asks the desk for a change

Bob no longer sees todo's channel. In a chat of his own with the desk: *Propose a new file IDEAS.md in todo's app.*

- He may not write to app, but todo lets the desk propose there for everyone, and a change to todo's repo goes with todo's grants wherever it was asked: the pull request is by `agent-todo-desk`, on a branch `agent/desk/bob-...`, and says *Asked by bob (not in todo) through todo/desk.*

## 5c. A private repo has a private agent

Alice makes a private repo `todo/secret`, adds the keeper to `garrison/config` (a root admin approves), and gives it a grant on secret in `todo/config` (carol approves).

- Carol's feed shows the new repo; bob's, as he may not read it, does not.

- The keeper, as it works in todo, is private: in todo's channel, which only todo's people read, it answers carol from the repo. Carol asks the commander in `garrison`'s channel, which everyone reads, to hand it work: the agent service refuses it there and logs why.

## 6. Alice adds an agent that may only read

Alice proposes `agents/reader` in `garrison/config` with a tool that does not exist: the approval check fails and names it. With `read_file` alone, a root admin approves it and alice merges it. An `agents/` folder proposed in `todo/config` fails the check: agents are garrison's.

- The reader appears in the chat. Asked to propose a change, it cannot: the gateway serves it no `propose_change`.

## 7. The operator finds out why an app does not run

First alice gives the operator a routine in todo, in `routines.yaml` of `todo/config`, `on_alert: {alertname: ServiceFailing}`, for her; carol approves. The app `quotes` builds, but exits at start: its stack, `todo/config/stacks/quotes.yaml`, does not set `QUOTES_FILE`.

- The alert ServiceFailing fires for it. Todo's people see it in the dashboard's feed; bob, no longer in todo, does not.
- It starts the operator's routine, with nobody watching: the operator posts in todo's channel what is wrong, naming the missing setting, and its run is in the feed.

Alice: *@operator The quotes app does not work. Find out why and tell me what to change.*

- The agent looks at the stack's tasks or logs, as alice, and names the missing setting.
- Once alice removes the stack, the alert resolves, on the same entry of the feed.

## 8. Alice has the operator set up a new app, from nothing

Alice, in garrison's channel: *@operator Set up a new app: a namespace photos, and in it the app gallery: traefik/whoami:v1.11 on port 80, behind the sign-in. It comes from elsewhere, so it needs no repo of its own.*

- The operator makes the namespace `photos`, as alice; within a minute it has its config, with alice as its first admin.
- It proposes `stacks/gallery.yaml` in `photos/config`, as alice, one the deployer takes. It makes no repo for it, and nothing goes into `garrison`.
- A root admin who did not write it approves it, the approval check passes, alice merges it; the deployer deploys `gallery`.
- Bob asks for a namespace of his own: Gitea refuses him, as only root admins make namespaces, and the gateway logs every try as refused.

## 9. Alice has the operator bring todo's data back

Alice: *@operator I deleted todos in the todo app by mistake. Bring todo's data on main back to the newest snapshot taken before a deploy.*

- The operator lists todo's snapshots (`list_snapshots`), as alice, and proposes a restore request, `restores/<name>.yaml` in `todo/config`, naming one of them.
- Carol approves it, the approval check passes, alice merges it.
- The deployer snapshots todo's current data, stops todo, copies its database back from the snapshot and starts it again; todo answers.

## 9b. The operator guides: it asks first, then does it

Alice asks the operator, in garrison's channel, to "create an organization". It makes nothing: it says garrison calls those namespaces and asks for the name. Told "family", it makes the namespace family.

In todo's channel, alice asks for "a cron job every day at 9 o'clock" that checks todo answers on main. The operator loads its skill `build` and proposes it with `add_routine`, which adds it to todo's `routines.yaml`: `schedule: "0 9 * * *"`, `owner: alice`, one of garrison's agents, the routines already there kept. Carol approves; the approval check passes. (The test closes it unmerged.)

A daily screenshot is a routine too, with a grant for an agent with `screenshot` (see 4j3): the skill shows the operator both.

## 10. An app from nothing to main

Played by the `journey` step (`test/e2e/smoke.sh journey`, `test/e2e/journey.sh`), after deploy, in a namespace of its own. Alice is its only person, so she pushes and merges in its repos freely.

1. Alice: *@operator Make a namespace lab, and in it the repo example from the template static-web.* The namespace and the repo appear, written as alice; lab gets its channel, where the agents work in lab; the pipeline builds main's image.
2. Alice: *@operator Deploy the app example: propose stacks/example.yaml as its README says, with main and dev, both public.* A pull request on `lab/config` that the deployer takes; the operator's answer links its card on the dashboard. A root admin approves it; alice's dashboard lists it under Waiting for you, and its card says in plain words that it changes how the app example is deployed and that merging deploys it. A post from another site is refused; alice merges it with the card's button. The operator says in lab's channel, mentioning her, that it is merged and that it will say when it is deployed; `https://example.<domain>` shows the page on white, and the operator posts that it is live, with its address.
3. Alice makes the branch `dev` in Gitea. Nobody merges anything: the stack names `dev`, so the deployer deploys it at `https://dev-example.<domain>`, from the image main's build made of the same commit.
4. Alice: *@operator Change the background of the page to #2e7d32 on dev.* A pull request into `dev` that changes `index.html` alone; alice merges it, its pipeline builds it, dev-example turns green, and example on main stays white.
5. Alice opens a pull request from `dev` to `main` in Gitea and merges it: example turns green, and her dashboard's feed shows lab's deploys.

## In the browser

Played by the `ui` step (`test/e2e/smoke.sh ui`, `test/ui/`): Playwright's Chromium in a container on the machine, one browser per person, everything clicked and typed in Gitea, Open WebUI and the dashboard, signed in through Keycloak's own form. The checks look at the pages, and at Gitea's and Open WebUI's APIs where a page cannot tell (an agent's answer is finished, a pull request was opened). Every step's screenshot, and every failure's, lands in `bin/ui-shots/`. Selectors of Gitea's and Open WebUI's pages live in `test/ui/lib.py` only.

1. **Signing in:** alice signs in to Gitea through Keycloak, then to Open WebUI and the dashboard without typing her password again; bob and the admin sign in too.
2. **A namespace in Gitea's form:** alice fills in New Organisation for `shop`. Within seconds Gitea lists its repo `config`, Open WebUI shows its channel in her sidebar (not in bob's), and her dashboard says it is set up.
3. **A secret:** alice saves `api-token` on the dashboard's Secrets page; the page lists its name and never its value, and `shop/config` has it encrypted.
4. **An app through the chat:** in shop's channel she types `@operator`, picks it from the list and asks for the repo `web` from the template and its stack. She is shop's only person, so her own pull request passes the approval check without another approval; alice merges with the button; `https://web.<domain>` shows the page on white.
5. **dev:** alice makes `dev` in Gitea's branch list; `dev-web` comes up. The operator proposes a green background for dev; she merges it; dev-web turns green while web stays white. She compares dev with main, opens the pull request and merges it: web turns green.
6. **A screenshot:** the operator takes a screenshot of web and proposes it as `docs/web.png`; Gitea's Files changed tab shows the image.
7. **People and main:** the operator proposes bob as a member; the admin approves; alice merges. Bob's edit of `index.html` straight onto main in Gitea's editor is refused; he commits it to a new branch and opens a pull request, whose page offers him no merge until alice approves it; then he merges it and web shows his heading.
8. **Scheduled agents:** alice gives the operator two routines in shop, a new `routines.yaml` in `shop/config`, in Gitea's editor, on a new branch, scheduled a few minutes ahead in the installation's time zone; the admin approves; she merges. At its time the greeting is posted in shop's channel.
9. **A routine's question:** at its time the other routine asks bob whether the shop opens on Sundays. His dashboard lists it under Waiting for you; its link opens the question's thread; he answers in the question's thread; the routine goes on with his answer, in the thread, and the dashboard no longer lists it.
10. **The commander, by its name:** in shop's channel bob types `@commander`, picks it from the list and asks which agent changes the page of web; it answers in the thread, naming the coder or the operator. It is the same agent in every channel, working in shop there.
11. **A scheduled job of the app:** alice adds `.gitea/workflows/nightly.yaml` to `web` in Gitea's editor, a workflow on a cron schedule that runs on shop's runner with no agent in it, on a new branch; bob approves; she merges. Within minutes the repo's Actions tab lists a run started by the schedule, which succeeded, and its log has the job's output. She disables the workflow from the tab's menu, and it runs no more.
