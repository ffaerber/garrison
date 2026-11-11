# Tests

End-to-end tests on throwaway VMs. The MVP acceptance list in `docs/spec.md` is the target suite.

```
make test            # unit tests, in a container
test/vm/run.sh       # fresh Multipass VM, install, smoke test, install again, smoke test
test/vm/run.sh --reuse    # re-run against the existing VM
test/vm/run.sh --delete   # delete the VM
```

## Opening the test installation in a browser

```
test/vm/dns.sh          # *.homecloud.test resolves to the VM from this computer
test/vm/dns.sh --off    # undo
multipass exec homecloud-test -- sudo cat /etc/homecloud/admin-password
multipass exec homecloud-test -- sudo cat /etc/homecloud/test-users/alice   # root admin; bob is a regular user
```

Then open `https://git.homecloud.test`, `https://keycloak.homecloud.test` or any deployed stack. The certificate is self-signed, so the browser asks once per hostname. The DNS setting lasts until a reboot or until the VM is recreated; run the script again then. It needs systemd-resolved on this computer.

`test/vm/run.sh` needs Multipass (`sudo snap install multipass`). The VM is named `homecloud-test` and is kept after a run for inspection.

`test/e2e/smoke.sh` runs on the installed machine and checks the system stack, a stack deploy through the controller, and hostname claims.
