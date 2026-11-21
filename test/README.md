# Tests

End-to-end tests on a throwaway Multipass VM. The MVP acceptance list in `docs/spec.md` is the target suite.

```
make test                        # unit tests, in a container
test/vm/steps.sh                 # every scenario that is not green yet, up to JOBS (4) VMs at once
test/vm/steps.sh routines people # only these, and the fixtures they start from
test/vm/steps.sh --from install  # rerun from `install`: it and all that grows from it
test/vm/steps.sh --list          # the tree: each step's kind, state and minutes
test/vm/steps.sh --clean         # delete the VMs, their snapshots and the green marks
KEEP=1 test/vm/steps.sh guided   # keep a passing scenario's VM, to look around in it
JOBS=2 test/vm/steps.sh          # at most 2 VMs at once
LLM=cloud test/vm/steps.sh       # the agents on Ollama's cloud instead of the local Ollama
COMFYUI=host test/vm/steps.sh media  # media with a real ComfyUI on this computer (port 8188 on the VMs' gateway), not the fake
LOCAL_MODEL=nemotron-3.5-lightning:latest test/vm/steps.sh   # another local model
test/e2e/smoke.sh roles          # in a VM: one scenario, test/e2e/scenarios/roles.sh
```

The agents run on Ollama on this computer by default (`qwen3.8-64k`), reached from the VM through its gateway; Ollama must listen on more than localhost (`OLLAMA_HOST=0.0.0.0`). No key is needed.

## The tree

A **fixture** is a state scenarios start from; green, it is saved as a snapshot of its own VM. A **scenario** is a test: it runs in a fresh clone of its fixture's VM, which is deleted when it passes and kept, stopped, when it fails (`garrison-test-<scenario>`), so one failure stops nothing else. Up to `JOBS` VMs run at once (8 GB each).

On the branch `k3s`, the platform runs on Kubernetes and only base, install and login are green so far; the scenarios follow as the sandboxes and apps move (`docs/kubernetes.md`). `VM=garrison-k3s test/vm/steps.sh login` keeps those VMs apart from main's.

```
1 base [F] - 2 install [F] - 3 login [F] -+- 4.1 platform
                                          +- 4.2 crud
                                          +- 4.3 ui
                                          +- 4.4 agent [F] --- 5.1 media
                                          +- 4.5 deploy [F] -+- 5.2 journey
                                                             +- 5.3 people
                                                             +- 5.4 team [F] -+- 6.1 roles      6.5 commander
                                                                              +- 6.2 change     6.6 operator
                                                                              +- 6.3 marketing  6.7 leaving
                                                                              +- 6.4 routines   6.8 guided
```

Each step is `test/e2e/scenarios/<step>.sh` on the helpers of `test/e2e/lib.sh`; the browser scenarios (`ui`, `people`, `guided`) run `test/ui/<step>.py` in Playwright's Chromium, and their screenshots land in `bin/ui-shots/<step>/`. What each scenario plays is in `docs/scenarios.md`.

| Step | Kind | Does |
| --- | --- | --- |
| `base` | fixture | Fresh Ubuntu VM with Docker, ZFS, git and the other base packages, and this repo at `~/garrison` |
| `install` | fixture | The installer: Traefik, Keycloak, Gitea, the runner, the control plane, Prometheus, Grafana and Loki; the test people; the system stack checked (`system`) |
| `login` | fixture | Signs in through Keycloak as admin, alice and bob, to Gitea, Grafana and Open WebUI, and checks what each may do |
| `platform` | scenario | The deployer deploying from Gitea, monitoring data, dashboards from Git, app stacks, private environments, hostname claims, the model sync with a fake provider, the dashboard's Models page |
| `crud` | scenario | Every kind of thing a unit holds, made, read, changed and removed without the LLM |
| `ui` | scenario | In the browser, as alice, bob and the admin: the unit `todo` and its app, built as `deploy` builds it, a dev branch, a screenshot, a second person, routines and a scheduled job. `docs/walkthrough/make.py` takes the walkthrough's pictures from its screenshots |
| `agent` | fixture | Bob and alice ask the agents of `garrison` in its channel: logs, metrics, proposals, an LLM provider, the scout's browser under gVisor |
| `media` | scenario | A unit's media library and file browser, `@media` with a fake ComfyUI (a real one with `COMFYUI=host`), a short, and the chain `software-updates` |
| `deploy` | fixture | The unit `todo`, alice its only person: `todo/app` and `todo/quotes` pushed, the app built on the user runner and deployed as `todo` and `dev-todo`, with the deployer's refusals |
| `journey` | scenario | Scenario 10: an app from nothing to main, through the operator |
| `people` | scenario | In the browser: alice asks the commander, vaguely, to bring bob and carol in; it asks which roles. Bob and carol then find todo's channel in their sidebar, with its how-to and history; after bob leaves, it is gone from his and Open WebUI refuses him its messages |
| `team` | fixture | Carol an admin and bob a member, by a config pull request; the developer, tester and desk in `garrison/config`, with their grants in todo. No LLM |
| `roles` | scenario | Bob cannot raise his own rights; nobody's approval of their own role change counts; no agent may have a grant on config |
| `change` | scenario | Bob changes the app through an agent and carol merges; the developer with Git, on dev, with previews the tester tries; an agent installs packages |
| `marketing` | scenario | The marketing agent's screenshots, its skill, what it may not do, its sandbox, and its routine shooting the app behind the sign-in |
| `routines` | scenario | A routine runs when main moves, with nobody watching; a routine asks its person and goes on with the answer |
| `commander` | scenario | The commander finds the agent and hands the work over; the operator asks what it cannot know, then does it |
| `operator` | scenario | Why an app does not run; a new app from nothing; todo's data back |
| `leaving` | scenario | Alice removes bob; the desk takes his request; a private repo with a private agent; an agent that may only read |
| `guided` | scenario | In the browser: bob asks, vaguely, for a blue background; the commander asks which app, and the change goes to the app's code, carol merges and the page turns blue. An address of its own goes to the stack, in todo's config |

A fixture's snapshot, or a scenario's green mark (`bin/green/<step>`), stays valid while the files its step depends on, and those of every step on its way from `base`, are unchanged; `--list` shows which are. Change one scenario's file and only it reruns; change `lib.sh` or `smoke.sh` and everything from `login` does; change the installer and everything from `install`. Starting from a snapshot boots the VM, so every step also tests a restart.

A full run takes about 45 minutes; one scenario, about 2 minutes to clone its fixture plus its own time. Each step copies this checkout into its VM first: tracked and new files, never ignored ones such as `.env`.

## The test people

`test/e2e/add-test-users.sh` creates three people with the password `12345678`: `alice`, a root admin, and `bob` and `carol`, regular users.

## Opening the test installation in a browser

A green run points this computer's DNS at the VM and opens Keycloak (`OPEN=0` skips that). By hand:

```
test/vm/dns.sh          # *.garrison.test resolves to the VM from this computer
test/vm/dns.sh --off    # undo
multipass exec garrison-test -- sudo cat /etc/garrison/admin-password
```

Then open `https://chat.garrison.test`, `https://git.garrison.test`, `https://grafana.garrison.test`, `https://keycloak.garrison.test` or any deployed stack. The certificate is self-signed, so the browser asks once per hostname. The DNS setting lasts until a reboot or until the VM is recreated. It needs systemd-resolved on this computer.

## Getting changes onto an installed machine

`test/vm/push-repos.sh`, run in the VM, commits `garrison/` to the repos of the `garrison` organisation and waits for the deployer to deploy them, the way an admin's push would. A changed controller is built by Gitea Actions first.

`test/vm/steps.sh` needs Multipass (`sudo snap install multipass`).
