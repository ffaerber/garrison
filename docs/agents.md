# Agents — Design

How agents are defined, run and updated. Status: steps 1 to 3 of the plan are built, and the proxies of step 4: agent folders and their checks; the agent service, which Open WebUI talks to; and a sandbox per chat, Pi in a container with its workspace in a volume, which gets the LLM and the agent's platform tools through the agent service. The gateway serves each agent only its own tools and skills. Agents can clone the repos the person may read into their workspace and propose their changes as pull requests by the person. Steps 7 to 10 are built too: agents' own accounts and grants, private agents for private repos, agents asking agents, routines that run without a person, previews of pull requests and browsing. Still design: datasets instead of volumes, clean-up of old chats, Pi's native skills. See Chat in [spec.md](spec.md).

## Goals

1. Agents are files in Git: a prompt, a model, tools and skills, changed by reviewed pull requests like `members.yaml`.
2. Garrison has several agents, e.g. an operator and a marketing agent, defined once in `garrison/config` and working in every unit; a unit gives them rights and routines there.
3. Every chat with an agent is a real agent with its own workspace: files, a shell, a browser, its own history, in a sandbox.
4. Open WebUI stays the chat; it talks to agents as if they were models.
5. An agent has the rights of the person talking to it, and holds no secrets. It has more only where its folder grants them, reviewed like a role change, and only for the people of its unit the grant names.
6. Agents can ask each other for work, without anyone gaining rights by the detour.

## Overview

```
Open WebUI ──/v1/chat/completions──▶ agent service (control plane)
  model "todo/marketing"              │ one container per chat
  + chat id, signed user              ▼
                                ┌─────────────────────────────┐
                                │ sandbox: Pi (agent harness) │
                                │ workspace + session history │
                                │ (dataset per chat)          │
                                └──────┬───────────────┬──────┘
                                   LLM proxy      gateway tools
                                (key stays out)  (as the person)
```

| Part | Where | Job |
| --- | --- | --- |
| Agent folders | `garrison/config/agents/<name>/` | What an agent is: prompt, model, tools, skills; the same agent in every unit |
| Grants and routines | `<unit>/config/grants.yaml`, `routines.yaml` | What a unit lets the agents do there beyond the person's rights, and what they do there by themselves |
| Agent service | Control plane, service `agents` | OpenAI-compatible endpoint for Open WebUI; starts, routes to and stops sandboxes; proxies the LLM and the tools |
| Sandbox image | `sandbox/` in the control plane's repo, built with it by Gitea Actions and pushed as `garrison/sandbox` | The harness (Pi) and what agents work with: bash, git, ripgrep, curl, jq, ffmpeg, and Chromium behind the command `web` (`sandbox/web.mjs`, Playwright through the sandbox's proxy, no sign-in) |
| Gateway | Control plane, service `gateway` | The tools, as today, acting as the person |

## Agent folders

The agents are Garrison's: all of them are folders in `garrison/config`, which only root admins change. Units have no agents of their own; each of garrison's agents works in every unit. Mentioned in a unit's channel, it works in that unit: there it is an instance, `<unit>/<agent>` (e.g. `todo/marketing`), with the rights and routines that unit gives it in its own config repo (see Accounts and grants, and Runs without a person). In a private chat, and in `garrison`'s channel, it is garrison's instance and works in whichever unit the person names.

```
garrison/config/
  skills/                       # shared by the agents
    homepage-screenshots/
      SKILL.md
      viewports.yaml
  agents/
    commander/
    operator/
      agent.yaml
      prompt.md
    marketing/
      agent.yaml
      prompt.md
      skills/                   # this agent's own skills
        youtube-thumbnail/
          SKILL.md
          template.svg
  grants.yaml                   # garrison's own, as in any unit
  routines.yaml

todo/config/
  members.yaml
  stacks/
  grants.yaml                   # rights of garrison's agents in todo
  routines.yaml                 # what they do in todo by themselves
```

`agent.yaml`:

```yaml
description: Makes screenshots and media and keeps the homepage current
model: openrouter/anthropic/claude-sonnet-5.5   # an enabled model of garrison/config llm/models.yaml; empty for the default
tools:                       # gateway tools this agent may use; `all` for every one, also later ones
  - list_repos
  - read_file
  - propose_change
  - screenshot
skills: [homepage-screenshots]   # shared skills; its own skills/ are always included
visible: members             # optional: only the people of the unit it works in
agents: [tester]             # agents it may ask for work, by name (see Agents asking agents); or any
internet: [packages, api.example.com]   # what its sandbox reaches; none without it
```

- `prompt.md` is the system prompt. The platform appends fixed rules after it (act as the person, propose and never claim a change is merged, give the link); a prompt can add rules but not remove these. Before it, the agent is told where it works: the unit of the channel, or none in particular.
- People mention an agent by its name, `@marketing`, the same in every channel. Its model in Open WebUI is `garrison/<agent>`; the agent service runs the instance of the channel's unit. Open WebUI lists only garrison's agents.
- `garrison/config` is written at install from `templates/garrison-config/`: the operator (the default model, `tools: all`, and its skill `build`, which turns a wish such as "a cron job every day at 9" into garrison's parts: unit, repo, stack, grant, routine) and the commander, and agents with one role each and only the tools it needs: `coder`, `sentry`, `scout` and `designer` (see the README of `agents/` there). The operator asks once for what only the person can say, such as a name or a time, and makes nothing until they answer. The scout searches the web with `web_search`, a tool of the agent service that asks the installation's SearXNG (the system stack `garrison/searxng`, which only the agent service reaches, with no account anywhere), for agents whose `agent.yaml` lists it and whose internet reaches the whole web; it reads pages with its browser (`web read`), lists a site's pages (`web map`: its sitemap, else the page's links) and reads up to 100 of them into text files to search (`web crawl`). It uses sites click by click with `browse` (links, buttons, fields, lists) and fills in and sends forms when the task asks: such pages open in the gateway's second Chrome, `webbrowser`, through the agent service's internet proxy as the scout, never signed in as anyone, and an agent that reaches the whole web never opens Garrison's own pages as the person. It signs in with a unit's site logins, which an admin adds on the dashboard's Secrets page (`login-<name>`: site, username, password) and `logins.yaml` of the unit's config allows, per agent, group and routine: `browse` with `login` and `part` has the gateway type the username or the password into the field named, only while the page is on the login's site and a password only into a password field; the deployer writes the logins for the gateway alone, and neither the agent nor its sandbox ever holds a password. Once it is signed in, it submits forms without asking, as the person chose. Without an `agents/` folder, garrison has the operator. A new unit's config gets `members.yaml` and a README, no agents.
- The `garrison/approval` check on `garrison/config`'s pull requests checks agent files: known tools and skills, an enabled model (see Models in [spec.md](spec.md)), a prompt that is not empty; root admins approve them. On a unit's `config`, it checks `grants.yaml` and `routines.yaml` (each entry names one of garrison's agents), and refuses an `agents/` folder: agents are garrison's only. `repos`, `grants` and `routines` in an `agent.yaml` are refused too: those are a unit's to give.
- `internet`: what the agent's sandbox reaches, through the agent service's proxy (see Sandbox). `packages` opens the package registries: Alpine, Debian, Ubuntu, PyPI, npm, Go modules and Rust crates, download hosts only, no GitHub. `all` opens every public address. A list names `packages` and domains, each with its subdomains. Without it, nothing. Where an agent works on a private repo (a grant or a routine on one), or is `visible: members`, it reaches the registries at most: the approval check refuses a unit's grant on a private repo to an agent that reaches more, and should a repo turn private later, the proxy holds it to the registries, if it had them.

## Accounts and grants

By default an agent has no rights of its own: every tool call acts as the person whose message started the turn, and changes leave only as pull requests by that person. A unit can give garrison's agents more there, per repo and branch, with the autonomy levels of [spec.md](spec.md) (Rights and autonomy), in `grants.yaml` of its config. Built: `control-plane/internal/agents/grants.go` (the file format and its checks), `internal/gateway/grants.go` (the decisions), `internal/gateway/members.go` (the accounts).

### Private repos and who sees an agent

Rights are per unit: its people (`members.yaml`) change its repos, everyone signed in reads the public ones, and only its people read the private ones. There are no people of single repos. An agent, as it works in a unit, follows the repos it works on there (those of its grants, and those its routines watch):

- **All public, or none** (like the operator in most units): everyone may talk to it. It acts with each person's rights, so someone outside the unit gets answers but no changes: proposing fails, as they may not write, unless a grant lets the agent propose for everyone.
- **One is private**: the agent, as it works in that unit, is private. It is only for the unit's people and root admins, and answers only where only they read: a private chat, or the unit's channel. Elsewhere, e.g. handed work in `garrison`'s channel, which everyone reads, the agent service refuses it with a note saying where to ask. A repo that does not exist yet counts as private. The same agent elsewhere is not affected.
- `visible: members` makes an agent private wherever it works: only the people of the unit it works in use it there, and in Open WebUI's list only root admins see it.

Everyone signed in sees garrison's agents in Open WebUI. The gateway keeps an Open WebUI group per unit, `garrison <unit>`, with the people of its `members.yaml` who have signed in to Open WebUI (matched by their email in Gitea); the unit's channel is given to that group; root admins are Open WebUI's admins and see everything. The agent service and the gateway check it again on every request, since Open WebUI is not the only way in: someone who may not use the agent gets a refusal that says why.

### The agent's account

Each agent with grants or routines in a unit gets an account in Gitea for its work there, `agent-<unit>-<name>` (e.g. `agent-todo-dev`; a hash where that is longer than Gitea allows), with the full name `Agent <unit>/<name>`. The gateway makes it, gives it write access to the repos its grants name and to no other repo of the unit, and locks it, with that access taken away, when the agent is gone, switched off or without grants there. Nobody signs in with it and no token of it exists: the gateway acts as it through Gitea's Sudo, as it acts as people.

The same account is in Keycloak, in the realm `garrison`, so the agent can sign in where a person would: in a routine, with nobody watching, its browser (`screenshot`, `browse`) shows the apps as the agent, also private environments. The gateway makes it with the Gitea account (email `<account>@agents.invalid`, no groups) and disables it when it locks that. It gets the account's tokens through the Keycloak client `agents`, which it sets up itself: password grants only, its tokens naming `sso`, so the sign-in in front of private environments takes them; the gateway's own MCP endpoint does not. The account's password and the client's secret are derived from the admin password, which the gateway holds anyway, so there is no further secret; a password someone changes in Keycloak is set back at the next sign-in.

### Grants

`grants.yaml` in `todo/config`:

```yaml
- agent: dev              # one of garrison's agents
  repo: todo
  branch: dev
  level: autonomous       # merge and deploy
  for: members
- agent: dev
  repo: todo
  branch: main
  level: autonomous
  for: admins
  checks: [ci]            # commit statuses that must be green before it merges
- agent: desk
  repo: todo
  level: propose          # outsiders' proposals, see below
  for: everyone
  unattended: true        # routines may use it too
```

| Level | The agent may, with its own account |
| --- | --- |
| `propose` | Write its own branches, `agent/<name>/...`, and open pull requests from them |
| `merge` | Also merge proposals into `branch` once the checks pass |
| `autonomous` | The same on a branch that deploys: the merge deploys as any merge to `main` or `dev` does |

- **A change to a unit's repo uses that unit's grants**, wherever the person asked: in its channel, another one, or a private chat. A run without a person keeps to the grants of its own unit.
- **`for` says whose requests may use a grant**, and only names groups of the unit that gives it: `members` (the default, admins included) or `admins`. `everyone` is allowed for `propose` only, and not on a private repo. Anything else fails the approval check.
- **The agent's account acts only for people who may read the repo.** Before it proposes or merges for someone, the gateway asks Gitea whether they may read it; nobody changes, or learns about, a repo they cannot see through an agent. Without `for`, a member could get through the agent what they may not do themselves, e.g. merge to `main`. Root admins count as admins everywhere.
- **Each action is decided at the gateway, in this order:** if the person may do it themselves, it is done as the person, as before. Otherwise, if a grant of the repo's unit covers the agent, repo, branch and level and the person is in its `for`, it is done with the agent's account there. Otherwise it is refused, and the agent is told which grant would be needed.
- **Gitea limits the account too:** it may write only to its granted repos. Which branches it may merge into is the gateway's decision, as app repos take pushes from their members and have no branch protection.
- **`merge` waits for the deployer.** Until the deployer can hold a deploy for a person's approval (not built), every merge to `main` or `dev` deploys, so the approval check refuses `merge` on those branches and asks for `autonomous`, which says what happens.
- **Pull requests name whom they were for.** The gateway, not the model, writes *Asked by bob through todo/dev.* into each pull request an agent opens, and *Changes added, asked by alice ...* as a comment on each addition; merges carry the same line. Runs without a person say so: *Asked by bob through todo/dev, routine todo/dev/nightly, with nobody watching.* The gateway logs every call with the person, the agent and what started the run.
- **Someone else's work needs someone else's approval.** A shared channel is one workspace, so one person's turn may carry another's work: bob edits, alice says "merge it". The agent merges a pull request only if everyone it was asked for (its author, if a person, and every *Asked by*) is the person asking now, or if someone who did not ask for it approved it in Gitea. So people can put their own work live through an agent, and never another's unreviewed.

### New units and repos

`create_unit` and `create_repo` act as the person: Gitea lets only root admins make units, and a unit's admins and root admins make its repos. A new unit gets its config and its channel from the gateway within a minute, the person as its first admin; the agents work in it at once. `create_unit` answers once both are there, with the channel's link (`https://chat.<domain>/channels/<id>`), for the agent to give the person, as Open WebUI's sidebar shows a new channel only after a reload; for a unit that is there already, its error carries that unit's link. A new repo is empty but for a README, and only for code of an app's own; its files come as a pull request like any other change. `create_branch` (the operator's and the coder's) also acts as the person, who must be allowed to push there: it makes a lasting branch, such as `dev`, from main or another branch; when a stack deploys that branch as an environment, the answer gives its address, and the deployer deploys it once its image is built. The branches of changes (`change/...`) are only `propose_patch`'s. Every agent's system prompt (`agents.Platform`) says how Garrison is laid out, with a full `config/stacks/<name>.yaml` that a test checks the deployer takes, so a request for a new app becomes a unit if needed and a proposed stack in its config, never a stack in `garrison` or a secret made by hand.

### Proposing and merging

- **Propose:** `propose_patch` (`propose_workspace_changes` in a sandbox) as before. Where the person may not write to the repo but a grant lets the agent propose there for them, Gitea applies the patch as the agent's account, on a branch `agent/<name>/<person>-<time>`. The Git proxy stays read-only: changes leave the sandbox only as patches, and granted branches are reached only by merging a pull request, so every change keeps its diff, its checks and an easy revert. `propose_workspace_changes` takes a `base`, e.g. `dev`.
- **Merge:** a tool `merge_change(unit, repo, number)`, only for agents (never on the plain `/mcp` endpoint), and only of proposals (branches `change/...` and `agent/<name>/...`). The gateway checks the grant, `for`, the checks and the approval rule above, then has Gitea merge as the agent's account.
- **Deploy and roll back:** no tool of their own. A merge deploys through the pipeline, as for people. Rolling back is a revert, merged the same way.
- **Keeping proposals current:** when the base branch of an open proposal moves, the gateway has Gitea merge the base into it, as the pull request's author. Where they conflict, it says so once in the pull request, for a person or the agent to resolve. `config`'s proposals are left alone: a new head would undo their approval.

### Outsiders

With `propose` for `everyone`, someone who is not in the unit can ask the agent for a change, e.g. in a private chat. The agent's account in that unit writes the branch, since the person cannot, and the pull request says *Asked by bob (not in todo) through todo/desk.* The unit's people review and merge it as any pull request. Without that grant, an outsider gets the agent with their own rights: they read what they may read, and proposing fails with the reason.

### What no grant can lift

The gateway refuses these whatever an agent folder or `grants.yaml` says, and the approval check refuses grants that would need them:

- Writing to a `config` repo, so an agent never widens its own grants, and so changing `members.yaml`. `propose_skill` is no exception: the platform's account opens its pull request, holding one skill file and nothing else, for a root admin to merge.
- Changing branch protection, deleting repos, or anything in Gitea's or Open WebUI's settings: there is no tool for it, and the agent's account has no admin rights.
- Grants on repos of another unit: each unit grants only on its own. Grants in `garrison` need a root admin's approval, as every change to `garrison/config` does.
- `enabled: false` in `agent.yaml` switches an agent off everywhere: at the next sync, seconds after the merge, it leaves Open WebUI and the gateway's catalogue, its tool calls are refused and its accounts are locked; the approval check then refuses grants and routines naming it. A turn already running ends at its next tool call.

## Agents asking agents

A tool, `ask_agent(agent, task, limits, done_when, background)`, served by the agent service to agents with an `agents` list, hands work to another agent: what to do, what bounds it, and what a good answer looks like, as fields, not a paragraph the other agent has to guess from.

- **Whom:** only the agents in the asker's `agents` list, by name, and only those the person may use themselves (`visible`). The approval check checks that they exist. The asked agent works where the asker does: `tester`, asked by `todo/dev`, is `todo/tester`; `<unit>/<agent>` names it as it works in another unit.
- **Rights:** the asked agent runs for the same person, with its own tools and the grants of where it works. A grant is always checked against the person who started the chain, never against the asking agent, so no detour adds rights: bob asking `todo/dev` to ask `lab/dev` is bob asking `lab/dev`. The gateway logs the asked agent's calls as started by `agent todo/dev`.
- **Where:** in its own sandbox for the same chat (key: the chat and the asked agent), with its own workspace. The two share work through Git, e.g. a proposal's branch or its preview.
- **Limits:** at most three agents in one run, and never one twice, so A asking B asking A ends at once. The agent service keeps the chain in the turn's state, not in anything the model writes.
- **Asking again:** the asked agent ends its answer with one line, `Status: done`, `Status: partial: <what is missing>` or `Status: stuck: <what blocked it>`. After one that did not finish, the asker is told to ask once more with a sharper task, saying what the answer lacked and changing what blocked it, not repeating it. One turn asks the same agent at most three times (`MaxAsks`), counted by the agent service. The feed marks the attempt ("asked again (2. time)") and an unfinished status. When an ask is done after one that missed, the feed keeps a lesson: what the first task was, why it missed, and the task that worked; an asker with `propose_skill` is told to propose it.
- **Waiting:** the asker waits for the answer, which counts against its turn's time limit; the asked agent's tool calls count against its own cap. If the asked agent is already working in this chat, the call waits for its turn. Its answer comes back as the tool result, word for word, and its progress shows in the reasoning, each line under its name.
- **Private agents** are asked only where they may answer: an agent in `garrison`'s channel cannot hand work to one that is private in `todo`; the refusal is logged.
- **Finding them:** `find_agents(in_unit)`, for the same agents, lists those it may ask for this person, as they work where the asker does (or in `in_unit`): what each does (`description`), its tools, skills and grants there, and which unit the channel belongs to, with that unit's app repos as the person sees them; with `in_unit`, the link to that unit's channel, which the commander gives when it sends someone there, so the commander knows when "the app" could mean several and asks which. Agents it may not ask, the person may not use, or that may not answer here are left out.
- **Anyone:** `agents: any` lets an agent ask every agent the person may use. Root admins approve it, as every agent; it adds no rights, as the asked agent still acts as the person.
- **The feed** has each hand-off, for the person and the asked agent's unit, with its answer; the asked agent's calls are threaded under it.
- **Background:** in a channel, `background: true` returns at once; the asked agent works on and its answer is posted in the channel when done. From a private chat it hands the person over: the asked agent's answer, with the task, starts a conversation in its unit's channel, mentioning the person, who goes on there with Reply; the asker gets the channel's link to give them. Only into a channel the person reads (a unit they are in, `garrison`'s, or any for a root admin); the task is all of the private chat that goes there, as the unit's people read it. It still acts with the person's Keycloak token, which lasts five minutes, so a long background run fails at its later tool calls.

## Chains of command

A chain of command is the order in which agents do one kind of job, written down once: `chains/<name>.yaml` in `garrison/config`, which only root admins change. It has a `description`, a `when` (which requests it is for), a `lead`, a `merge` (what happens to its pull requests, below) and 2 to 12 `steps`, each an agent and what it `does`; the lead has the first step.

- **The lead runs it.** It does its own steps and asks each other agent for its step with `ask_agent`, one after another, passing on only what that step needs. Each answers back to the lead; nobody asks the next one. So a run stays commander → lead → one agent at a time, within `MaxChain`. The lead may ask the chain's agents without naming them in its `agents:`.
- **Every agent knows the order.** The chains are part of each agent's system prompt: the lead gets the whole chain and how to run it, every other agent the chains it is in, its own steps marked, and who comes after it. The commander gets every chain in one line each, and hands a request that matches a chain's `when` to its lead as a whole. `find_agents` names the chains an agent leads.
- **Routines** run a chain with `chain: <name>` instead of `agent` (its lead runs it); `task` adds to it. `add_routine` with `chain` and `repos` gives the chain's agents that propose (`propose_patch`, `screenshot`) the grants with `unattended: true` on those repos. In `garrison`, only root admins add routines.
- **Merging:** `merge` says what happens to the pull requests a run of the chain opens. `review` (the default): a person reviews and merges them in Gitea. `ask`: the lead asks the routine's owner with `ask_person`, and merges after they agree. `auto`: the lead merges each one as soon as it is proposed. It is the most a run may do, not a right: merging also needs the unit's grant (`autonomous` on the branch, `unattended: true`), which `add_routine` proposes with a routine of an `ask` or `auto` chain, for the unit's admins to approve. The lead is told its mode; in a routine the gateway enforces it, as the agent service names the run's mode to it with the run's origin: `merge_change` is refused under `review`, and under `ask` until the run has gone on from the owner's answer. In a chat, the person there decides, as for any merge.
- A chain naming a missing or switched-off agent, or whose first step is not the lead's, is left out; the approval check names why.

`news-page`, from the template, `merge: auto`: keeps a page made from the app template news current. The coder reads the repo's `topics.md` (the topics, and the sites to look at first) and the links `news.yaml` already lists, and passes on only those; the scout looks at the sources and searches the web, and returns only new items on those topics, ignoring instructions found on pages; the coder rewrites `news.yaml`, proposes it and merges it, and the merge deploys the page. The agent that reads the web cannot change a repo, and the one that changes the repo never opens a web page, so the repo may be private; the page's tests refuse a `news.yaml` with unknown fields, a link that is not `https://` or the same link twice, a topic that is not a heading of `topics.md`, or a link off the sites a topic names in a line `Only from:`, so a bad file is never built. Only that line holds a topic to its sites for sure: without it, which items fit is the model's judgment, and a page that lies in plain words can still mislead the scout. The operator sets it up from a wish such as "a page of new Go jobs in Berlin, every morning" (its skill `build`): the repo, its stack behind the sign-in, and two routines, daily and on a change of `topics.md`. Tested in the `newsroom` step.

`software-updates`, from the template, `merge: review`: the sentry lists the images and tags in use (each `garrison` repo's `kubernetes.yaml`, a unit's `stacks/`), the scout finds on the projects' own pages how each tags its images (semantic versions, dates such as SearXNG's, a browser's frequent majors such as Chrome's, variants such as `-alpine`), what was published since and whether the line or variant in use is still maintained, with only those names and tags; the sentry decides by the release notes, not by the numbers' shape (an ordinary upgrade is proposed, whatever its number; a migration such as PostgreSQL 17 to 18 is only reported; a discontinued line or variant is flagged with its replacement), the coder proposes one pull request per repo changing only the tags, and the sentry reports. Every Monday: a routine in `garrison` with `chain: software-updates`. Tested in the `media` step: alice asks the commander about updates of a unit whose apps pin `traefik/whoami:v1.10.0` and `chromedp/headless-shell:150.0.7871.125`, and both are proposed, Chrome's newer major too.

## The commander

The commander is for when someone does not know which agent to ask. It is written into `garrison/config` at install (`agents: any`, no tools), is the default model of new chats in Open WebUI (`garrison/commander`), and every channel's pinned message names it. Like every agent, it goes by its name, `@commander`, in any channel, and works in the channel's unit. It does no work itself: it calls `find_agents`, then

- hands a clear, one-off task (a screenshot, a new repo, why an app does not start) to the agent that fits, with `ask_agent`, working in the same unit (or the one named, as `<unit>/<agent>`); in a channel in the background, so that agent posts its answer there itself;
- for work with back and forth (a feature, a new app), from a private chat hands the person over (`ask_agent` with `background`): the agent starts the conversation in the unit's channel, mentioning them, and the commander gives them the link; in a channel, or for a unit they are not in, it tells them which agent to talk to, `@<agent>` in that unit's channel, with its link, and why;
- asks one question when it cannot tell, and says when no agent fits and what one would need.

Root admins change it like any agent of `garrison`, by a pull request to its folder.

## Runs without a person

Routines: work one of garrison's agents does in a unit by itself, in `routines.yaml` of the unit's config. A routine is a one-shot task: the agent service starts the agent, as it works in that unit, and gives it the task as its prompt.

```yaml
- agent: dev                           # one of garrison's agents
  name: nightly
  every: 24h                           # 10m to 7 days
  owner: bob                           # a person of the unit
  task: Update the dependencies of todo and propose it.
- agent: operator
  name: morning
  schedule: "30 6 * * mon-fri"         # cron, in Garrison's time zone
  owner: alice
  task: Sum up what happened in todo yesterday.
- agent: dev
  name: main-moved
  on_push: {repo: todo, branch: main}  # when the branch moves
  owner: bob
  task: Check what changed and say whether it needs a follow-up.
- agent: operator
  name: on-failure
  on_alert: {alertname: ServiceFailing}  # when one of this unit's alerts fires
  owner: bob
  task: Find out why it fails and say what to change.
```

- **Asking for one:** people ask the operator in words ("every morning at 9, take a screenshot of todo"); it proposes the routine with `add_routine`, which writes the YAML, keeps the routines there and adds the grant a run needs. Someone allowed approves it, on its card or in Gitea.
- **Who:** a routine acts with the agent's own account in the unit, for its owner, who must be in the unit when it runs (else it posts that it did not run). It uses only grants that say `unattended: true` and have the owner in their `for`; otherwise it reads and reports. Its browser signs in with the agent's account in Keycloak, so `screenshot` and `browse` work, and a screenshot is proposed with the agent's account through such a grant. `propose_change`, `create_unit` and `create_repo` need a person and are refused.
- **Asking:** a routine that needs a choice, a fact or an approval from its owner calls `ask_person`; it ends its turn and goes on, in its sandbox, when the owner replies in the question's thread (see Notifications in [spec.md](spec.md)).
- **When:** each routine has one of `schedule`, `every`, `on_push` and `on_alert`. The agent service looks every minute.
  - `on_alert` lists label values an alert of this unit must have, e.g. `{alertname: ServiceFailing}` or `{alertname: ServiceDown, app: todo}`; `{}` takes every alert of the unit. The platform's own alerts belong to `garrison`. The routine starts once per firing, and its task is told the alert's summary, labels and description. If it is still running from an earlier one, Alertmanager's next reminder (every 4 hours) starts it.
  - `schedule` is cron's five fields, minute hour day-of-month month day-of-week (names like `mon-fri` and `dec` work), or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`. It is read in Garrison's time zone, `TIMEZONE` in `install.env` (the machine's at install, else UTC). A time a clock change skips runs right after the change. The approval check refuses a schedule that never comes or runs more often than every 10 minutes.
  - `on_push` may name `paths`, e.g. `{repo: news, branch: main, paths: [topics.md]}`: the routine then starts only when a push changed one of those files, or a file below one ending in `/` (Gitea's compare of the commit it last saw and the new one; if Gitea cannot say, it runs). So a routine whose own merges change other files does not start itself again.
  - A routine with `schedule` first runs at the next time it names after it is first seen; one with `every` one interval after; one with `on_push` when the branch moves after that.
  - When each last ran, and the commit each `on_push` routine last saw, are kept in Postgres (`routine_state` in `garrison`). After a restart, a schedule missed meanwhile runs once, and a push made meanwhile starts its routine. Until the agent service can read that state, no routine runs.
  - One run of a routine at a time.
- **In the feed:** each run is in the dashboard's feed with its answer, for the unit's people and the owner, and its thread shows what it did.
- **Where the answer goes:** posted in the unit's channel, which only its people read, under the agent's name, by the gateway's Open WebUI account.
- **How the gateway knows:** the agent service and the gateway derive a key from the admin password, which both hold. With it, the agent service tells the gateway what started each run (`person`, `agent <id>`, `routine <id>/<name>`) and, for a routine, its owner instead of a person's token. The gateway logs such calls as `bob (nobody watching, as agent-todo-dev)`.

## Previews and browsing

- **Previews:** a stack with an environment `preview` gets one per open pull request of its source repo, from a branch of the repo itself: `pr-<n>`, at `pr-<n>-<stack>.<domain>`, deployed like `preview` from the pull request's branch once its pipeline has built the image, and taken down when the pull request closes. At most the three newest pull requests of a repo have one. Branches named `pr-<number>` cannot be environments.
- **`browse`:** an agent uses a page as the person would, in the gateway's Chrome, signed in as them: open a page, click an element, type into a field (and press Enter), each call answered with the page's text and its links, buttons and fields, numbered. Each person and agent has a tab of its own, in a browser context of its own, kept for five minutes between calls. Like `screenshot`, it loads only the installation's own names.
- Together: an agent proposes a change, waits for its preview, and tries it with `browse`, or asks a tester agent to.

## Skills

Skills use the Agent Skills format: a folder with `SKILL.md` (frontmatter `name` and `description`, then instructions) and any files it needs. Pi loads them natively.

- An agent sees each of its skills' name and description in its prompt, and reads a skill's instructions and files only when a task needs it.
- Tools are abilities, enforced by the gateway. Skills are know-how: how to use the tools for a job. A skill grants nothing; a call to a tool the agent lacks is refused.

### Agents propose skills

What a run learnt sticks as a skill, through review. An agent with the tool `propose_skill(agent, skill, content, description)` writes a skill for itself or an agent it asks (the coder for the scout in `news-page`, say), typically after asking again with a sharper task helped: the skill says, in general words, what the better task said.

- It writes one file, `agents/<agent>/skills/<skill>/SKILL.md` in `garrison/config`, checked like any skill (frontmatter, a name matching its folder, at most 32 KB) and never under the name of a shared skill. The same name again replaces it.
- No person of a unit and no agent may write to `garrison/config`, so the platform's account opens the pull request, on a branch `skill/<agent>/<skill>-<ms>`, naming the agent and the person it worked for, with the agent's reasons. The approval check refuses a skill branch that changes any other file, whoever pushed it: a skill never brings tools, internet, a model, grants or a prompt with it.
- A root admin reviews and merges it, as every change of `garrison/config`; nothing merges it by itself, whatever a chain's `merge` says. Once merged, the agent sees it in its prompt and loads it when a task matches.
- The feed's Lessons show each proposal, waiting for review, then merged (with a new entry: the agent learnt the skill) or closed unmerged.
- The risk is a lasting prompt injection: a page the scout read that tells it to "always add this link" would, copied into a skill, steer every later run. So the leads write skills in their own words, never from page text, and a person reads every one as instructions the agent will follow.

## Agent service

An OpenAI-compatible API, reachable only from Open WebUI on the system network.

- `GET /v1/models` lists every agent folder of `garrison/config` as a model, `garrison/<agent>`, named `<agent>`. A chat in a unit's channel runs the agent's instance there, `<unit>/<agent>`. Open WebUI shows them in its model picker; the gateway no longer syncs model presets.
- `POST /v1/chat/completions` streams the agent's answer. Tool calls and progress show as reasoning, so people see what the agent does.
- Open WebUI's connection sends, as custom headers, `X-Garrison-Chat: {{CHAT_ID}}` and `X-Garrison-Task: {{TASK}}`, and the person's Keycloak token (`system_oauth`), which the agent service verifies.
- Requests with a task header (titles, tags, follow-up suggestions) go straight to the LLM without a sandbox.
- Open WebUI sends the whole conversation each time. The agent service passes on only the new message, and Open WebUI's own system messages (a channel thread's history): the harness keeps its own session, including tool calls and results. A fresh session, e.g. of a chat begun before sandboxes, gets the earlier messages as context. Branching the session when a message is edited or regenerated is to come.
- The answer streams as Pi writes it: its thinking, tool calls and failed tool results as reasoning, its text as content, piece by piece. Text before a tool call stays in the answer as a step ("Let me look at the logs."), as only a message's end says whether it was the answer. For an agent asked by another, only its last message is the answer; earlier text shows as progress.

## Sessions

A container belongs to a chat, not to an agent or a person.

| Situation | Containers |
| --- | --- |
| Alice and Bob each chat with `todo/marketing` | 2 |
| Alice opens a second chat with it | 3 |
| Alice returns to her first chat the next day | Hers starts again where it left off |

- **Workspaces are separate.** Each chat has its own volume (built: a Docker volume `garrison-agent-<hash>`; planned: a ZFS dataset, for snapshots and quotas) holding the workspace and the session, so drafts of one chat never show in another. Agents share work through Git (branches and pull requests) and the unit's media library, like people do: the share `media`, mounted at `/data/media` in the sandboxes of the unit's channel and routines once a stack mounts it, never in private chats or another unit's. The sandbox guard binds that one host folder, `/garrison/ns/<ns>/shares/media`, and only for the unit the container is labelled with.
- **Rights are separate.** Every tool call acts as the person whose message started the turn, or with the agent's account where only a grant allows it (Accounts and grants).
- **Team channels:** a unit channel is one conversation shared by its members. Open WebUI names the channel (`channel:<id>`), not the thread, in its requests, so a channel gets one container per agent for all its threads; each thread's history still comes along with each message. Each turn acts as the person who mentioned the agent in that message. The workspace is visible to everyone in the thread; private work belongs in a private chat.
- **One turn at a time per chat;** a message sent while the agent works waits for the turn to end.

## Sandbox

- One container per chat and agent from the sandbox image, its main process `pi --mode rpc`: the agent service attaches to its stdin and stdout, sends commands and reads events, so the sandbox opens no port. Built.
- Pi gets the agent's prompt with the platform's rules and a note on its sandbox; its own tools (read, write, edit, bash) work in `/data/work`; a Pi extension (`sandbox/garrison.ts`) adds the agent's platform tools.
- Mounted: the chat's volume at `/data` (workspace and Pi's session). Skills are still read through the gateway's `load_skill`; mounting the agent's folder and skills for Pi to load natively is to come.
- Git: the agent service passes fetches (never pushes) on to Gitea with its admin account, after asking Gitea, as the person, whether they may read the repo; Gitea's Git protocol knows no Sudo. Changes leave only as patches through `propose_patch`, which Gitea applies as the person, so branch protection and authorship hold.
- Network: `garrison-sandboxes`, internal, shared only with the agent service. On a second port (8071) the agent service passes on the LLM, adding the API key and enforcing the agent's model, and the gateway's tools, adding the Keycloak token of the person whose turn runs. A sandbox names itself with a random token given at its start, which counts only during a turn. The sandbox holds no key, no person's token and no Docker access.
- Runtime: gVisor (`runsc`, installed by `install.sh`). A sandbox's processes run as root, with Docker's default capabilities, against gVisor's kernel in user space, which in turn uses few system calls of the host's; code that breaks out of the container meets gVisor before Linux. Root lets agents install what a task needs (`apk add`, `pip install`, `npm install -g`); it lasts until the container stops, and only `/data` is kept. gVisor has no Docker DNS, so the agent service gives sandboxes its own address on the network rather than its name.
- Internet: on a third port (8072) the agent service is an HTTP proxy, set in the sandbox's `HTTP_PROXY` and `HTTPS_PROXY` with its token, which curl, apk, pip, npm and git follow. It lets a sandbox in a turn reach what its agent's `internet` allows, on ports 80 and 443, and never an address that is not public (the home network, the platform's services, loopback, link-local, shared CGNAT space), checked on the address it connects to, so a name that resolves into the home network is refused too. Each request is logged with the person and the agent. A sandbox has no other way out: its network is internal.
- Private repos: an agent that reaches more than the registries clones only public repos through the Git proxy, so a page that tricks it cannot get it to send a private repo out.
- Limits per container: 1 GB of memory, one CPU, 512 processes. Containers are started through the sandbox guard, like the user runner's, which allows only the sandbox image, under gVisor, with the label `garrison.sandbox`, these limits, that network and volumes named `garrison-agent-*`. It knows its containers by the label, so it still does after a restart.

## Lifecycle and updates

- **Start:** on the first message of a chat, or the next message after a stop. A new chat's first answer takes a few seconds more.
- **Stop:** after 15 minutes without a message. The volume stays. When the agent service restarts, it removes all sandbox containers; their next message starts them again.
- **Clean-up:** Open WebUI does not report deleted chats; datasets of chats unused for 30 days are removed, after a snapshot.
- **Updates:** an agent is built from three layers, each changed in its own place.

| Layer | Lives in | A change reaches running chats |
| --- | --- | --- |
| Sandbox image | `garrison/sandbox` | At their next turn |
| Agent definition | `garrison/config/agents/`; grants and routines in `<unit>/config` | At their next turn |
| Tools and platform rules | Gateway, in the control plane | When the gateway is deployed |

Before each turn the agent service compares the container's image and agent commit with the current ones and, if either changed, restarts the container between turns; the session resumes from the dataset. A running turn is never interrupted.

A new unit or repo starts nothing: the agents work in it at once, and containers start only when someone chats.

## Limits

- Per person: at most 3 running sandboxes; a new chat stops that person's oldest idle one, or is refused while all three work.
- Machine: a cap from free memory; idle sandboxes are stopped first.
- Per turn: a time limit (10 minutes) and a cap on tool calls (60 in a sandbox), after which the turn is aborted.

## Observability

- The Grafana dashboard **Agents** (`garrison/grafana/dashboards/agents.json`), built: running sandboxes, by agent and by person, their memory and CPU, and a table of each with its chat; from cAdvisor, which passes on the sandboxes' labels `garrison.agent`, `garrison.person` and `garrison.chat`. Below, what agents did: the agent service's log (turns with person, agent, chat and number of tool calls; Git fetches; agents asking agents; routines) and the gateway's (every tool call with person, agent, tool and what started the run, refusals with why).
- What was said stays out of it: a sandbox's output is Pi's conversation, with whatever the agent read, private repos included, so Alloy does not ship sandboxes' logs to Loki, which everyone signed in reads.
- To come: a `list_sessions` tool that shows the same to the operator agents; whether a sandbox is working or idle.

## Plan

1. Agent folders: parsing, checks on `config` pull requests, `garrison/config` with the operator and the commander, every agent working in every unit with the unit's grants and routines. Built. The gateway serves an agent its tools and skills (`load_skill`, `read_skill_file`) when a call names it in `X-Garrison-Agent`.
2. Agent service: models list, streaming, chat id, the person's Keycloak token (Open WebUI's `system_oauth`, so no forwarded-user JWT is needed), task requests to the LLM. Built with the loop in its own process: the LLM with the agent's prompt, and its tools from the gateway as the person; tool calls show as reasoning. Open WebUI has no LLM key and no tools any more.
3. Sandbox image with Pi; one container per chat with its volume, idle stop and resume. Built; requests without a chat id still run in the agent service's process. To come: a dataset per chat, removing unused ones.
4. LLM and tool proxies; per-agent tool lists enforced; skills. Proxies built, with the per-agent tool lists of step 1. Git built: the agent service serves Git's fetch protocol to sandboxes (`$GARRISON_GIT/<unit>/<repo>.git`) for the repos the person may read, and the gateway's `propose_patch` applies a patch as the person on a `change/...` branch with a pull request; in Pi it is `propose_workspace_changes`, which proposes a checkout's changes and adds later ones to the same pull request. Several agents work on one repo like people do: each on its own branch, merged by a person. To come: Pi's native skills, Git LFS.
5. Move the operator agents over; remove the model preset sync.
6. Marketing agent: `screenshot` tool, built: headless Chrome in the control plane, signed in as the person, the PNG proposed as a pull request; tested with a marketing agent that bob asks for a screenshot of the todo app. The media library, built: `/data/media`, browsed in `stacks/files.yaml` (`garrison/files`). `generate_media`, built: the agent service reads a project's `styles/<style>.yaml` and its ComfyUI workflow as the person, fills in the nodes the style names (prompt, negative, seed, size, frames), runs it on the installation's ComfyUI (`COMFYUI_URL` in `install.env`, outside Garrison, one run at a time) and hands what it made to the turn's sandbox, which writes each file with a `.json` of how it was made into `/data/media/<project>/`. `media_models` lists ComfyUI's models and the model families they make up, with the nodes each needs, so the agent writes workflows only for models that are there. `generate_speech`, built: a text-to-speech service outside Garrison with an OpenAI-style `/v1/audio/speech`, such as Kokoro (`TTS_URL` in `install.env`), speaks a narration in the voice of the project's `narration.yaml` into the library, as an MP3 with its `.json`; narrated shorts take their length from it. From stories: the template `storyboards` (one `stories/<name>.md` per short: title, sources, narration, scenes), written by the chain `storyboards` (scout and coder, `merge: review`); once merged, a routine of the unit (`on_push` of `stories/`, and `every: 30m` to catch up) has `@designer` make the next story's short, `shorts/<name>.mp4`, one per run. A routine whose whole answer is `NOTHING TO DO` is recorded in the feed but posts nothing. Shorts are made of video clips only unless someone asks for pictures: 5-second clips (Wan 2.2 TI2V 5B, 704x1280 at 24 fps, about 3 minutes each on a 4090, preferred by `media_models` over Wan 2.1), each started from one key picture of the short's subject (Z-Image, then Wan 2.2 image to video: `generate_media` with `image`, which the agent service uploads to ComfyUI), so every clip shows the same character; each clip's prompt names an action, and the sound is normalised to -14 LUFS, 48 kHz stereo, with crossfades between clips in the project's video style, about one per 5 seconds of narration, so `designer`'s `agent.yaml` gives its turns 30 minutes (`turn: 30m`, at most 30; others keep the agent service's 10) and its sandbox 4 GiB (`memory: 4Gi`, at most 4, as the cluster's admission policy allows; others keep 1 GiB): cutting six 1080p clips with crossfades in one ffmpeg ran a 1 GiB sandbox out of memory, so it also cuts in steps. The agent `designer` (with the skill `media-project`; shorts with ffmpeg in the sandbox). To come: Git LFS for larger media.
7. Accounts and grants, built: private agents for private repos, kept as Open WebUI groups and checked by the agent service and the gateway; an account per agent with grants or routines, writing only to its granted repos; grants checked on `config` pull requests, with the limits no grant lifts; proposals by the agent's account on `agent/<name>/...`; `merge_change`; outsiders' proposals. Tested in scenarios 4g, 4h, 5b and 5c.
8. `ask_agent`, built: waiting for the answer, or, in a channel, posting it later. Tested in 4i.
9. Runs without a person, built: routines (`schedule`, `every`, `on_push`, kept across restarts), posting into the unit's channel, the record of what started a run; keeping proposals current when their base moves. Tested in 4j.
10. Previews of pull requests and `browse`, built. Tested in 4i.

## Open questions

- Branching Pi's session when a message is edited or regenerated in Open WebUI: today the newest message is simply sent on.
- Thread ids for channels, so each thread gets its own sandbox.
- GPU or a local LLM for agents that run many turns.
- Posts from routines and background answers come from the gateway's account with the agent's name in bold; Open WebUI has no way to post as a model. They go to the channel, not into a thread.
- A background `ask_agent` acts with the person's Keycloak token, which lasts five minutes.
- Previews' datasets and secrets stay after the preview is taken down, like a deleted branch's.
- Whether `merge` on `main` and `dev` becomes usable once the deployer holds deploys for approval.
