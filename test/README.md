# Tests

End-to-end tests on a throwaway Multipass VM. The MVP acceptance list in `docs/spec.md` is the target suite.

```
make test                        # unit tests, in a container
test/vm/steps.sh                 # the end-to-end chain, resuming from the newest valid snapshot
test/vm/steps.sh login           # stop after the step `login`
test/vm/steps.sh --from install  # rerun from `install`
test/vm/steps.sh --list          # steps, their snapshots, and whether those are still valid
test/vm/steps.sh --clean         # delete the VM and its snapshots
```

## The chain

| Step | Does | Starts from |
| --- | --- | --- |
| `base` | Fresh Ubuntu VM with Docker, ZFS, git and the other base packages, and this repo at `~/homecloud` | nothing |
| `install` | Runs the installer, adds the test people, checks the system stack (`smoke.sh system`) | `base` |
| `login` | Signs in through Keycloak as admin, alice and bob, to Gitea, Grafana and Open WebUI, and checks what each may do (`smoke.sh login`) | `install` |
| `platform` | The controller deploying from Gitea, monitoring data, app stacks, private environments, hostname claims (`smoke.sh platform`) | `login` |
| `agent` | Bob and alice chat with the agent of `system` in Open WebUI: it answers from the control plane's tools as them, reads logs and metrics, refuses bob's proposal and opens alice's as a pull request (`smoke.sh agent`). Talks to the real LLM with `OLLAMA_API_KEY` from this checkout's `.env`; skips without it | `platform` |

Every green step is saved as a VM snapshot named after it. A snapshot stays valid while the files its step depends on, and those of every earlier step, are unchanged; `--list` shows which are. Change the smoke checks and the chain resumes from `install`; change the installer and it resumes from `base`. Restoring a snapshot boots the VM, so every resumed step also tests a restart.

Each step copies this checkout into the VM first: tracked and new files, never ignored ones such as `.env`.

## The test people

`test/e2e/add-test-users.sh` creates two people with the password `12345678`: `alice`, a root admin, and `bob`, a regular user.

## Opening the test installation in a browser

A green run points this computer's DNS at the VM and opens Keycloak (`OPEN=0` skips that). By hand:

```
test/vm/dns.sh          # *.homecloud.test resolves to the VM from this computer
test/vm/dns.sh --off    # undo
multipass exec homecloud-test -- sudo cat /etc/homecloud/admin-password
```

Then open `https://chat.homecloud.test`, `https://git.homecloud.test`, `https://grafana.homecloud.test`, `https://keycloak.homecloud.test` or any deployed stack. The certificate is self-signed, so the browser asks once per hostname. The DNS setting lasts until a reboot or until the VM is recreated. It needs systemd-resolved on this computer.

## Getting changes onto an installed machine

`test/vm/push-repos.sh`, run in the VM, commits `system/` to the repos of the `system` organisation and waits for the controller to deploy them, the way an admin's push would. A changed controller is built by Gitea Actions first.

`test/vm/steps.sh` needs Multipass (`sudo snap install multipass`).
