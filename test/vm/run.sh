#!/usr/bin/env bash
# End-to-end test on a throwaway Multipass VM: fresh Ubuntu, install, smoke test.
#
#   test/vm/run.sh            fresh VM, install, smoke test, install again, smoke test
#   test/vm/run.sh --reuse    keep the existing VM and re-run against it
#   test/vm/run.sh --delete   delete the VM
#
# The VM is kept afterwards for inspection: multipass shell homecloud-test
set -euo pipefail

VM=${VM:-homecloud-test}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ANSWERS=${ANSWERS:-test/answers/selfsigned.env}
cd "$ROOT"

exists() { multipass info "$VM" >/dev/null 2>&1; }
in_vm()  { multipass exec "$VM" -- "$@"; }

case ${1:-} in
  --delete) exists && multipass delete --purge "$VM"; exit 0 ;;
  --reuse)  exists || { echo "no VM named $VM" >&2; exit 1; } ;;
  "")       exists && multipass delete --purge "$VM"
            multipass launch 26.04 --name "$VM" --cpus 4 --memory 8G --disk 40G ;;
  *)        echo "unknown argument: $1" >&2; exit 2 ;;
esac

echo "==> Building the controller"
make --no-print-directory build >/dev/null

echo "==> Copying the repo into the VM"
in_vm rm -rf homecloud
in_vm mkdir homecloud
# Tracked and new files that exist on disk, plus the built controller.
# Sent as one archive: piping binary data through `multipass exec` truncates it.
# The archive lives under the repo because the Multipass snap cannot read /tmp.
archive=$ROOT/bin/homecloud.tar
trap 'rm -f "$archive"' EXIT
{ git ls-files -co --exclude-standard | while read -r f; do [ -e "$f" ] && echo "$f"; done; echo bin/controller; } \
  | tar -c -f "$archive" -T -
multipass transfer "$archive" "$VM:homecloud.tar"
in_vm tar -x -f homecloud.tar -C homecloud
in_vm rm homecloud.tar

echo "==> Installing"
in_vm sudo homecloud/install.sh --answers "homecloud/$ANSWERS"

echo "==> Smoke test"
in_vm sudo bash -c 'cd homecloud && test/e2e/smoke.sh'

echo "==> Installing again (must change nothing and still pass)"
in_vm sudo homecloud/install.sh --answers "homecloud/$ANSWERS"
in_vm sudo bash -c 'cd homecloud && test/e2e/smoke.sh'
