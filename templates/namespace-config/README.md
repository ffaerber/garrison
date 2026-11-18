# config

What this namespace is: who belongs to it, its apps, and what Garrison's agents may do here. Every change goes by pull request and needs the approval of an admin who did not write it.

- `members.yaml`: its admins and members.
- `stacks/<name>.yaml`: its apps, one per file, each deployed at `https://<name>.<domain>`.
- `restores/<name>.yaml`: bringing an app's data back from a snapshot.

## Agents

The agents are Garrison's, defined in `garrison/config`; each works in every namespace. Mention one by its name in this namespace's channel, e.g. `@operator`, and it works here, with your rights. A namespace has no agents of its own, but it may give them more:

`grants.yaml`: rights of an agent's own, here, each for some of this namespace's people:

```yaml
- agent: operator
  repo: web
  branch: dev
  level: autonomous     # propose, merge or autonomous (merge into a branch that deploys)
  for: members          # admins, members or everyone (propose only)
  checks: [ci]          # commit statuses that must pass before it merges
  unattended: true      # routines may use it too
```

`routines.yaml`: work an agent does here by itself, for an `owner` of this namespace, with a `task`; its answer goes to this namespace's channel:

```yaml
- agent: operator
  name: morning
  schedule: "30 6 * * mon-fri"      # cron, in Garrison's time zone; or every: 6h,
  owner: alice                      # on_push: {repo: web, branch: main}, on_alert: {alertname: ServiceFailing}
  task: Look at the apps' logs of the night and tell us what failed.
```

An agent with grants or routines here acts with an account of its own, `agent-<namespace>-<agent>`, which writes only to the repos its grants name.
