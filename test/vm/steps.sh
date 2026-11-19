#!/usr/bin/env bash
# The end-to-end test as a tree of steps on Multipass VMs. A fixture is a
# state that scenarios start from (Garrison installed, a namespace with its
# app deployed): green, it is saved as a VM snapshot. A scenario is a test
# that starts from its fixture: it runs in a clone of the fixture's VM, which
# is thrown away when it passes and kept, stopped, when it fails. So a change
# reruns only the fixtures and scenarios it affects, a failure in one does not
# stop the others, and up to JOBS VMs run at once.
#
#   test/vm/steps.sh               every scenario that is not green yet
#   test/vm/steps.sh talk media    only these, and the fixtures they start from
#   test/vm/steps.sh --from login  rerun from `login`: it and all that grows from it
#   test/vm/steps.sh --list        the tree, and what is green, valid or stale
#   test/vm/steps.sh --clean       delete the VMs, their snapshots and the green marks
#   JOBS=2 test/vm/steps.sh        at most 2 VMs at once (default 4)
#   LLM=cloud test/vm/steps.sh     use Ollama's cloud instead of the local Ollama
#
# The scenarios that talk to a real LLM (agent, media, talk, journey, ui) use by
# default Ollama on this computer, which the VM reaches through its gateway
# (LOCAL_MODEL, default qwen3.8-64k); with LLM=cloud, Ollama's cloud with
# OLLAMA_API_KEY from this checkout's .env.
#
# A fixture's snapshot, or a scenario's green mark (bin/green/<step>), is
# valid while the files its step depends on, and those of every step on its
# way from base, are unchanged: each records a fingerprint of them. Starting
# from a snapshot boots the VM, so every step also tests a restart.
#
# After a green run, this computer's DNS points at the VM with the apps
# (test/vm/dns.sh) and Keycloak opens in the browser. OPEN=0 skips that.
set -euo pipefail

VM=${VM:-garrison-test}
BASE_VM=$VM   # VM changes to the VM of each job; names come from this
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ANSWERS=${ANSWERS:-test/answers/selfsigned.env}
JOBS=${JOBS:-4}
cd "$ROOT"

# The tree. Fixtures [F] are saved as snapshots; the others are scenarios.
#
#   1 base [F] - 2 install [F] - 3 login [F] -+- 4.1 platform
#                                             +- 4.2 crud
#                                             +- 4.3 ui
#                                             +- 4.4 agent [F] --- 5.1 media
#                                             +- 4.5 deploy [F] -+- 5.2 talk
#                                                                +- 5.3 journey
#
# The trunk (base, install, login) lives in $VM; every other fixture and
# scenario in a VM of its own, $VM-<step>, cloned from its parent's.
STEPS=(base install login platform crud ui agent media deploy talk journey)
declare -A PARENT=([base]="" [install]=base [login]=install [platform]=login [crud]=login [ui]=login
  [agent]=login [media]=agent [deploy]=login [talk]=deploy [journey]=deploy)
declare -A NUMBER=([base]=1 [install]=2 [login]=3 [platform]=4.1 [crud]=4.2 [ui]=4.3
  [agent]=4.4 [media]=5.1 [deploy]=4.5 [talk]=5.2 [journey]=5.3)
FIXTURES=" base install login agent deploy "
TRUNK=" base install login "

# Which LLM the agents use: local (Ollama on this computer, LOCAL_MODEL) or cloud.
LLM=${LLM:-local}
LOCAL_MODEL=${LOCAL_MODEL:-qwen3.8-64k:latest}
case $LLM in local|cloud) ;; *) echo "LLM must be local or cloud" >&2; exit 2 ;; esac

# Files each step depends on, besides this script's own definition of the step.
inputs() {
  case $1 in
    base)     ;;
    install)  echo install.sh installer garrison control-plane schemas templates files sandbox go.mod go.sum host "$ANSWERS" test/e2e/add-test-users.sh ;;
    login)    echo test/e2e/smoke.sh ;;
    platform) echo test/e2e/smoke.sh test/e2e/stacks ;;
    agent)    echo test/e2e/smoke.sh ;;
    deploy)   echo test/e2e/smoke.sh templates/app test/e2e/apps/quotes installer/push-repo.sh ;;
    talk)     echo test/e2e/smoke.sh test/e2e/apps docs/scenarios.md ;;
    journey)  echo test/e2e/smoke.sh test/e2e/journey.sh ;;
    ui)       echo test/e2e/smoke.sh test/ui ;;
    crud)     echo test/e2e/smoke.sh test/e2e/crud.sh ;;
    media)    echo test/e2e/smoke.sh test/e2e/media.sh test/e2e/media templates/namespace-config/README.md ;;
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
# runner and deployed by the deployer: todo/app, from the template todo, left running.
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
# and the dashboard, from the namespace todo to its todo app deployed, a
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

# A namespace's media library and its file browser; @media makes pictures
# and a video with a fake ComfyUI outside Garrison, then cuts a short.
# Talks to the real LLM.
step_media() {
  sync_repo
  put_llm_key
  smoke media
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
  # A guest that does not power off within Multipass's five minutes (it
  # happens, with many containers) is forced off once its disks are synced:
  # without a stop there is no snapshot, and later steps have nothing to
  # start from.
  in_vm sudo sync || true
  if ! multipass stop "$VM"; then
    say "The VM did not stop; forcing it off"
    multipass stop --force "$VM"
  fi
  [ -n "$(snapshot_of "$step")" ] && multipass delete --purge "$VM.$step"
  multipass snapshot "$VM" --name "$step" --comment "$fp" >/dev/null
  multipass start "$VM"
  say "Snapshot ${NUMBER[$step]} $step took $((SECONDS - t0))s (stop, snapshot, boot)"
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


# ---------- the tree ----------

is_fixture() { [[ $FIXTURES == *" $1 "* ]]; }
in_trunk()   { [[ $TRUNK == *" $1 "* ]]; }
# vm_of STEP: the VM a fixture's snapshot, or a scenario's run, lives in.
vm_of() { if in_trunk "$1"; then echo "$BASE_VM"; else echo "$BASE_VM-$1"; fi; }
children() { local s; for s in "${STEPS[@]}"; do [ "${PARENT[$s]}" = "$1" ] && echo "$s"; done; return 0; }
green_of() { cat "$ROOT/bin/green/$1" 2>/dev/null || true; }
needed() { [[ $NEEDED == *" $1 "* ]]; }
minutes() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

# status STEP STATE: records how a step went, for the summary.
status() {
  echo "$2" > "$ROOT/bin/state/$1"
  say "${NUMBER[$1]} $1: $2"
}

# skip_below STEP: what grows from a failed fixture does not run.
skip_below() {
  local c
  for c in $(children "$1"); do
    needed "$c" || continue
    status "$c" "skipped (${NUMBER[$1]} $1 failed)"
    skip_below "$c"
  done
}

# A pool of JOBS slots: a job holds one while its VM runs.
acquire() {
  local i
  while :; do
    for i in $(seq "$JOBS"); do
      mkdir "$ROOT/bin/slots/$i" 2>/dev/null && { SLOT=$i; return 0; }
    done
    sleep 3
  done
}
release() { rmdir "$ROOT/bin/slots/$SLOT" 2>/dev/null || true; }

# clone_from STEP DEST: DEST becomes a copy of the fixture STEP's VM at its
# snapshot. One clone at a time per source, which is stopped for it.
clone_from() {
  local src; src=$(vm_of "$1")
  (
    flock 9
    multipass stop "$src" >/dev/null 2>&1 || true
    multipass restore --destructive "$src.$1" >/dev/null
    multipass clone "$src" --name "$2" >/dev/null
  ) 9>"$ROOT/bin/lock-$src"
}

# fresh STEP: $VM becomes a new clone of STEP's parent, booted, with its
# swarm formed on the clone's address and every service running again.
fresh() {
  exists && multipass delete --purge "$VM"
  say "Cloning ${NUMBER[${PARENT[$1]}]} ${PARENT[$1]} into $VM"
  clone_from "${PARENT[$1]}" "$VM" || return 1
  multipass start "$VM" >/dev/null || return 1
  readdress || return 1
  local t0=$SECONDS
  settle || return 1
  say "Restarted the clone's services in $((SECONDS - t0))s"
}

# run_step STEP: the step in $VM, timed. Not called after || or &&: bash
# would then ignore set -e in the step, and a failing command would not stop it.
run_step() {
  local t0=$SECONDS rc
  say "Step ${NUMBER[$1]} $1"
  ( set -e; "step_$1" )
  rc=$?
  echo $((SECONDS - t0)) > "$ROOT/bin/times/$1"
  say "Step ${NUMBER[$1]} $1 took $(minutes $((SECONDS - t0)))"
  return $rc
}

# trunk LAST: base, install and login, in $VM, from the newest valid snapshot,
# up to LAST.
trunk() {
  local last=$1 s resume= steps=() rc
  read -r -a steps <<<"$(path "$last")"
  if exists; then
    for s in "${steps[@]}"; do
      valid "$s" || break
      resume=$s
    done
  fi
  if [ "$resume" = "$last" ]; then
    status "$last" "green already"
    return 0
  fi
  if [ -n "$resume" ]; then
    say "Restoring snapshot ${NUMBER[$resume]} $resume"
    multipass stop "$VM" 2>/dev/null || true
    multipass restore --destructive "$VM.$resume" >/dev/null
    multipass start "$VM"
  fi
  local go=${resume:+no}
  for s in "${steps[@]}"; do
    if [ "$go" = no ]; then
      [ "$s" = "$resume" ] && go=yes
      continue
    fi
    run_step "$s"
    rc=$?
    if [ $rc != 0 ]; then
      status "$s" failed
      return $rc
    fi
    save "$s"
    status "$s" green
  done
}

# grow FIXTURE: its fixtures and scenarios that are needed, each in a job of
# its own; waits for them all.
grow() {
  local c pids=() rc=0 p
  for c in $(children "$1"); do
    needed "$c" || continue
    if is_fixture "$c"; then fixture_job "$c" & else scenario_job "$c" & fi
    pids+=($!)
  done
  for p in "${pids[@]}"; do wait "$p" || rc=1; done
  return $rc
}

# fixture_job STEP: a fixture below the trunk, kept from its snapshot when
# valid, else made in a new clone of its parent and saved; then what grows
# from it. Logs to bin/steps-STEP.log.
fixture_job() {
  set +e
  local s=$1 log=$ROOT/bin/steps-$1.log rc=0
  VM=$(vm_of "$s")
  if exists && valid "$s"; then
    status "$s" "green already"
  else
    acquire
    status "$s" "running (log: ${log#"$ROOT/"})"
    {
      fresh "$s"; rc=$?
      if [ $rc = 0 ]; then run_step "$s"; rc=$?; fi
      if [ $rc = 0 ]; then save "$s"; rc=$?; fi
      multipass stop "$VM" >/dev/null 2>&1
    } > "$log" 2>&1
    release
    if [ $rc != 0 ]; then
      status "$s" failed
      report "$log"
      skip_below "$s"
      return 1
    fi
    status "$s" "green, $(minutes "$(cat "$ROOT/bin/times/$s")")"
  fi
  grow "$s"
}

# scenario_job STEP: a scenario, unless green already, in a new clone of its
# fixture; the clone is deleted when it passes (KEEP=1 keeps it, stopped)
# and kept, stopped, when it fails. Logs to bin/steps-STEP.log.
scenario_job() {
  set +e
  local s=$1 log=$ROOT/bin/steps-$1.log fp rc=0
  VM=$(vm_of "$s")
  fp=$(fingerprint "$s")
  if [ "$(green_of "$s")" = "$fp" ]; then
    status "$s" "green already"
    return 0
  fi
  rm -f "$ROOT/bin/green/$s"
  acquire
  status "$s" "running (log: ${log#"$ROOT/"})"
  {
    fresh "$s"; rc=$?
    if [ $rc = 0 ]; then run_step "$s"; rc=$?; fi
    multipass stop "$VM" >/dev/null 2>&1
  } > "$log" 2>&1
  release
  if [ $rc != 0 ]; then
    status "$s" "failed (its VM $VM is kept)"
    report "$log"
    return 1
  fi
  echo "$fp" > "$ROOT/bin/green/$s"
  [ "${KEEP:-}" = 1 ] || multipass delete --purge "$VM"
  status "$s" "green, $(minutes "$(cat "$ROOT/bin/times/$s")")"
}

# report LOG: the failures in a job's log.
report() { grep -E '^(FAIL|==> .*(failed|did not))' -A2 "$1" | head -40 || true; }

# forget STEP: STEP and what grows from it are no longer green.
forget() {
  local s
  for s in $(descendants "$1"); do
    if is_fixture "$s"; then
      if VM=$(vm_of "$s") exists && [ -n "$(VM=$(vm_of "$s") snapshot_of "$s")" ]; then
        multipass stop "$(vm_of "$s")" 2>/dev/null || true
        multipass delete --purge "$(vm_of "$s").$s"
      fi
    else
      rm -f "$ROOT/bin/green/$s"
    fi
  done
}

list() {
  local s have want state indent kind took
  printf '%-20s %-9s %-7s %-8s %s\n' STEP KIND STATE TOOK VM
  for s in "${STEPS[@]}"; do
    if is_fixture "$s"; then
      kind=fixture; have=$(VM=$(vm_of "$s") snapshot_of "$s")
    else
      kind=scenario; have=$(green_of "$s")
    fi
    want=$(fingerprint "$s")
    if [ -z "$have" ]; then state=none; elif [ "$have" = "$want" ]; then state=green; else state=stale; fi
    took=-; [ -f "$ROOT/bin/times/$s" ] && took=$(minutes "$(cat "$ROOT/bin/times/$s")")
    indent=$(printf '%*s' $(( ($(path "$s" | wc -w) - 1) * 2 )) '')
    printf '%-20s %-9s %-7s %-8s %s\n' "$indent${NUMBER[$s]} $s" "$kind" "$state" "$took" "$(vm_of "$s")"
  done
}

# ---------- main ----------

mkdir -p "$ROOT/bin/green" "$ROOT/bin/state" "$ROOT/bin/times"
targets=() from=
while [ $# -gt 0 ]; do
  case $1 in
    --list)   list; exit 0 ;;
    --clean)  for v in $(multipass list --format csv | awk -F, -v p="$VM" 'NR > 1 && ($1 == p || index($1, p "-") == 1) { print $1 }'); do
                multipass delete --purge "$v"; echo "deleted $v"
              done
              rm -rf "$ROOT/bin/green" "$ROOT/bin/state" "$ROOT/bin/times"; exit 0 ;;
    --from)   from=${2:?--from needs a step}; shift 2 ;;
    -*)       echo "unknown option: $1" >&2; exit 2 ;;
    *)        targets+=("$1"); shift ;;
  esac
done
if [ ${#targets[@]} = 0 ]; then
  for s in "${STEPS[@]}"; do is_fixture "$s" || targets+=("$s"); done
fi
for s in "${targets[@]}" $from; do
  [ -n "${NUMBER[$s]:-}" ] || { echo "unknown step: $s (steps: ${STEPS[*]})" >&2; exit 2; }
done
NEEDED=" "
for t in "${targets[@]}"; do
  for s in $(path "$t"); do needed "$s" || NEEDED+="$s "; done
done
[ -z "$from" ] || forget "$from"
rm -rf "$ROOT/bin/slots" && mkdir -p "$ROOT/bin/slots"
rm -f "$ROOT/bin/state/"*

# The trunk, as far as it is needed.
last=
for s in base install login; do needed "$s" && last=$s; done
set +e
trunk "$last"
rc=$?
set -e
[ $rc = 0 ] || { echo; list; say "Failed: ${NUMBER[$last]} $last, or a step before it"; exit 1; }
multipass stop "$VM" 2>/dev/null || true

# The rest, in parallel, built once for all of them.
make --no-print-directory build >/dev/null
export BUILT=1
set +e
grow login
set -e

echo
list
failed=()
for s in "${STEPS[@]}"; do
  case $(cat "$ROOT/bin/state/$s" 2>/dev/null) in failed*|skipped*) failed+=("${NUMBER[$s]} $s") ;; esac
done
if [ ${#failed[@]} -gt 0 ]; then
  say "Failed: ${failed[*]} (logs: bin/steps-<step>.log)"
  exit 1
fi
say "Green: ${targets[*]}"
# The browser goes to the fixture with the apps, todo's.
if [ "${OPEN:-1}" != 0 ] && needed deploy && VM=$(vm_of deploy) exists; then
  multipass start "$(vm_of deploy)" >/dev/null 2>&1 || true
  VM=$(vm_of deploy) test/vm/dns.sh
  # shellcheck disable=SC1090
  url=https://keycloak.$(. "$ANSWERS" && echo "$DOMAIN")
  if command -v xdg-open >/dev/null && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    xdg-open "$url" >/dev/null 2>&1 &
    echo "Opened $url"
  else
    echo "Open $url"
  fi
fi
