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
| `base` | Fresh Ubuntu VM with Docker, ZFS, git and the other base packages, and this repo at `~/homecloud` | nothing |
| `install` | Runs the installer, adds the test people, checks the system stack (`smoke.sh system`) | `base` |
| `login` | Signs in through Keycloak as admin, alice and bob, to Gitea, Grafana and Open WebUI, and checks what each may do (`smoke.sh login`) | `install` |
| `platform` | The deployer deploying from Gitea, monitoring data, dashboards from Git (every panel's query runs, a committed dashboard appears, an edit in Grafana is put back), app stacks, private environments, hostname claims (`smoke.sh platform`) | `login` |
| `agent` | Bob and alice mention the agent of `system` in its channel in Open WebUI: it answers from the gateway's tools as them, reads logs and metrics, refuses bob's proposal and opens alice's as a pull request and deletes its messages afterwards (`smoke.sh agent`). Talks to the real LLM: local Ollama by default, Ollama's cloud with `LLM=cloud` | `platform` |
| `deploy` | Alice creates the namespace `todo` (bob may not); alice and bob each sign in to todo and see only their own todos; it gets a build account; `examples/todo/app` is pushed to `todo/app`; its pipeline tests, builds and pushes the image on the user runner; the deployer deploys `todo` and a `dev` branch as `dev-todo`, each with its own database; a failing test keeps the last good commit running; a job asking for the host's processes is refused; a reserved hostname is refused; deleting `dev` takes `dev-todo` down. Meanwhile the namespace `homepage` gets `examples/homepage/app`, a hello-world page, deployed at `homepage.<domain>` for everyone. Leaves `todo` and `homepage` running (`smoke.sh deploy`) | `agent` |
| `talk` | The scenarios of `docs/scenarios.md` against the real LLM, in the namespace `todo`: alice adds carol and bob through the agent and a root admin approves; bob tries to make himself an admin or root admin; carol's approval of her own demotion does not count; bob changes the todo app through the agent and carol merges, and it deploys; alice removes bob (`smoke.sh talk`) | `deploy` |
| `crud` | Every kind of thing a namespace holds, made, read, changed and removed without the LLM: alice makes the namespace `crud` (config, build account, Open WebUI group, channel and operator follow) and changes it; adds bob as member, makes him admin and removes him (teams, push rights and channel follow); makes, hides and deletes repos; adds an agent, changes it, grants it a repo (its account may write there and nowhere else; a private repo hides it from bob), takes the grant away (its account is locked) and removes it; adds an app, changes its setting and dataset size (snapshot first), removes it (hostname freed, data and secret kept); changes the approval check refuses. Then deletes the namespace with an app and an agent account: the app goes down, its channel, group, agents and build account go, the agent's account is locked; made again, the namespace starts fresh (`smoke.sh crud`) | `login` |

Every green step is saved as a VM snapshot named after it. A snapshot stays valid while the files its step depends on, and those of every earlier step, are unchanged; `--list` shows which are. Change the smoke checks and the chain resumes from `install`; change the installer and it resumes from `base`. Restoring a snapshot boots the VM, so every resumed step also tests a restart.

Each step copies this checkout into the VM first: tracked and new files, never ignored ones such as `.env`.

## The test people

`test/e2e/add-test-users.sh` creates three people with the password `12345678`: `alice`, a root admin, and `bob` and `carol`, regular users.

## Opening the test installation in a browser

A green run points this computer's DNS at the VM and opens Keycloak (`OPEN=0` skips that). By hand:

```
test/vm/dns.sh          # *.homecloud.test resolves to the VM from this computer
test/vm/dns.sh --off    # undo
multipass exec homecloud-test -- sudo cat /etc/homecloud/admin-password
```

Then open `https://chat.homecloud.test`, `https://git.homecloud.test`, `https://grafana.homecloud.test`, `https://keycloak.homecloud.test` or any deployed stack. The certificate is self-signed, so the browser asks once per hostname. The DNS setting lasts until a reboot or until the VM is recreated. It needs systemd-resolved on this computer.

## Getting changes onto an installed machine

`test/vm/push-repos.sh`, run in the VM, commits `system/` to the repos of the `system` organisation and waits for the deployer to deploy them, the way an admin's push would. A changed controller is built by Gitea Actions first.

`test/vm/steps.sh` needs Multipass (`sudo snap install multipass`).
