# Walkthrough

How Garrison is used, step by step: one page per step, with screenshots.
alice and bob are two people of the Garrison installation at
`garrison.test`. alice makes a unit `todo`, has the agents set up a
todo app there and put it online, works on it on a `dev` branch, lets bob
join, and has things done on a schedule. The todo app is Garrison's one
example app, the template `todo`: a list of their own for everyone signed
in, in Go on Postgres, with its tests.

Every picture is from the browser test (`test/ui/scenarios.py`), which
clicks through Gitea, Open WebUI and the dashboard as a person would and
checks what happens. So what these pages show is what a current Garrison does.
To make them again after a change, see [Making the pictures](#making-the-pictures).

| Step | What happens |
| --- | --- |
| [1. Installation](01-installation.md) | One command on a fresh Ubuntu machine |
| [2. Signing in](02-signing-in.md) | One account for the dashboard, Gitea and the chat |
| [3. A unit](03-unit.md) | `todo`: its config, its channel, its secrets |
| [4. An app and its stack](04-app-and-stack.md) | The operator makes the repo `app` from the template `todo` and proposes its stack; alice merges; it is live |
| [5. A dev environment](05-dev-environment.md) | A branch `dev` runs as `dev-todo`; changes go there first, then to main |
| [6. The agents at work](06-agents.md) | A screenshot for the repo; the commander finds the right agent |
| [7. A second person](07-members.md) | bob joins; from then on main takes only approved changes |
| [8. Routines: agents on a schedule](08-routines.md) | "Every day at 9, …": an agent does it by itself and asks when it needs to |
| [9. Scheduled jobs](09-scheduled-jobs.md) | A plain cron job of the app, in Gitea Actions, with no agent |

## Making the pictures

The UI step of the VM test saves a screenshot of every step in
`bin/ui-shots/`; `make.py` picks the ones these pages show:

```sh
test/vm/steps.sh ui                      # the browser test, on a test VM
python3 docs/walkthrough/make.py         # bin/ui-shots/ -> docs/walkthrough/img/
```

`make.py` names the shots it takes in `STILLS`. If
the test no longer takes a shot, `make.py` names it and writes nothing.
