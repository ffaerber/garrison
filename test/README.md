# Tests

End-to-end tests on throwaway VMs. The MVP acceptance list in `docs/spec.md` is the target suite.

```
make test            # unit tests, in a container
test/vm/run.sh       # fresh Multipass VM, install, smoke test, install again, smoke test
test/vm/run.sh --reuse    # re-run against the existing VM
test/vm/run.sh --delete   # delete the VM
```

`test/vm/run.sh` needs Multipass (`sudo snap install multipass`). The VM is named `homecloud-test` and is kept after a run for inspection.

`test/e2e/smoke.sh` runs on the installed machine and checks the system stack, a stack deploy through the controller, and hostname claims.
