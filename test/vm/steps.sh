#!/usr/bin/env bash
# The end-to-end test as a chain of steps on one Multipass VM. Each green step
# is saved as a VM snapshot, and the next run starts from the newest snapshot
# that is still valid, so a failure late in the chain does not mean
# reinstalling from scratch.
#
#   test/vm/steps.sh               run the chain, resuming from the newest valid snapshot
#   test/vm/steps.sh login         stop after the step `login`
#
# Steps: base, install, login, platform, agent. The agent step talks to the
# real LLM with OLLAMA_API_KEY from this checkout's .env; without it, it skips.
#   test/vm/steps.sh --from install    rerun from `install`, ignoring its snapshot and later ones
#   test/vm/steps.sh --list        show the steps and their snapshots
#   test/vm/steps.sh --clean       delete the VM and its snapshots
#
# A snapshot is valid while the files its step depends on, and those of every
# step before it, are unchanged: each snapshot's comment records a fingerprint
# of them. Restoring a snapshot boots the VM, so every resumed step also tests
# a restart.
#
# After a green run, this computer's DNS points at the VM (test/vm/dns.sh) and
# Keycloak opens in the browser. OPEN=0 skips that.
set -euo pipefail

VM=${VM:-homecloud-test}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ANSWERS=${ANSWERS:-test/answers/selfsigned.env}
cd "$ROOT"

STEPS=(base install login platform agent)

# Files each step depends on, besides this script's own definition of the step.
inputs() {
  case $1 in
    base)     ;;
    install)  echo install.sh installer system controller schemas go.mod go.sum host "$ANSWERS" test/e2e/add-test-users.sh ;;
    login)    echo test/e2e/smoke.sh ;;
    platform) echo test/e2e/smoke.sh test/e2e/stacks ;;
    agent)    echo test/e2e/smoke.sh ;;
  esac
}

say()   { printf '\n==> %s\n' "$*"; }
in_vm() { multipass exec "$VM" -- "$@"; }
exists() { multipass info "$VM" >/dev/null 2>&1; }

# ---------- the steps ----------

# A fresh Ubuntu VM with Docker, ZFS and the other base packages, and the repo.
step_base() {
  exists && multipass delete --purge "$VM"
  multipass launch 26.04 --name "$VM" --cpus 4 --memory 8G --disk 40G
  in_vm sudo bash -c 'export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq docker.io zfsutils-linux git curl openssl dnsmasq-base >/dev/null
    systemctl enable --now docker >/dev/null'
  sync_repo
}

# The installer, then the two test people.
step_install() {
  sync_repo
  put_llm_key
  in_vm sudo homecloud/install.sh --answers "homecloud/$ANSWERS"
  in_vm sudo bash -c 'cd homecloud && test/e2e/add-test-users.sh'
  smoke system
}

# Signing in through Keycloak as admin, alice and bob, and what each may do.
step_login() {
  sync_repo
  smoke login
}

# The controller deploying from Gitea, monitoring data, app stacks.
step_platform() {
  sync_repo
  smoke platform
}

# The operator agent talking to the real LLM, with alice's and bob's rights.
step_agent() {
  sync_repo
  put_llm_key
  smoke agent
}

# ---------- helpers ----------

# Copies OLLAMA_API_KEY from this checkout's .env to /etc/homecloud/llm-api-key
# in the VM, as a file: never on a command line, never in Git.
put_llm_key() {
  local key file=$ROOT/bin/llm-api-key
  [ -f "$ROOT/.env" ] || return 0
  key=$(sed -n 's/^OLLAMA_API_KEY=//p' "$ROOT/.env" | tr -d '"'"'"'\r' | head -n1)
  [ -n "$key" ] || return 0
  (umask 077; printf '%s' "$key" > "$file")
  multipass transfer "$file" "$VM:llm-api-key"
  rm -f "$file"
  in_vm sudo bash -c 'mkdir -p -m 700 /etc/homecloud && touch /etc/homecloud/llm-api-key && chmod 600 /etc/homecloud/llm-api-key && cat ~ubuntu/llm-api-key > /etc/homecloud/llm-api-key && rm ~ubuntu/llm-api-key'
}

smoke() {
  in_vm sudo bash -c "cd homecloud && WAIT_READY=600 test/e2e/smoke.sh $*"
}

# Puts this checkout into the VM at ~/homecloud: tracked and new files, never
# ignored ones such as .env, plus the controller binary for the host.
sync_repo() {
  make --no-print-directory build >/dev/null
  local archive=$ROOT/bin/homecloud.tar   # the Multipass snap cannot read /tmp
  { git ls-files -co --exclude-standard | while read -r f; do [ -e "$f" ] && echo "$f"; done; echo bin/controller; } \
    | tar -c -f "$archive" -T -
  in_vm rm -rf homecloud homecloud.tar
  multipass transfer "$archive" "$VM:homecloud.tar"
  rm -f "$archive"
  in_vm mkdir homecloud
  in_vm tar -x -f homecloud.tar -C homecloud
  in_vm rm homecloud.tar
}

# fingerprint STEP: of the step's definition and inputs, chained with the step before.
fingerprint() {
  local step=$1 prev=
  for s in "${STEPS[@]}"; do
    {
      echo "$prev"
      declare -f "step_$s" sync_repo smoke
      local files
      files=$(inputs "$s")
      # Without paths, git ls-files would list the whole repo.
      if [ -n "$files" ]; then
        # shellcheck disable=SC2086
        git ls-files -co --exclude-standard -- $files | sort | while read -r f; do
          [ -f "$f" ] && { echo "$f"; cat "$f"; }
        done
      fi
    } > "$ROOT/bin/fingerprint"
    prev=$(sha256sum < "$ROOT/bin/fingerprint" | cut -c1-16)
    [ "$s" = "$step" ] && break
  done
  rm -f "$ROOT/bin/fingerprint"
  echo "$prev"
}

# The fingerprint stored with a step's snapshot, or nothing.
snapshot_of() {
  multipass list --snapshots --format csv 2>/dev/null |
    awk -F, -v vm="$VM" -v s="$1" '{ gsub(/"/, "") } $1 == vm && $2 == s { print $4 }'
}

save() {
  local step=$1 fp
  fp=$(fingerprint "$step")
  say "Saving snapshot $step"
  multipass stop "$VM"
  [ -n "$(snapshot_of "$step")" ] && multipass delete --purge "$VM.$step"
  multipass snapshot "$VM" --name "$step" --comment "$fp" >/dev/null
  multipass start "$VM"
}

drop_from() { # delete the snapshots of a step and every later one
  local found=
  for s in "${STEPS[@]}"; do
    [ "$s" = "$1" ] && found=1
    [ -n "$found" ] && [ -n "$(snapshot_of "$s")" ] && multipass delete --purge "$VM.$s"
  done
  return 0
}

list() {
  printf '%-10s %-18s %-18s %s\n' STEP SNAPSHOT CURRENT STATE
  for s in "${STEPS[@]}"; do
    local have want state
    have=$(snapshot_of "$s"); want=$(fingerprint "$s")
    if [ -z "$have" ]; then state=none; elif [ "$have" = "$want" ]; then state=valid; else state=stale; fi
    printf '%-10s %-18s %-18s %s\n' "$s" "${have:--}" "$want" "$state"
  done
}

# ---------- main ----------

target=${STEPS[-1]} from=
case ${1:-} in
  --list)  list; exit 0 ;;
  --clean) exists && multipass delete --purge "$VM"; exit 0 ;;
  --from)  from=${2:?--from needs a step}; target=${3:-$target} ;;
  "")      ;;
  -*)      echo "unknown option: $1" >&2; exit 2 ;;
  *)       target=$1 ;;
esac
for s in "$target" $from; do
  printf '%s\n' "${STEPS[@]}" | grep -qx "$s" || { echo "unknown step: $s (steps: ${STEPS[*]})" >&2; exit 2; }
done

# The newest valid snapshot before --from, or before the target. Multipass
# changes snapshots only while the VM is stopped.
start=0 resume=
if exists; then
  multipass stop "$VM" 2>/dev/null || true
  [ -n "$from" ] && drop_from "$from"
  for i in "${!STEPS[@]}"; do
    s=${STEPS[$i]}
    [ -n "$(snapshot_of "$s")" ] && [ "$(snapshot_of "$s")" = "$(fingerprint "$s")" ] || break
    resume=$s start=$((i + 1))
    [ "$s" = "$target" ] && break
  done
  if [ -n "$resume" ]; then
    say "Restoring snapshot $resume"
    multipass restore --destructive "$VM.$resume" >/dev/null
    # Later snapshots no longer match what this run will build.
    [ $start -lt ${#STEPS[@]} ] && drop_from "${STEPS[$start]}"
  fi
  multipass start "$VM"
fi

for ((i = start; i < ${#STEPS[@]}; i++)); do
  s=${STEPS[$i]}
  say "Step $s"
  "step_$s"
  save "$s"
  [ "$s" = "$target" ] && break
done

say "Green up to $target"
if [ "${OPEN:-1}" != 0 ]; then
  test/vm/dns.sh
  # shellcheck disable=SC1090
  url=https://keycloak.$(. "$ANSWERS" && echo "$DOMAIN")
  if command -v xdg-open >/dev/null && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    xdg-open "$url" >/dev/null 2>&1 &
    echo "Opened $url"
  else
    echo "Open $url"
  fi
fi
