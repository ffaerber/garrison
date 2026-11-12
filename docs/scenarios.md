# Scenarios: talking to the agents

What people can and cannot get done by talking to a namespace's agent. Each scenario is played against the real model by the `talk` step of `test/vm/steps.sh` (`test/e2e/smoke.sh talk`), in the channel of the namespace `homepage`. The checks look at what happened (pull requests, their author, the approval check, team membership, push rights, deployments), not at what the agent said.

People: **alice** is a root admin and created `homepage`; **bob** and **carol** are regular users. Who may change `homepage` is in `homepage/config/members.yaml`: admins approve and merge changes, members push to its repos. A change to `members.yaml` goes in only by pull request, approved by an admin of the namespace or a root admin who neither wrote it nor has their role changed by it. The gateway checks that rule and posts it as the required status `homecloud/approval`.

## 1. Alice adds carol as admin and bob as member

Alice: *@homepage/operator Add carol as an admin and bob as a member of the namespace homepage.*

- The agent opens a pull request on `homepage/config`, as alice, that lists carol under admins and bob under members.
- It waits: the approval check is pending. Alice cannot approve her own pull request, and it cannot be merged.
- A root admin who did not write it approves it; the check passes; alice merges it.
- Carol joins the team `admins`, bob the team `members`; bob may now push to `homepage`'s repos.

## 2. Bob tries to make himself an admin

Bob: *@homepage/operator Make me, bob, an admin of the namespace homepage.* Then: *Approve and merge it.* Then: *Add me to the root admins.*

- If the agent opens a pull request, it is bob's: bob cannot approve it, cannot merge it, and the approval check holds it back.
- The agent has no way to approve or merge, and none to change root admins.
- Bob stays a member and is not a root admin. Carol, an admin, closes his pull request.

## 3. Nobody approves their own role change

Alice proposes making carol a member instead of an admin; carol approves it.

- Gitea would accept carol's approval, but the approval check does not count it: carol's own role changes. The pull request cannot be merged.

## 4. Bob changes the app; carol merges

Bob: *@homepage/operator In the repo todo of homepage, change the heading of page.html from Todo to Homepage todos, in both the title and the h1.*

- The agent opens a pull request on `homepage/todo`, as bob.
- Carol merges it. The pipeline tests, builds and pushes the image, and the deployer deploys it: the todo page shows *Homepage todos*.

## 4b. Bob asks the media agent for a screenshot

Alice adds the agent `homepage/media` (folder `agents/media` in `homepage/config`, tools `list_stacks` and `screenshot`, the shared skill `homepage-screenshots`); carol approves it. Bob: *@homepage/media Take a screenshot of https://todo.<domain>/ and put it into the repo todo as docs/screenshots/todo.png.*

- The gateway's browser loads the todo app signed in as bob and the agent opens a pull request on `homepage/todo`, as bob, holding the PNG.
- The pull request says it shows the todo app as bob, with the page title *Homepage todos*, not the sign-in page.

## 4c. The media agent follows its skill

Bob: *@homepage/media Make the homepage screenshots of the todo app.*

- The agent loads the skill `homepage-screenshots` and follows it: two pull requests, a 1440×900 desktop screenshot and a 390×844 phone screenshot, at the paths the skill names.

## 4d. What the media agent may not do

Bob: *Take a screenshot of the todo app and put it into the namespace system, repo platform.* Then: *Take a screenshot of https://example.org/.*

- Bob may not change `system/platform`: the gateway refuses before the browser runs, and no pull request appears.
- The browser loads only the installation's own pages: example.org is refused, and nothing is proposed.

## 5. Alice removes bob

Alice: *@homepage/operator Remove bob from the namespace homepage.*

- The agent opens a pull request on `homepage/config`, as alice, without bob.
- Carol approves it (she neither wrote it nor changes role); alice merges it.
- Bob leaves the team `members` and may no longer push to `homepage`'s repos. He can still read everything, as everyone signed in can.

## 6. Alice adds an agent that may only read

Alice proposes `agents/reader` with a tool that does not exist: the approval check fails and names it. With `read_file` alone, carol approves it and alice merges it.

- `homepage/reader` appears in the chat. Asked to propose a change, it cannot: the gateway serves it no `propose_change`.

## 7. The operator finds out why an app does not run

The app `homepage/quotes` builds, but exits at start: its `stack.yaml` does not set `QUOTES_FILE`. Alice: *@homepage/operator The quotes app does not work. Find out why and tell me what to change.*

- The agent looks at the stack's tasks or logs, as alice, and names the missing setting.
