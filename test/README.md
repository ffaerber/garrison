# Tests

End-to-end tests on a throwaway Multipass VM. The MVP acceptance list in `docs/spec.md` is the target suite.

```
make test                        # unit tests, in a container
test/vm/steps.sh                 # the end-to-end chain, resuming from the newest valid snapshot
test/vm/steps.sh login           # stop after the step `login`
test/vm/steps.sh --from install  # rerun from `install`
test/vm/steps.sh --list          # steps, their snapshots, and whether those are still valid
test/vm/steps.sh --clean         # delete the VM and its snapshots
LLM=cloud test/vm/steps.sh       # the agents on Ollama's cloud instead of the local Ollama
LOCAL_MODEL=nemotron-3.5-lightning:latest test/vm/steps.sh   # another local model
```

The agents run on Ollama on this computer by default (`qwen3.8-64k`), reached from the VM through its gateway; Ollama must listen on more than localhost (`OLLAMA_HOST=0.0.0.0`). No key is needed.

## The chain

| Step | Does | Starts from |
| --- | --- | --- |
| `base` | Fresh Ubuntu VM with Docker, ZFS, git and the other base packages, and this repo at `~/garrison` | nothing |
| `install` | Runs the installer, adds the test people, checks the system stack (`smoke.sh system`) | `base` |
| `login` | Signs in through Keycloak as admin, alice and bob, to Gitea, Grafana and Open WebUI, and checks what each may do (`smoke.sh login`) | `install` |
| `platform` | The deployer deploying from Gitea, monitoring data, dashboards from Git (every panel's query runs, a committed dashboard appears, an edit in Grafana is put back), app stacks, private environments, hostname claims, the model sync with a fake provider, and the dashboard's Models page: alice adds a provider with its key (bob and other sites may not), it is proposed and its key encrypted in `garrison/config`, the deployer writes the key file and removes it once the key is deleted (`smoke.sh platform`) | `login` |
| `agent` | Bob and alice mention the agent of `garrison` in its channel in Open WebUI: it answers from the gateway's tools as them, reads logs and metrics, refuses bob's proposal and opens alice's as a pull request, proposes an LLM provider for alice and sends her to the Models page for its key, and deletes its messages afterwards (`smoke.sh agent`). Talks to the real LLM: local Ollama by default, Ollama's cloud with `LLM=cloud` | `login` |
| `deploy` | Alice creates the namespace `todo` (bob may not); alice and bob each sign in to todo and see only their own todos; it gets a build account; the app template `todo` (`templates/app/todo`) is pushed to `todo/app`, and the stack its README gives to `todo/config`; its pipeline tests, builds and pushes the image on the user runner; the deployer deploys `todo` and a `dev` branch as `dev-todo`, each with its own database; a failing test keeps the last good commit running; a job asking for the host's processes is refused; a reserved hostname is refused; deleting `dev` takes `dev-todo` down. Leaves `todo` running (`smoke.sh deploy`) | `login` |
| `talk` | The scenarios of `docs/scenarios.md` against the real LLM, in the namespace `todo`: alice adds carol and bob through the agent and a root admin approves; bob tries to make himself an admin or root admin; carol's approval of her own demotion does not count; bob changes the todo app through the agent and carol merges, and it deploys; alice removes bob (`smoke.sh talk`) | `deploy` |
| `journey` | Scenario 10 of `docs/scenarios.md` against the real LLM: alice has the operator make the namespace `lab` and in it the repo `example` from the template `todo`, proposes its stack, merges it on its card, makes `dev`, has the operator turn dev's background green and merges dev into main (`smoke.sh journey`) | `deploy` |
| `media` | A namespace's media library and its file browser, as the namespace's README gives it; a fake ComfyUI outside Garrison; alice has `@media` make two pictures and a video in the styles of `media/demo` (ComfyUI gets each style's prompt, size and length in their nodes; each file has its `.json`), then a 9:16 short from them; in the file browser bob looks but may not upload, alice uploads and her agents see it, nobody makes a public link, and nothing past the sign-in gets in (`smoke.sh media`). Talks to the real LLM | `agent` |
| `crud` | Every kind of thing a namespace holds, made, read, changed and removed without the LLM: alice makes the namespace `crud` (config, build account, Open WebUI group, channel and operator follow) and changes it; adds bob as member, makes him admin and removes him (teams, push rights and channel follow); makes, hides and deletes repos; adds an agent, changes it, grants it a repo (its account may write there and nowhere else; a private repo hides it from bob), takes the grant away (its account is locked) and removes it; adds an app, changes its setting and dataset size (snapshot first), removes it (hostname freed, data and secret kept); changes the approval check refuses. Then deletes the namespace with an app and an agent account: the app goes down, its channel, group, agents and build account go, the agent's account is locked; made again, the namespace starts fresh (`smoke.sh crud`) | `login` |
| `ui` | The browser scenarios of `docs/scenarios.md` (`test/ui`), clicked and typed in Gitea, Open WebUI and the dashboard as alice, bob and the admin: the namespace `todo` and its todo app, built as `deploy` builds it, a dev branch, a screenshot, a second person, routines and a scheduled job. Its screenshots land in `bin/ui-shots/`, where `docs/walkthrough/make.py` takes the walkthrough's pictures from (`smoke.sh ui`) | `crud` |

Every green step is saved as a VM snapshot named after it. A snapshot stays valid while the files its step depends on, and those of every earlier step, are unchanged; `--list` shows which are. Change the smoke checks and the chain resumes from `install`; change the installer and it resumes from `base`. Restoring a snapshot boots the VM, so every resumed step also tests a restart.

Each step copies this checkout into the VM first: tracked and new files, never ignored ones such as `.env`.

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
