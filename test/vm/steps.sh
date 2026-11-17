#!/usr/bin/env bash
# The end-to-end test as a tree of steps on one Multipass VM. Each green step
# is saved as a VM snapshot, and a step starts from its parent's snapshot, so
# a change reruns only the steps it affects, and a failure in one branch
# does not stop the others.
#
#   test/vm/steps.sh               run every branch, in parallel, each from its newest valid snapshot
#   test/vm/steps.sh talk agent    run only the branches up to these steps
#   test/vm/steps.sh --serial      run the branches one after another in one VM
#   test/vm/steps.sh --from login  rerun from `login`, ignoring its snapshot and those that grow from it
#   test/vm/steps.sh --list        show the steps and their snapshots
#   test/vm/steps.sh --clean       delete the VMs and their snapshots
#   LLM=cloud test/vm/steps.sh     use Ollama's cloud instead of the local Ollama
#
# Steps: base, install, login, platform, agent, deploy, talk, journey, ui, crud.
# The agent, talk, journey and ui steps talk to a real LLM: by default Ollama on this computer, which
# the VM reaches through its gateway (LOCAL_MODEL, default qwen3.8-64k);
# with LLM=cloud, Ollama's cloud with OLLAMA_API_KEY from this checkout's .env.
#
# A snapshot is valid while the files its step depends on, and those of every
# step on its way from base, are unchanged: each snapshot's comment records a
# fingerprint of them. Restoring a snapshot boots the VM, so every resumed step also tests
# a restart.
#
# After a green run, this computer's DNS points at the VM (test/vm/dns.sh) and
# Keycloak opens in the browser. OPEN=0 skips that.
set -euo pipefail

VM=${VM:-garrison-test}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ANSWERS=${ANSWERS:-test/answers/selfsigned.env}
cd "$ROOT"

# The steps form a tree: each starts from the snapshot of its parent, so
# steps that do not depend on each other are not chained, and a change to one
# reruns only it and what grows from it.
#
#   1 base - 2 install - 3 login -+- 4.1 platform
#                                 +- 4.2 agent
#                                 +- 4.3 deploy -+- 5.1 talk
#                                 |              +- 5.2 journey
#                                 |              +- 5.3 ui
#                                 +- 4.4 crud
STEPS=(base install login platform agent deploy talk journey ui crud)
declare -A PARENT=([base]="" [install]=base [login]=install [platform]=login [agent]=login [deploy]=login [talk]=deploy [journey]=deploy [ui]=deploy [crud]=login)
declare -A NUMBER=([base]=1 [install]=2 [login]=3 [platform]=4.1 [agent]=4.2 [deploy]=4.3 [talk]=5.1 [journey]=5.2 [ui]=5.3 [crud]=4.4)
# Where the tree forks. In parallel runs (the default) the trunk up to here
# runs in $VM, and each branch from here in its own clone, $VM-<first step>.
FORK=login

# Which LLM the agents use: local (Ollama on this computer, LOCAL_MODEL) or cloud.
LLM=${LLM:-local}
LOCAL_MODEL=${LOCAL_MODEL:-qwen3.8-64k:latest}
case $LLM in local|cloud) ;; *) echo "LLM must be local or cloud" >&2; exit 2 ;; esac

# Files each step depends on, besides this script's own definition of the step.
inputs() {
  case $1 in
    base)     ;;
    install)  echo install.sh installer garrison control-plane schemas templates go.mod go.sum host "$ANSWERS" test/e2e/add-test-users.sh ;;
    login)    echo test/e2e/smoke.sh ;;
    platform) echo test/e2e/smoke.sh test/e2e/stacks ;;
    agent)    echo test/e2e/smoke.sh ;;
    deploy)   echo test/e2e/smoke.sh examples installer/push-repo.sh ;;
    talk)     echo test/e2e/smoke.sh test/e2e/apps docs/scenarios.md ;;
    journey)  echo test/e2e/smoke.sh test/e2e/journey.sh ;;
    ui)       echo test/e2e/smoke.sh test/ui ;;
    crud)     echo test/e2e/smoke.sh test/e2e/crud.sh ;;
  esac
}

say()   { printf '\n==> [%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
in_vm() { multipass exec "$VM" -- "$@"; }
# By name in the list: info fails for a VM that is running but unreachable.
exists() { multipass list --format csv 2>/dev/null | cut -d, -f1 | grep -qx "$VM"; }

# ---------- the steps ----------

# A fresh Ubuntu VM with Docker, ZFS and the other base packages, and the repo.
step_base() {
  exists && multipass delete --purge "$VM"
  multipass launch 26.04 --name "$VM" --cpus 4 --memory 8G --disk 40G
  in_vm sudo bash -c 'export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq docker.io zfsutils-linux git curl openssl dnsmasq-base python3-yaml >/dev/null
    systemctl enable --now docker >/dev/null'
  sync_repo
}

# The installer, then the test people.
step_install() {
  sync_repo
  put_llm_key
  # LLM=local (the default): the agents use Ollama on this computer, which
  # the VM reaches through its gateway; nothing to pay for. LLM=cloud: Ollama's
  # cloud with OLLAMA_API_KEY from .env.
  if [ "$LLM" = local ]; then
    in_vm sudo bash -c "gw=\$(ip route | awk '/^default/ { print \$3; exit }')
      { echo LLM_BASE_URL=http://\$gw:11434/v1; echo LLM_MODEL=$LOCAL_MODEL; } >> garrison/$ANSWERS"
  fi
  in_vm sudo garrison/install.sh --answers "garrison/$ANSWERS"
  in_vm sudo bash -c 'cd garrison && test/e2e/add-test-users.sh'
  smoke system
}

# Signing in through Keycloak as admin, alice and bob, and what each may do.
step_login() {
  sync_repo
  smoke login
}

# The deployer deploying from Gitea, monitoring data, app stacks.
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

# A namespace with an app of its own, built by its pipeline on the user
# runner and deployed by the deployer: todo/app, and homepage/app, left running.
step_deploy() {
  sync_repo
  smoke deploy
}

# The scenarios of docs/scenarios.md: people ask the agent of todo to
# add and remove members, bob tries to raise his own rights, bob changes the
# app through the agent and carol merges. Talks to the real LLM.
step_talk() {
  sync_repo
  put_llm_key
  smoke talk
}

# Scenario 10 of docs/scenarios.md: alice has the agents make a namespace, a
# repo from a template and its stack, deploys it, changes it on dev through
# an agent and merges dev into main. Talks to the real LLM.
step_journey() {
  sync_repo
  put_llm_key
  smoke journey
}

# The browser scenarios (test/ui): people click and type in Gitea, Open WebUI
# and the dashboard, from a new namespace to a deployed web app, a
# screenshot, a scheduled job and a routine's question. Talks to the real
# LLM. The screenshots of every step land in bin/ui-shots/.
step_ui() {
  sync_repo
  put_llm_key
  local rc=0
  smoke ui || rc=$?
  in_vm sudo tar -C /var/tmp -cf /home/ubuntu/ui-shots.tar garrison-ui 2>/dev/null &&
    in_vm sudo chmod 644 /home/ubuntu/ui-shots.tar &&
    multipass transfer "$VM:ui-shots.tar" "$ROOT/bin/ui-shots.tar" &&
    rm -rf "$ROOT/bin/ui-shots" && mkdir -p "$ROOT/bin/ui-shots" &&
    tar -C "$ROOT/bin/ui-shots" --strip-components=1 -xf "$ROOT/bin/ui-shots.tar" || true
  return $rc
}

# Every kind of thing a namespace holds, made, read, changed and removed:
# the namespace itself, its people, repos, agents with grants, apps with
# their data, secrets and hostnames. No LLM.
step_crud() {
  sync_repo
  smoke crud
}

# ---------- helpers ----------

# Puts the LLM's key in /etc/garrison/llm-api-key in the VM, as a file: never
# on a command line, never in Git. A local Ollama needs none, so it gets a
# placeholder; the cloud gets OLLAMA_API_KEY from this checkout's .env.
put_llm_key() {
  local key file=$ROOT/bin/llm-api-key-$VM
  if [ "$LLM" = local ]; then
    key=ollama
  else
    [ -f "$ROOT/.env" ] || return 0
    key=$(sed -n 's/^OLLAMA_API_KEY=//p' "$ROOT/.env" | tr -d '"'"'"'\r' | head -n1)
    [ -n "$key" ] || return 0
  fi
  (umask 077; printf '%s' "$key" > "$file")
  multipass transfer "$file" "$VM:llm-api-key"
  rm -f "$file"
  in_vm sudo bash -c 'mkdir -p -m 700 /etc/garrison && touch /etc/garrison/llm-api-key && chmod 600 /etc/garrison/llm-api-key && cat ~ubuntu/llm-api-key > /etc/garrison/llm-api-key && rm ~ubuntu/llm-api-key'
}

smoke() {
  in_vm sudo bash -c "cd garrison && WAIT_READY=600 test/e2e/smoke.sh $*"
}

# Puts this checkout into the VM at ~/garrison: tracked and new files, never
# ignored ones such as .env, plus the garrison binary for the host.
sync_repo() {
  # Built once before parallel branches start (BUILT=1), never by two at once.
  [ -n "${BUILT:-}" ] || make --no-print-directory build >/dev/null
  local archive=$ROOT/bin/garrison-$VM.tar   # the Multipass snap cannot read /tmp; one per VM
  { git ls-files -co --exclude-standard | while read -r f; do [ -e "$f" ] && echo "$f"; done; echo bin/garrison; } \
    | tar -c -f "$archive" -T -
  in_vm rm -rf garrison garrison.tar
  multipass transfer "$archive" "$VM:garrison.tar"
  rm -f "$archive"
  in_vm mkdir garrison
  in_vm tar -x -f garrison.tar -C garrison
  in_vm rm garrison.tar
}

# path STEP: the steps from base to STEP.
path() {
  local s=$1 out=()
  while [ -n "$s" ]; do out=("$s" "${out[@]}"); s=${PARENT[$s]}; done
  echo "${out[@]}"
}

# descendants STEP: STEP and every step that grows from it.
descendants() {
  local s
  echo "$1"
  for s in "${STEPS[@]}"; do
    [ "${PARENT[$s]}" = "$1" ] && descendants "$s"
  done
}

# leaves: the steps no other step grows from.
leaves() {
  local s t child
  for s in "${STEPS[@]}"; do
    child=
    for t in "${STEPS[@]}"; do [ "${PARENT[$t]}" = "$s" ] && child=1; done
    [ -z "$child" ] && echo "$s"
  done
}

# fingerprint STEP: of the step's definition and inputs, chained with its
# parent's. Piped straight into sha256sum: no temporary file, so parallel
# branches cannot collide.
fingerprint() {
  local s prev= files
  for s in $(path "$1"); do
    prev=$({
      echo "$prev"
      declare -f "step_$s" sync_repo smoke put_llm_key
      [ "$s" = install ] && echo "LLM=$LLM LOCAL_MODEL=$LOCAL_MODEL"
      files=$(inputs "$s")
      # Without paths, git ls-files would list the whole repo.
      if [ -n "$files" ]; then
        # shellcheck disable=SC2086
        git ls-files -co --exclude-standard -- $files | sort | while read -r f; do
          [ -f "$f" ] && { echo "$f"; cat "$f"; }
        done
      fi
    } | sha256sum | cut -c1-16)
  done
  echo "$prev"
}

# The fingerprint stored with a step's snapshot, or nothing.
snapshot_of() {
  multipass list --snapshots --format csv 2>/dev/null |
    awk -F, -v vm="$VM" -v s="$1" '{ gsub(/"/, "") } $1 == vm && $2 == s { print $4 }'
}

valid() { local have; have=$(snapshot_of "$1"); [ -n "$have" ] && [ "$have" = "$(fingerprint "$1")" ]; }

# Multipass changes snapshots only while the VM is stopped.
save() {
  local step=$1 fp
  fp=$(fingerprint "$step")
  local t0=$SECONDS
  say "Saving snapshot ${NUMBER[$step]} $step"
  multipass stop "$VM"
  [ -n "$(snapshot_of "$step")" ] && multipass delete --purge "$VM.$step"
  multipass snapshot "$VM" --name "$step" --comment "$fp" >/dev/null
  multipass start "$VM"
  say "Snapshot ${NUMBER[$step]} $step took $((SECONDS - t0))s (stop, snapshot, boot)"
}

# drop STEP: deletes the snapshots of STEP and of everything that grows from it.
drop() {
  local s
  for s in $(descendants "$1"); do
    [ -n "$(snapshot_of "$s")" ] && multipass delete --purge "$VM.$s"
  done
  return 0
}

list() {
  local s have want state indent
  printf '%-18s %-18s %-18s %-8s %s\n' STEP SNAPSHOT CURRENT STATE VM
  for s in "${STEPS[@]}"; do
    local vm=$VM
    [ -n "$SERIAL" ] || vm=$(branch_vm "$s")
    have=$(VM=$vm snapshot_of "$s"); want=$(fingerprint "$s")
    if [ -z "$have" ]; then state=none; elif [ "$have" = "$want" ]; then state=valid; else state=stale; fi
    indent=$(printf '%*s' $(( ($(path "$s" | wc -w) - 1) * 2 )) '')
    printf '%-18s %-18s %-18s %-8s %s\n' "$indent${NUMBER[$s]} $s" "${have:--}" "$want" "$state" "$vm"
  done
}

# below_fork STEP: whether STEP grows from the fork (is not on the trunk).
below_fork() { [[ " $(path "$FORK") " != *" $1 "* ]]; }

# branch_vm STEP: the VM a step's snapshot lives in, in parallel runs.
branch_vm() {
  local s
  below_fork "$1" || { echo "$VM"; return; }
  for s in $(path "$1"); do
    if [ "${PARENT[$s]}" = "$FORK" ]; then echo "$VM-$s"; return; fi
  done
}

# readdress: a clone has a new IP address; the swarm is formed again on it,
# keeping its services, secrets and configs, and repaired like after a boot.
readdress() {
  in_vm sudo bash -c '
    ip=$(ip -4 route get 1.1.1.1 | sed -n "s/.* src \([0-9.]*\).*/\1/p")
    docker swarm init --force-new-cluster --advertise-addr "$ip" >/dev/null
    /usr/local/lib/garrison/repair-services.sh >/dev/null'
}

# settle: on a clone's first boot, some tasks start before the re-formed swarm
# has their names, and stay unreachable by name on internal networks (the user
# runner could not find the guard). Every service gets new tasks, once; then
# this waits until all run again.
settle() {
  in_vm sudo bash -c '
    for s in $(docker service ls -q); do docker service update --quiet --detach --force "$s" >/dev/null; done
    # The old tasks still count as running for a moment.
    sleep 15
    # A task can count as running while its container lost its network
    # endpoint (the old task'"'"'s cleanup raced the new one), so its name does
    # not resolve; those services are restarted again.
    detached() {
      for c in $(docker ps -q -f label=com.docker.swarm.service.name); do
        n=$(docker inspect "$c" --format "{{.Name}} {{range .NetworkSettings.Networks}}{{if not .EndpointID}}lost{{end}}{{end}}")
        # Named /SERVICE.SLOT.TASK
        case $n in *lost) n=${n#/}; echo "${n%%.*}" ;; esac
      done | sort -u
    }
    for i in $(seq 1 60); do
      if docker service ls --format "{{.Replicas}}" | awk -F"[/ ]" "\$1 != \$2 { bad = 1 } END { exit bad }"; then
        lost=$(detached)
        [ -z "$lost" ] && exit 0
        echo "restarting services without a network endpoint: $lost" >&2
        for s in $lost; do docker service update --quiet --detach --force "$s" >/dev/null; done
        sleep 15
      fi
      sleep 5
    done
    echo "services still starting after 5 minutes:" >&2
    docker service ls --format "{{.Name}} {{.Replicas}}" >&2
    exit 1'
}

# branch TARGET [FORK]: runs the steps from the newest valid snapshot on the
# way to TARGET up to TARGET, saving each. With FORK, only the steps after
# FORK are considered, and with none of them valid the VM is taken to be at
# FORK (a fresh clone). Returns non-zero if a step fails. Called with set +e;
# each step runs in a subshell with set -e.
branch() {
  local target=$1 fork=${2:-} s steps=() resume= rc
  read -r -a steps <<<"$(path "$target")"
  if [ -n "$fork" ]; then
    while [ "${steps[0]}" != "$fork" ]; do steps=("${steps[@]:1}"); done
    steps=("${steps[@]:1}")
  fi
  if exists; then
    for s in "${steps[@]}"; do
      valid "$s" || break
      resume=$s
    done
  fi
  if [ "$resume" = "$target" ]; then
    say "${NUMBER[$target]} $target is green already"
    return 0
  fi
  if [ -n "$resume" ]; then
    say "Restoring snapshot ${NUMBER[$resume]} $resume for ${NUMBER[$target]} $target"
    multipass stop "$VM" 2>/dev/null || true
    multipass restore --destructive "$VM.$resume" >/dev/null
    # What grows from here no longer matches what this run builds.
    for s in "${STEPS[@]}"; do
      [ "${PARENT[$s]}" = "$resume" ] && [[ " ${steps[*]} " == *" $s "* ]] && drop "$s"
    done
    multipass start "$VM"
  fi
  if [ -z "$resume" ] && [ -n "$fork" ]; then
    multipass start "$VM" >/dev/null 2>&1
    local t0=$SECONDS
    settle || return 1
    say "Restarted the clone's services in $((SECONDS - t0))s"
  fi
  local go=${resume:+no}
  for s in "${steps[@]}"; do
    if [ "$go" = no ]; then
      [ "$s" = "$resume" ] && go=yes
      continue
    fi
    say "Step ${NUMBER[$s]} $s"
    local t0=$SECONDS
    ( set -e; "step_$s" )
    rc=$?
    say "Step ${NUMBER[$s]} $s took $(( (SECONDS - t0) / 60 ))m$(( (SECONDS - t0) % 60 ))s"
    if [ $rc != 0 ]; then
      say "Step ${NUMBER[$s]} $s failed"
      return $rc
    fi
    save "$s"
  done
}

# ---------- main ----------

targets=() from= SERIAL=
while [ $# -gt 0 ]; do
  case $1 in
    --list)   shift; [ "${1:-}" = --serial ] && SERIAL=1; list; exit 0 ;;
    --clean)  for v in $(multipass list --format csv | awk -F, -v p="$VM" 'NR > 1 && ($1 == p || index($1, p "-") == 1) { print $1 }'); do
                multipass delete --purge "$v"; echo "deleted $v"
              done; exit 0 ;;
    --serial) SERIAL=1; shift ;;
    --from)   from=${2:?--from needs a step}; shift 2 ;;
    -*)       echo "unknown option: $1" >&2; exit 2 ;;
    *)        targets+=("$1"); shift ;;
  esac
done
[ ${#targets[@]} -gt 0 ] || read -r -a targets <<<"$(leaves | tr '\n' ' ')"
for s in "${targets[@]}" $from; do
  [ -n "${NUMBER[$s]:-}" ] || { echo "unknown step: $s (steps: ${STEPS[*]})" >&2; exit 2; }
done
if [ -n "$from" ]; then
  for s in $(descendants "$from"); do
    vm=$VM; [ -n "$SERIAL" ] || vm=$(branch_vm "$s")
    if VM=$vm exists && [ -n "$(VM=$vm snapshot_of "$s")" ]; then
      multipass stop "$vm" 2>/dev/null || true
      multipass delete --purge "$vm.$s"
    fi
  done
fi

failed=()
if [ -n "$SERIAL" ] || ! ( for t in "${targets[@]}"; do below_fork "$t" || exit 1; done ); then
  # One VM: each branch in turn. Not "branch || ...": bash would then ignore
  # set -e in every step, and a failing command would not stop its step.
  for t in "${targets[@]}"; do
    set +e
    branch "$t"
    rc=$?
    set -e
    [ $rc = 0 ] || failed+=("${NUMBER[$t]} $t")
  done
  last_vm=$VM
else
  # The trunk first, in $VM.
  set +e
  branch "$FORK"
  rc=$?
  set -e
  [ $rc = 0 ] || { say "The trunk failed at or before ${NUMBER[$FORK]} $FORK"; exit 1; }

  # The main VM holds only the trunk; snapshots below the fork live in the clones.
  multipass stop "$VM" 2>/dev/null || true
  for s in "${STEPS[@]}"; do
    below_fork "$s" && [ -n "$(snapshot_of "$s")" ] && multipass delete --purge "$VM.$s"
  done

  # Group the targets by branch VM; clone the ones whose first step is not valid.
  declare -A group=()
  for t in "${targets[@]}"; do
    v=$(branch_vm "$t"); group[$v]="${group[$v]:-} $t"
  done
  for v in "${!group[@]}"; do
    first=${v#"$VM-"}
    if VM=$v exists && VM=$v valid "$first"; then
      continue
    fi
    say "Cloning ${NUMBER[$FORK]} $FORK into $v"
    VM=$v exists && multipass delete --purge "$v"
    multipass stop "$VM" 2>/dev/null || true
    multipass restore --destructive "$VM.$FORK" >/dev/null
    multipass clone "$VM" --name "$v" >/dev/null
    multipass start "$v" >/dev/null
    VM=$v readdress
    multipass stop "$v" >/dev/null
  done
  multipass stop "$VM" 2>/dev/null || true

  # The branches, in parallel, each in its VM, each logging to bin/.
  make --no-print-directory build >/dev/null
  export BUILT=1
  declare -A pid=()
  for v in "${!group[@]}"; do
    log=$ROOT/bin/steps-${v#"$VM-"}.log
    # The steps of the branch that failed, one per line: a branch's other
    # steps may well be green.
    : > "${log%.log}.failed"
    say "Branch $v:${group[$v]} (log: ${log#"$ROOT/"})"
    (
      VM=$v
      rc=0
      for t in ${group[$v]}; do
        set +e
        branch "$t" "$FORK"
        r=$?
        set -e
        [ $r = 0 ] || { rc=$r; echo "${NUMBER[$t]} $t" >> "${log%.log}.failed"; }
      done
      exit $rc
    ) > "$log" 2>&1 &
    pid[$v]=$!
  done
  for v in "${!group[@]}"; do
    if wait "${pid[$v]}"; then
      say "Branch $v green"
    else
      say "Branch $v failed"
      log=$ROOT/bin/steps-${v#"$VM-"}.log
      grep -E '^(FAIL|==> .*Step .* failed)' -A2 "$log" | head -40
      while read -r t; do failed+=("$t"); done < "${log%.log}.failed"
      [ -s "${log%.log}.failed" ] || failed+=("${group[$v]# }")
    fi
  done
  # The browser goes to the branch with the apps, if it ran.
  last_vm=$(branch_vm "${targets[-1]}")
  for t in "${targets[@]}"; do [ "$t" = talk ] || [ "$t" = deploy ] && last_vm=$(branch_vm "$t"); done
  multipass start "$last_vm" >/dev/null 2>&1 || true
fi

echo
list
if [ ${#failed[@]} -gt 0 ]; then
  say "Failed: ${failed[*]}"
  exit 1
fi
say "Green: ${targets[*]}"
if [ "${OPEN:-1}" != 0 ]; then
  VM=$last_vm test/vm/dns.sh
  # shellcheck disable=SC1090
  url=https://keycloak.$(. "$ANSWERS" && echo "$DOMAIN")
  if command -v xdg-open >/dev/null && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    xdg-open "$url" >/dev/null 2>&1 &
    echo "Opened $url"
  else
    echo "Open $url"
  fi
fi
