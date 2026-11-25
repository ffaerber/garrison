---
name: build
description: Turns a wish into garrison's parts (unit, repo, stack, grant, routine) and sets them up in order. Load it when someone asks how to build something, for an app, or for something that should happen by itself, like a cron job.
---

# Building something in garrison

Find out which parts the wish needs, ask once for what is missing, then make or propose each part, in this order.

| The person wants | The part | Where | How |
| --- | --- | --- | --- |
| a home for a team, an organisation, a project | a unit | Gitea organisation | create_unit; root admins only |
| code of their own | a repo | `<unit>/<repo>` | create_repo, from a template (todo, news, ...) |
| a page of what is new on their topics (news, job offers, releases), kept current by agents | a news page | a repo from the template news, its stack, two routines | see "A news page" below |
| it to run, at a hostname | a stack | `<unit>/config` `stacks/<app>.yaml` | propose_change; an admin approves |
| an agent to change a repo by itself | a grant | `<unit>/config` `grants.yaml` | propose_change; an admin approves |
| another AI provider, for more models | a provider | `garrison/config` `llm/providers.yaml` | add_provider; root admins only. Its API key goes on the dashboard's Models page, never into the chat |
| something done by itself: daily, hourly, on a push, when an app fails (a cron job) | a routine | `<unit>/config` `routines.yaml` | add_routine (on a schedule or every so often); propose_change for on_push or on_alert; an admin approves |

Agents are garrison's only: a unit never gets an `agents/` folder. A grant or routine names one of garrison's agents (find_agents lists them).

## Asking first

Ask in one message for what you would otherwise guess, and only that:

- the unit, unless the channel says it;
- names: of the unit, the repo, the app;
- for a routine: when (a time, a weekday), and what the agent should do each time, if the wish leaves it open.

Do not ask for what you can look up: read the unit's config, its stacks and its routines.yaml first.

## A routine (a cron job)

A routine starts an agent, as it works in the unit, with a one-shot task.

For one on a schedule or every so often, call add_routine: unit, agent, name, schedule (or every), task, and repo if the task changes a repo (a screenshot or a patch proposed into it). It writes the entry into `routines.yaml`, keeps the entries already there, adds the grant the run needs when you give repo, checks it all, and opens one pull request for the person asking, who owns the routine. Do not write that YAML yourself.

add_routine also takes on_push (a repo, a branch, and paths: the files whose change starts it). For `on_alert`, read the unit's `routines.yaml` first and propose it with propose_change, the new entry added and the entries already there kept. The entries look like this:

```yaml
- agent: sentry                       # one of garrison's agents
  name: morning-check                # lowercase, unique in the file
  schedule: "0 9 * * *"              # cron: minute hour day-of-month month day-of-week, in Garrison's time zone
  owner: alice                       # the person asking; a person of the unit
  task: "Check that todo answers on main and dev, and say in a sentence how it is."
```

- Always put the task in double quotes: a task with a colon in it (a time, a URL, "Note: ...") is not valid YAML otherwise. Inside it, write ' instead of ".

- One of `schedule` (cron, or `@daily`, `@hourly`, ...), `every` (`10m` to `168h`), `on_push: {repo: todo, branch: main}` or `on_alert: {alertname: ServiceFailing}`. Not more often than every 10 minutes.
- Its answer is posted in the unit's channel.
- It runs with nobody watching, with the agent's own account: it may change a repo only through a grant with `unattended: true` and the owner in its `for`, with propose_patch or screenshot (and merge_change where the grant's level allows). Propose that grant in the same pull request when the task changes something.
- Its browser signs in as the agent's own account, so screenshot and browse work, also on private environments. propose_change, create_unit and create_repo need a person and are refused.
- The agent must have the tools the task needs, and no more: pick the one whose role fits: the sentry reads logs and metrics, tries apps in the browser and takes screenshots (good for checks and `on_alert`), the coder proposes code with propose_patch. find_agents shows each agent's tools.

A daily screenshot of the app todo into its repo app, for example, is add_routine with agent sentry, schedule "0 9 * * *", repos [app] and the task below; it writes a grant and a routine in `todo/config`:

```yaml
# grants.yaml
- agent: sentry
  repo: app
  level: propose
  for: members
  unattended: true

# routines.yaml
- agent: sentry
  name: daily-screenshot
  schedule: "0 9 * * *"
  owner: alice
  task: "Take a screenshot of https://todo.<domain>/ at 1280x800 and propose it as docs/screenshots/todo.png in the repo app of todo."
```
- Write the task as you would ask the agent yourself: what to look at, what to do, what to say.

A job several agents do in order is a chain of command (`chains/` in garrison/config; find_agents shows the agent that leads one). Its routine is add_routine with chain instead of agent and task, and repos: the repos it proposes into; the chain's agents that propose get the grants. "Check for software updates every Monday" is add_routine in garrison (root admins only) with chain software-updates, schedule "0 7 * * 1" and repos: the repos of garrison with a kubernetes.yaml (list_repos and list_files show them):

```yaml
# routines.yaml of garrison/config
- chain: software-updates
  name: weekly-updates
  schedule: "0 7 * * 1"
  owner: admin
```

## A news page

A page agents keep current by themselves: "a page of new Go jobs in Berlin, every morning", "news about k3s". It is the chain news-page: the coder reads the repo's topics.md, the scout searches the web for its topics, and the coder rewrites news.yaml and merges it (merge: auto in the chain), which deploys the page. Ask once for the unit, the repo's name (e.g. news), the time, and the topics with any sites to look at first.

1. create_repo with template news. Then propose its topics.md with the person's topics: a heading per topic, a line on what it is about, and its sources as https:// links, as the template's topics.md shows.
2. Propose the stack `stacks/<repo>.yaml` its README shows, with `main: { private: true }`: the page is behind the sign-in.
3. add_routine twice, both with chain news-page and repos [<repo>]:
   - the daily one, with schedule, e.g. "0 7 * * *";
   - one on a change of topics.md: on_push {repo: <repo>, branch: main, paths: [topics.md]}, so editing the topics updates the page at once, and the coder's own merges of news.yaml start nothing.

   The first gives the coder the grant to merge into main of the repo with nobody watching; say so plainly: an admin of the unit approves it once, and after that nobody reviews the updates. A bad one is undone by reverting its merge.

## After proposing

Before proposing a YAML file with propose_change, read it once more as YAML: quoted tasks, two-space indents, one `-` per entry. Give each pull request's link and say who must approve it: an admin of the unit who did not write it (for garrison, a root admin). Nothing happens until it is merged.
