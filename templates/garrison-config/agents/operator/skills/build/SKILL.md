---
name: build
description: Turns a wish into garrison's parts (namespace, repo, stack, grant, routine) and sets them up in order. Load it when someone asks how to build something, for an app, or for something that should happen by itself, like a cron job.
---

# Building something in garrison

Find out which parts the wish needs, ask once for what is missing, then make or propose each part, in this order.

| The person wants | The part | Where | How |
| --- | --- | --- | --- |
| a home for a team, an organisation, a project | a namespace | Gitea organisation | create_namespace; root admins only |
| code of their own | a repo | `<namespace>/<repo>` | create_repo, from a template (static-web, ...) |
| it to run, at a hostname | a stack | `<namespace>/config` `stacks/<app>.yaml` | propose_change; an admin approves |
| an agent to change a repo by itself | a grant | `<namespace>/config` `grants.yaml` | propose_change; an admin approves |
| something done by itself: daily, hourly, on a push, when an app fails (a cron job) | a routine | `<namespace>/config` `routines.yaml` | propose_change; an admin approves |

Agents are garrison's only: a namespace never gets an `agents/` folder. A grant or routine names one of garrison's agents (find_agents lists them).

## Asking first

Ask in one message for what you would otherwise guess, and only that:

- the namespace, unless the channel says it;
- names: of the namespace, the repo, the app;
- for a routine: when (a time, a weekday), and what the agent should do each time, if the wish leaves it open.

Do not ask for what you can look up: read the namespace's config, its stacks and its routines.yaml first.

## A routine (a cron job)

A routine starts an agent, as it works in the namespace, with a one-shot task. Read the namespace's `routines.yaml` first and propose it with the new entry added; keep the entries already there.

```yaml
- agent: monitor                     # one of garrison's agents
  name: morning-check                # lowercase, unique in the file
  schedule: "0 9 * * *"              # cron: minute hour day-of-month month day-of-week, in the home cloud's time zone
  owner: alice                       # the person asking; a person of the namespace
  task: "Check that todo answers on main and dev, and say in a sentence how it is."
```

- Always put the task in double quotes: a task with a colon in it (a time, a URL, "Note: ...") is not valid YAML otherwise. Inside it, write ' instead of ".

- One of `schedule` (cron, or `@daily`, `@hourly`, ...), `every` (`10m` to `168h`), `on_push: {repo: todo, branch: main}` or `on_alert: {alertname: ServiceFailing}`. Not more often than every 10 minutes.
- Its answer is posted in the namespace's channel.
- It runs with nobody watching, with the agent's own account: it may change a repo only through a grant with `unattended: true` and the owner in its `for`, with propose_patch or screenshot (and merge_change where the grant's level allows). Propose that grant in the same pull request when the task changes something.
- Its browser signs in as the agent's own account, so screenshot and browse work, also on private environments. propose_change, create_namespace and create_repo need a person and are refused.
- The agent must have the tools the task needs, and no more: pick the one whose role fits. The photographer takes screenshots, the monitor reads logs and metrics (good for checks and `on_alert`), the coder proposes code with propose_patch, qa tries apps in the browser. find_agents shows each agent's tools.

A daily screenshot of the app todo into its repo app, for example, is a grant and a routine in `todo/config`:

```yaml
# grants.yaml
- agent: photographer
  repo: app
  level: propose
  for: members
  unattended: true

# routines.yaml
- agent: photographer
  name: daily-screenshot
  schedule: "0 9 * * *"
  owner: alice
  task: "Take a screenshot of https://todo.<domain>/ at 1280x800 and propose it as docs/screenshots/todo.png in the repo app of todo."
```
- Write the task as you would ask the agent yourself: what to look at, what to do, what to say.

## After proposing

Before proposing a YAML file, read it once more as YAML: quoted tasks, two-space indents, one `-` per entry. Give each pull request's link and say who must approve it: an admin of the namespace who did not write it (for garrison, a root admin). Nothing happens until it is merged.
