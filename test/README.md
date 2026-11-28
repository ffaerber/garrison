# Tests

End-to-end tests on a throwaway Multipass VM. The MVP acceptance list in `docs/spec.md` is the target suite.

```
make test                        # unit tests, in a container
test/vm/steps.sh                 # every scenario that is not green yet, up to JOBS (2) VMs at once
test/vm/steps.sh routines people # only these, and the fixtures they start from
test/vm/steps.sh --from install  # rerun from `install`: it and all that grows from it
test/vm/steps.sh --list          # the tree: each step's kind, state and minutes
test/vm/steps.sh --clean         # delete the VMs, their snapshots and the green marks
KEEP=1 test/vm/steps.sh guided   # keep a passing scenario's VM, to look around in it
JOBS=4 test/vm/steps.sh          # at most 4 VMs at once
FREE_GB=12 test/vm/steps.sh      # a VM boots only while this much memory is free (default 10)
test/vm/watch.py                 # the run, live, in the browser: http://localhost:8099
test/vm/agents-live.py garrison-k3s-animalfacts   # the agents in a VM, live: thinking, tool calls, results
COMFYUI_URL= test/vm/steps.sh media  # media with the fake ComfyUI, though .env names a real one
test/vm/steps.sh animalfacts     # five narrated shorts on the real ComfyUI and Kokoro (COMFYUI_URL, TTS_URL in .env); watch them in bin/animalfacts/
LOCAL_MODEL=nemotron-3.5-lightning:latest test/vm/steps.sh   # another model of the local Ollama, without PROVIDERS
test/e2e/smoke.sh roles          # in a VM: one scenario, test/e2e/scenarios/roles.sh
test/vm/ask-bank.sh garrison-k3s-commander   # ask the agents the questions of test/e2e/questions.tsv, each in a fresh private chat; the answers, with flags for known mistakes, in bin/ask-bank/
```

This computer's settings come from this checkout's `.env`, which git ignores, so addresses on your network and keys stay out of the repo; `.env.example` lists them all. `PROVIDERS` are the AI providers the setup adds after the install, `name=url` each (OpenAI-style APIs; `GATEWAY` in a URL is this computer as a VM reaches it), with their keys as `<NAME>_API_KEY` (`OLLAMA_CLOUD_API_KEY` for `ollama-cloud`), which go into the VM as files; `DEFAULT_MODEL` is the default model, and `AGENT_MODELS` puts agents on models of their own. Without `PROVIDERS`, the agents run on Ollama on this computer (`LOCAL_MODEL`, default `qwen3.8-64k`), which must listen on more than localhost (`OLLAMA_HOST=0.0.0.0`). `COMFYUI_URL` and `TTS_URL` are the real ComfyUI and text-to-speech service for `media` and `animalfacts`; without them, the ones on this computer when they answer. The providers are set in the setup, at install, so changing them reruns everything from install.

## The tree

A **fixture** is a state scenarios start from; green, it is saved as a snapshot of its own VM. A **scenario** is a test: it runs in a fresh clone of its fixture's VM, which is deleted when it passes and kept, stopped, when it fails (`garrison-k3s-<scenario>`), so one failure stops nothing else. A step that Multipass's daemon crashed under (its process changed, and the log shows a command cut off) runs once more. Up to `JOBS` VMs run at once (8 GB each; 2 by default, as more at once crashed Multipass's daemon more often), fewer when the memory available at the start holds fewer, and each boots only while `FREE_GB` is free. One run at a time (`bin/lock-run`): a run first stops any VM of ours still running, left by a run that was killed or a computer that crashed. Its output also goes to `bin/steps.out`.

`test/vm/watch.py` serves a page that follows a run as it goes: the tree with each step's state and time, the memory and the running VMs, and the log of the step you pick, growing as it is written. It only reads `bin/`, so it can start before, during or after a run.

The VMs run Garrison on Kubernetes (`docs/kubernetes.md`).

```
1 base [F] - 2 install [F] - 3 login [F] -+- 4.1 platform
                                          +- 4.2 crud
                                          +- 4.3 ui
                                          +- 4.4 agent [F] --- 5.1 media
                                          +- 4.5 deploy [F] -+- 5.2 journey
                                                             +- 5.3 people
                                                             +- 5.5 nodes
                                                             +- 5.6 newsroom
                                                             +- 5.4 team [F] -+- 6.1 roles      6.5 commander
                                                                              +- 6.2 change     6.6 operator
                                                                              +- 6.3 marketing  6.7 leaving
                                                                              +- 6.4 routines   6.8 guided
                                                                                                6.9 threads
```

Each step is `test/e2e/scenarios/<step>.sh` on the helpers of `test/e2e/lib.sh`; the browser scenarios (`ui`, `people`, `guided`, `threads`) run `test/ui/<step>.py` in Playwright's Chromium, and their screenshots land in `bin/ui-shots/<step>/`. What each scenario plays is in `docs/scenarios.md`.

| Step | Kind | Does |
| --- | --- | --- |
| `base` | fixture | Fresh Ubuntu VM with Docker, ZFS, git and the other base packages, and this repo at `~/garrison`; one virtual NVMe drive, `/dev/loop40` (test/vm/nvme.sh: Multipass attaches no disks, so a loop device on a sparse file, attached at boot before ZFS), on which the install makes Garrison's pool (`POOL_DEVICE`) |
| `install` | fixture | The installer: Traefik, Keycloak, Gitea, the runner, the control plane, Prometheus, Grafana and Loki; then its setup in the browser (`scenarios/setup.sh`): the first root admin `owner`, the AI providers of `PROVIDERS`, each tested (a wrong key refused), and the default, tested and saved; the test people; the system stack checked (`system`) |
| `login` | fixture | Signs in through Keycloak as admin, alice and bob, to Gitea, Grafana and Open WebUI, and checks what each may do |
| `platform` | scenario | The deployer deploying from Gitea, monitoring data, dashboards from Git, app stacks, private environments, hostname claims, the model sync with a fake provider, the dashboard's Models page (a provider's key, choosing an agent's model), a skill an agent proposes for another (`propose_skill`) and its lessons in the feed, the agents' web search (SearXNG) |
| `crud` | scenario | Every kind of thing a unit holds, made, read, changed and removed without the LLM |
| `ui` | scenario | In the browser, as alice, bob and the admin: the unit `todo` and its app, built as `deploy` builds it, a dev branch, a screenshot, a second person, routines and a scheduled job. `docs/walkthrough/make.py` takes the walkthrough's pictures from its screenshots |
| `agent` | fixture | With `AGENT_MODELS` (e.g. `coder=ollama-cloud/glm-5.3`, from the environment or `.env`), alice first puts those agents on those models on the Models page, one pull request that the install's admin approves (`scenarios/models.sh`); the setup added Ollama's cloud with `OLLAMA_API_KEY`. Then bob and alice ask the agents of `garrison` in its channel: logs, metrics, proposals, an LLM provider, the scout's browser under gVisor |
| `media` | scenario | A unit's media library and file browser, `@designer` with the real ComfyUI of this computer when it answers on port 8188 with a GPU (else a fake one), a short, and the chain `software-updates`, which must propose both a semantic-version update and Chrome's next major |
| `animalfacts` | scenario | A YouTube channel as a unit: `@designer` sets up its project with no workflow given (only models ComfyUI has), then makes five narrated 30-second shorts that must look and sound alike, and `@operator` puts them on a public website (template `channel`). Needs the real ComfyUI on this computer and a text-to-speech service such as Kokoro (`TTS_URL` in `.env`, or one on port 8880 here), else skipped; the shorts land in `bin/animalfacts/` |
| `deploy` | fixture | The unit `todo`, alice its only person: `todo/app` and `todo/quotes` pushed, the app built on the user runner and deployed as `todo` and `dev-todo`, with the deployer's refusals |
| `journey` | scenario | Scenario 10: an app from nothing to main, through the operator |
| `people` | scenario | In the browser: alice asks the commander, vaguely, to bring bob and carol in; it asks which roles. Bob and carol then find todo's channel in their sidebar, with its how-to and history; after bob leaves, it is gone from his and Open WebUI refuses him its messages |
| `nodes` | scenario | A second VM, a clone of `base`, joins with `install.sh --join` and a larger drive (40G); an app with data runs there, with its snapshots, a restore and a reboot (`docs/nodes.md`) |
| `newsroom` | scenario | A news page the chain `news-page` keeps current: a fake news site, the coder and the scout, an update merged with nobody watching, and the next day's news (`docs/scenarios.md`) |
| `team` | fixture | Carol an admin and bob a member, by a config pull request; the developer, tester and desk in `garrison/config`, with their grants in todo. No LLM |
| `roles` | scenario | Bob cannot raise his own rights; nobody's approval of their own role change counts; no agent may have a grant on config |
| `change` | scenario | Bob changes the app through an agent and carol merges; the developer with Git, on dev, with previews the tester tries; an agent installs packages |
| `marketing` | scenario | The marketing agent's screenshots, its skill, what it may not do, its sandbox, and its routine shooting the app behind the sign-in |
| `routines` | scenario | A routine runs when main moves, with nobody watching; a routine asks its person and goes on with the answer |
| `commander` | scenario | The commander finds the agent and hands the work over; the operator asks what it cannot know, then does it |
| `operator` | scenario | Why an app does not run; a new app from nothing; todo's data back |
| `leaving` | scenario | Alice removes bob; the desk takes his request; a private repo with a private agent; an agent that may only read |
| `guided` | scenario | In the browser: bob asks, vaguely, for a blue background; the commander asks which app, and the change goes to the app's code, carol merges and the page turns blue. An address of its own goes to the stack, in todo's config |
| `threads` | scenario | In the browser: bob asks the operator in todo's channel; a plain message in its thread goes to no agent, its Reply button goes on with the operator without @, and @commander answers in the same thread |
| `invites` | scenario | In the browser, the People page's links: carol, an admin of todo, invites frank, who makes his account with the link; the pull request on todo's `members.yaml` in carol's name waits for alice's approval; alice's link without a unit, revoked; her password link for bob |

A fixture's snapshot, or a scenario's green mark (`bin/green/<step>`), stays valid while the files its step depends on, and those of every step on its way from `base`, are unchanged; `--list` shows which are. Change one scenario's file and only it reruns; change `lib.sh` or `smoke.sh` and everything from `login` does; change the installer and everything from `install`. Starting from a snapshot boots the VM, so every step also tests a restart.

A full run takes about 45 minutes; one scenario, about 2 minutes to clone its fixture plus its own time. Each step copies this checkout into its VM first: tracked and new files, never ignored ones such as `.env`.

## A news page on the real web

`test/e2e/scenarios/newsdemo.sh` is not in the tree: it sets up the news page of `newsroom` with topics of your own, and the agents look on the real web, so what it finds depends on the day. Run it in a clone of the fixture `deploy`, with your `topics.md` copied into the VM:

```
multipass clone garrison-k3s-deploy --name garrison-k3s-newsdemo   # once garrison-k3s-deploy is stopped
multipass start garrison-k3s-newsdemo
multipass transfer bin/news-topics.md garrison-k3s-newsdemo:topics.md
multipass exec garrison-k3s-newsdemo -- sudo bash -c 'cd garrison && NEWS_TOPICS=/home/ubuntu/topics.md WAIT_READY=600 test/e2e/smoke.sh newsdemo'
```

It prints `news.yaml` and leaves the page running at `https://news.<domain>/` (see below for opening it).

## The test people

`test/e2e/add-test-users.sh` creates three people with the password `12345678`: `alice`, a root admin, and `bob` and `carol`, regular users.

## Opening the test installation in a browser

A green run points this computer's DNS at the VM and opens Keycloak (`OPEN=0` skips that). By hand:

```
test/vm/dns.sh          # *.garrison.test resolves to the VM from this computer
test/vm/dns.sh --off    # undo
multipass exec garrison-k3s -- sudo cat /etc/garrison/admin-password
```

Then open `https://chat.garrison.test`, `https://git.garrison.test`, `https://grafana.garrison.test`, `https://keycloak.garrison.test` or any deployed stack. The certificate is self-signed, so the browser asks once per hostname. The DNS setting lasts until a reboot or until the VM is recreated. It needs systemd-resolved on this computer.

## Getting changes onto an installed machine

`test/vm/push-repos.sh`, run in the VM, commits `garrison/` to the repos of the `garrison` organisation and waits for the deployer to deploy them, the way an admin's push would. A changed controller is built by Gitea Actions first.

`test/vm/steps.sh` needs Multipass (`sudo snap install multipass`).
