#!/usr/bin/env bash
# Smoke test for an installed machine. Run on the machine, as root, from the repo root.
#
#   test/e2e/smoke.sh [SCENARIO...]   system, login and platform by default
#
# Each scenario is test/e2e/scenarios/<name>.sh, defining section_<name>, on
# the helpers of test/e2e/lib.sh. test/vm/steps.sh runs them as a tree of
# fixtures and scenarios on Multipass VMs.
set -uo pipefail
. test/e2e/lib.sh

sections=${*:-system login platform}
for s in $sections; do
  f=test/e2e/scenarios/$s.sh
  [ -f "$f" ] || { echo "unknown scenario: $s (test/e2e/scenarios/)" >&2; exit 2; }
  # shellcheck disable=SC1090
  . "$f"
  t0=$SECONDS
  "section_$s"
  echo "time  section $s took $(( (SECONDS - t0) / 60 ))m$(( (SECONDS - t0) % 60 ))s"
done
echo
if [ $failed -eq 0 ]; then echo "all checks passed"; else echo "$failed check(s) failed"; fi
exit $((failed > 0))
