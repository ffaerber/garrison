# Walkthrough

How Garrison is used, step by step: one page per step, with screenshots and
short clips. alice and bob are two people of a home cloud at
`garrison.test`. alice makes a namespace `shop`, has the agents build a web
page there and put it online, works on it on a `dev` branch, lets bob join,
and has things done on a schedule.

Every picture is from the browser test (`test/ui/scenarios.py`), which
clicks through Gitea, Open WebUI and the dashboard as a person would and
checks what happens. So what these pages show is what a current Garrison does.
To make them again after a change, see [Making the pictures](#making-the-pictures).

| Step | What happens |
| --- | --- |
| [1. Installation](01-installation.md) | One command on a fresh Ubuntu machine |
| [2. Signing in](02-signing-in.md) | One account for the dashboard, Gitea and the chat |
| [3. A namespace](03-namespace.md) | `shop`: its config, its channel, its secrets |
| [4. An app and its stack](04-app-and-stack.md) | The operator makes the repo `web` and proposes its stack; alice merges; it is live |
| [5. A dev environment](05-dev-environment.md) | A branch `dev` runs as `dev-web`; changes go there first, then to main |
| [6. The agents at work](06-agents.md) | A screenshot for the repo; the commander finds the right agent |
| [7. A second person](07-members.md) | bob joins; from then on main takes only approved changes |
| [8. Routines: agents on a schedule](08-routines.md) | "Every day at 9, …": an agent does it by itself and asks when it needs to |
| [9. Scheduled jobs](09-scheduled-jobs.md) | A plain cron job of the app, in Gitea Actions, with no agent |

## Making the pictures

The UI step of the VM test saves a screenshot of every step in
`bin/ui-shots/`; `make.py` picks the ones these pages show and makes the
clips:

```sh
test/vm/steps.sh ui                      # the browser test, on a test VM
python3 docs/walkthrough/make.py         # bin/ui-shots/ -> docs/walkthrough/img/
```

`make.py` names the shots it takes in `STILLS` and the clips in `CLIPS`. If
the test no longer takes a shot, `make.py` names it and writes nothing.
