# config

What this unit is: who belongs to it, its apps, and what Garrison's agents may do here. Every change goes by pull request and needs the approval of an admin who did not write it.

- `members.yaml`: its admins and members.
- `stacks/<name>.yaml`: its apps, one per file, each deployed at `https://<name>.<domain>`.
- `restores/<name>.yaml`: bringing an app's data back from a snapshot.
- `moves/<name>.yaml`: moving an app's environment, with its data, to another machine (`stack`, `environment`, `to`).

## Media

The unit's media library is the share `media`: a folder of pictures, videos and other files its people and agents share. It exists once a stack mounts it. Then the agents see it at `/data/media` in this unit's channel and routines, and `@designer` makes pictures, videos and short videos into it. This file browser, `stacks/files.yaml`, shows it at `https://files.<domain>`: everyone signed in looks and downloads, and this unit's people (`{{members}}`, from `members.yaml`) upload, rename and delete.

```yaml
entrypoint: web
services:
  web:
    image: "{{registry}}/garrison/files:current"
    port: 8080
    env:
      GARRISON_WRITERS: "{{members}}"
    volumes:
      library: { share: media, path: /srv }
      data: { dataset: files, size: 1Gi, path: /database }
    resources: { memory: 128Mi }
environments:
  main: { private: true }
```

The address is the file's name, so another unit names its own, e.g. `stacks/photos-files.yaml`.

## Agents

The agents are Garrison's, defined in `garrison/config`; each works in every unit. Mention one by its name in this unit's channel, e.g. `@operator`, and it works here, with your rights. A unit has no agents of its own, but it may give them more:

`grants.yaml`: rights of an agent's own, here, each for some of this unit's people:

```yaml
- agent: operator
  repo: web
  branch: dev
  level: autonomous     # propose, merge or autonomous (merge into a branch that deploys)
  for: members          # admins, members or everyone (propose only)
  checks: [ci]          # commit statuses that must pass before it merges
  unattended: true      # routines may use it too
```

`routines.yaml`: work an agent does here by itself, for an `owner` of this unit, with a `task`; its answer goes to this unit's channel:

```yaml
- agent: operator
  name: morning
  schedule: "30 6 * * mon-fri"      # cron, in Garrison's time zone; or every: 6h,
  owner: alice                      # on_push: {repo: web, branch: main}, on_alert: {alertname: ServiceFailing},
                                    # on_release: {unit: todo, repo: app}
  task: Look at the apps' logs of the night and tell us what failed.
```

A routine with `on_release: {unit: todo, repo: app}` runs on each new release of that repo (one released in Gitea, not a draft or pre-release), with its notes; another unit's releases only while that unit publishes them here.

`publishes.yaml`: what other units may use of this one. Its admins decide what leaves the unit:

```yaml
- releases: app        # the releases of the repo app
  to: [site]           # may start routines of these units (on_release)
```

An agent with grants or routines here acts with an account of its own, `agent-<unit>-<agent>`, which writes only to the repos its grants name.

`logins.yaml`: which agent may sign in to a web site with one of this unit's site logins, for whose requests. An admin adds the login itself (site, username, password) on the dashboard's Secrets page, as `login-<name>`; Garrison's browser types it into that site's own pages only, and the agent never sees the password:

```yaml
- login: shop           # the secret login-shop
  agent: scout
  for: members          # admins, members or everyone
  unattended: true      # routines may use it too
```
