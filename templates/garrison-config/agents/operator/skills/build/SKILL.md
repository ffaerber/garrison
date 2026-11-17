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
- agent: operator                    # one of garrison's agents
  name: morning-check                # lowercase, unique in the file
  schedule: "0 9 * * *"              # cron: minute hour day-of-month month day-of-week, in the home cloud's time zone
  owner: alice                       # the person asking; a person of the namespace
  task: Check that todo answers on main and dev, and say in a sentence how it is.
```

- One of `schedule` (cron, or `@daily`, `@hourly`, ...), `every` (`10m` to `168h`), `on_push: {repo: todo, branch: main}` or `on_alert: {alertname: ServiceFailing}`. Not more often than every 10 minutes.
- Its answer is posted in the namespace's channel.
- It runs with nobody watching, with the agent's own account: it may change a repo only through a grant with `unattended: true` and the owner in its `for`, and then only with propose_patch. Propose that grant in the same pull request when the task changes something.
- Tools that need a person's sign-in, screenshot, browse and propose_change, are refused in a routine. When a wish needs them on a schedule, say so plainly and offer what works: the same task in a chat whenever the person asks, or a routine that reads and reports.
- Write the task as you would ask the agent yourself: what to look at, what to do, what to say.

## After proposing

Give each pull request's link and say who must approve it: an admin of the namespace who did not write it (for garrison, a root admin). Nothing happens until it is merged.
