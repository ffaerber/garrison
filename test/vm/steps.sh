#!/usr/bin/env bash
# The end-to-end test as a tree of steps on Multipass VMs. A fixture is a
# state that scenarios start from (Garrison installed, a unit with its
# app deployed): green, it is saved as a VM snapshot. A scenario is a test
# that starts from its fixture: it runs in a clone of the fixture's VM, which
# is thrown away when it passes and kept, stopped, when it fails. So a change
# reruns only the fixtures and scenarios it affects, a failure in one does not
# stop the others, and up to JOBS VMs run at once.
#
#   test/vm/steps.sh               every scenario that is not green yet
#   test/vm/steps.sh routines media  only these, and the fixtures they start from
#   test/vm/steps.sh --from login  rerun from `login`: it and all that grows from it
#   test/vm/steps.sh --list        the tree, and what is green, valid or stale
#   test/vm/steps.sh --clean       delete the VMs, their snapshots and the green marks
#   JOBS=4 test/vm/steps.sh        at most 4 VMs at once (default 2, fewer
#                                  when memory is short, see FREE_GB)
#   FREE_GB=12 test/vm/steps.sh    a VM boots only while the host has this
#                                  much memory available (default 10)
#   LLM=cloud test/vm/steps.sh     use Ollama's cloud instead of the local Ollama
#   COMFYUI=host test/vm/steps.sh media
#                                  media with a real ComfyUI on this computer,
#                                  reached at the VM's gateway, port 8188,
#                                  instead of the fake one: it really draws
#
# The scenarios that talk to a real LLM (agent, media, journey, newsroom, ui, and team
# and what grows from it) use by
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

VM=${VM:-garrison-k3s}
BASE_VM=$VM   # VM changes to the VM of each job; names come from this
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ANSWERS=${ANSWERS:-test/answers/selfsigned.env}
JOBS=${JOBS:-2}   # more at once has crashed Multipass's daemon more often
FREE_GB=${FREE_GB:-10}   # memory a VM waits for before it boots: its 8G and a margin
cd "$ROOT"

# The tree. Fixtures [F] are saved as snapshots; the others are scenarios.
#
#   1 base [F] - 2 install [F] - 3 login [F] -+- 4.1 platform
#                                             +- 4.2 crud
#                                             +- 4.3 ui
#                                             +- 4.4 agent [F] --- 5.1 media
#                                             +- 4.5 deploy [F] -+- 5.2 journey
#                                                                +- 5.3 people
#                                                                +- 5.5 nodes
#                                                                +- 5.6 newsroom
#                                                                +- 5.4 team [F] -+- 6.1 roles
#                                                                                 +- 6.2 change
#                                                                                 +- 6.3 marketing
#                                                                                 +- 6.4 routines
#                                                                                 +- 6.5 commander
#                                                                                 +- 6.6 operator
#                                                                                 +- 6.7 leaving
#                                                                                 +- 6.8 guided
#
# deploy: the unit todo with its app deployed, alice its only person.
# team: carol an admin, bob a member, and the agents the scenarios share.
# Each scenario is test/e2e/scenarios/<name>.sh (see docs/scenarios.md).
# The trunk (base, install, login) lives in $VM; every other fixture and
# scenario in a VM of its own, $VM-<step>, cloned from its parent's.
STEPS=(base install login platform crud ui agent media deploy journey people nodes newsroom team roles change marketing routines commander operator leaving guided)
declare -A PARENT=([base]="" [install]=base [login]=install [platform]=login [crud]=login [ui]=login
  [agent]=login [media]=agent [deploy]=login [journey]=deploy [people]=deploy [nodes]=deploy [newsroom]=deploy [team]=deploy
  [roles]=team [change]=team [marketing]=team [routines]=team [commander]=team [operator]=team [leaving]=team [guided]=team)
declare -A NUMBER=([base]=1 [install]=2 [login]=3 [platform]=4.1 [crud]=4.2 [ui]=4.3
  [agent]=4.4 [media]=5.1 [deploy]=4.5 [journey]=5.2 [people]=5.3 [nodes]=5.5 [newsroom]=5.6 [team]=5.4
  [roles]=6.1 [change]=6.2 [marketing]=6.3 [routines]=6.4 [commander]=6.5 [operator]=6.6 [leaving]=6.7 [guided]=6.8)
FIXTURES=" base install login agent deploy team "
TRUNK=" base install login "

# Which LLM the agents use: local (Ollama on this computer, LOCAL_MODEL) or cloud.
LLM=${LLM:-local}
COMFYUI=${COMFYUI:-fake}
LOCAL_MODEL=${LOCAL_MODEL:-qwen3.8-64k:latest}
case $LLM in local|cloud) ;; *) echo "LLM must be local or cloud" >&2; exit 2 ;; esac

# Files each step depends on, besides this script's own definition of the
# step: its scenario file, and what every scenario shares.
inputs() {
  local shared="test/e2e/smoke.sh test/e2e/lib.sh test/e2e/ready.sh test/e2e/scenarios/$1.sh"
  case $1 in
    base)     ;;
    install)  echo install.sh installer garrison control-plane schemas templates files sandbox go.mod go.sum host "$ANSWERS" test/e2e/add-test-users.sh ;;
    login)    echo "$shared" test/e2e/scenarios/system.sh ;;
    platform) echo "$shared" test/e2e/stacks ;;
    deploy)   echo "$shared" templates/app test/e2e/apps/quotes installer/push-repo.sh ;;
    ui|people|guided)
              echo "$shared" test/ui/Dockerfile test/ui/run.sh test/ui/lib.py "test/ui/$1.py" ;;
    media)    echo "$shared" test/e2e/media templates/unit-config/README.md ;;
    nodes)    echo "$shared" installer/join.sh installer/node.sh host/hosts.sh ;;
    newsroom) echo "$shared" templates/app/news ;;
    team|roles|change|marketing|routines|commander|operator|leaving)
              echo "$shared" test/e2e/apps ;;
    *)        echo "$shared" ;;
  esac
}

say()   { printf '\n==> [%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
in_vm() { multipass exec "$VM" -- "$@"; }
# multipass: Multipass 1.17 segfaulted twice when VMs were changed at once
# (a fixture's snapshot while other VMs were restored and cloned), and its
# restarts then failed on the dnsmasq it left behind. So commands that
# change a VM run one at a time, under one lock. A command that finds the
# daemon gone waits for it, bringing it back after half a minute with
# test/vm/revive-multipass.sh when sudo needs no password (see AGENTS.md,
# "Multipass"), up to 10 minutes, then tries once more.
multipass() {
  local rc try
  for try in 1 2; do
    case $1 in
      launch|start|stop|restart|snapshot|restore|clone|delete|purge)
        ( flock 8; command multipass "$@" ) 8>"$ROOT/bin/multipass.lock" ;;
      exec) watched "$@" ;;
      *) command multipass "$@" ;;
    esac
    rc=$?
    [ $rc = 0 ] && return 0
    # A command cut off by the daemon's crash is not run again: it may have
    # changed the VM halfway. Its step fails, and its VM is kept.
    [ $rc = 125 ] && return $rc
    command multipass version 2>/dev/null | grep -q '^multipassd' && return $rc
    echo "==> Multipass's daemon is not answering; waiting for it (see AGENTS.md, Multipass)" >&2
    local i
    for i in $(seq 1 120); do
      command multipass version 2>/dev/null | grep -q '^multipassd' && break
      if [ "$i" = 6 ] && sudo -n true 2>/dev/null; then
        echo "==> Bringing Multipass's daemon back" >&2
        ( flock 8; "$ROOT/test/vm/revive-multipass.sh" ) 8>"$ROOT/bin/multipass.lock" >&2 || true
      fi
      sleep 5
    done
  done
  return $rc
}
# watched exec VM -- COMMAND...: multipass exec, given up when Multipass's
# daemon restarts meanwhile. The session of a crashed daemon never ends (a
# step hung for six hours): it is killed, and watched returns 125.
watched() {
  local daemon pid now
  daemon=$(pidof multipassd || true)
  # Without the run's lock (fd 8): an exec that outlives the run would
  # hold it, and every later run would find the run still going.
  command multipass "$@" <&0 8>&- &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    sleep 15 &
    wait $! 2>/dev/null
    now=$(pidof multipassd || true)
    if [ -n "$daemon" ] && [ "$now" != "$daemon" ] && kill -0 "$pid" 2>/dev/null; then
      echo "==> Multipass's daemon restarted during: multipass $*; giving it up (see AGENTS.md, Multipass)" >&2
      pkill -P "$pid" 2>/dev/null
      kill "$pid" 2>/dev/null
      sleep 5
      kill -9 "$pid" 2>/dev/null   # a client of the crashed daemon may not end otherwise
      wait "$pid" 2>/dev/null
      return 125
    fi
  done
  wait "$pid"
}
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

# A unit with an app of its own, built by its pipeline on the user
# runner and deployed by the deployer: todo/app, from the template todo, left running.
step_deploy() {
  sync_repo
  smoke deploy
}

# The fixture team and the scenarios that grow from it (docs/scenarios.md):
# people ask the agents of todo for changes, rights, screenshots, routines.
# Talk to the real LLM.
scenario() {
  sync_repo
  put_llm_key
  smoke "$1"
}
step_team()      { scenario team; }
step_roles()     { scenario roles; }
step_change()    { scenario change; }
step_marketing() { scenario marketing; }
step_routines()  { scenario routines; }
step_commander() { scenario commander; }
step_operator()  { scenario operator; }
step_leaving()   { scenario leaving; }

# Scenario 10 of docs/scenarios.md: alice has the agents make a unit, a
# repo from a template and its stack, deploys it, changes it on dev through
# an agent and merges dev into main. Talks to the real LLM.
step_journey() {
  sync_repo
  put_llm_key
  smoke journey
}

step_newsroom() {
  sync_repo
  put_llm_key
  smoke newsroom
}

# The browser scenarios (test/ui/NAME.py): people click and type in Gitea,
# Open WebUI and the dashboard. ui: from the unit todo to its todo app
# deployed, a screenshot, a scheduled job and a routine's question; people:
# joining and leaving todo, as Open WebUI shows it; guided: vague requests,
# and the agents asking what is meant. Talk to the real LLM. The screenshots
# of every step land in bin/ui-shots/NAME/.
browser_scenario() {
  sync_repo
  put_llm_key
  local rc=0 shots=$ROOT/bin/ui-shots/$1
  smoke "$1" || rc=$?
  in_vm sudo tar -C /var/tmp -cf /home/ubuntu/ui-shots.tar garrison-ui 2>/dev/null &&
    in_vm sudo chmod 644 /home/ubuntu/ui-shots.tar &&
    multipass transfer "$VM:ui-shots.tar" "$ROOT/bin/ui-shots-$1.tar" &&
    rm -rf "$shots" && mkdir -p "$shots" &&
    tar -C "$shots" --strip-components=1 -xf "$ROOT/bin/ui-shots-$1.tar" || true
  rm -f "$ROOT/bin/ui-shots-$1.tar"
  return $rc
}
# A second machine, $VM-node2, a clone of base, joins deploy's with
# `install.sh --join` and a larger pool, so new data goes there; scenario
# nodes then checks what runs where, and an app's data there. The machine is
# deleted when the scenario passes and kept, stopped, when it fails.
step_nodes() {
  sync_repo
  local main=$VM node=$VM-node2 join rc=0
  VM=$node exists && multipass delete --purge "$node"
  say "Cloning ${NUMBER[base]} base into $node"
  clone_from base "$node"
  # Apps only: less than the main machine needs.
  multipass set "local.$node.memory=3G"
  multipass start "$node" >/dev/null
  trap 'multipass stop "$node" >/dev/null 2>&1 || true' EXIT
  VM=$node sync_repo
  join=$(in_vm sudo bash -c 'cd garrison && ./install.sh --join-command')
  VM=$node in_vm sudo bash -c "cd garrison && ${join#sudo } --pool-size 40G" || rc=$?
  [ $rc != 0 ] || VM=$main smoke nodes || rc=$?
  trap - EXIT
  if [ $rc = 0 ] && [ "${KEEP:-}" != 1 ]; then
    multipass delete --purge "$node"
  else
    multipass stop "$node" >/dev/null 2>&1 || true
  fi
  return $rc
}

step_ui()     { browser_scenario ui; }
step_people() { browser_scenario people; }
step_guided() { browser_scenario guided; }

# A unit's media library and its file browser; @media makes pictures
# and a video with a fake ComfyUI outside Garrison, then cuts a short.
# Talks to the real LLM.
step_media() {
  sync_repo
  put_llm_key
  in_vm sudo bash -c "cd garrison && COMFYUI=$COMFYUI WAIT_READY=600 test/e2e/smoke.sh media"
}

# Every kind of thing a unit holds, made, read, changed and removed:
# the unit itself, its people, repos, agents with grants, apps with
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
      [[ $(declare -f "step_$s") == *scenario* ]] && declare -f scenario browser_scenario
      [ "$s" = install ] && echo "LLM=$LLM LOCAL_MODEL=$LOCAL_MODEL"
      [ "$s" = media ] && [ "$COMFYUI" != fake ] && echo "COMFYUI=$COMFYUI"
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
  # Multipass's daemon has crashed while stopping the VM (1.17, see AGENTS.md).
  # Restarted, it boots the VM again and leaves it "starting", half off, and
  # refuses to stop or snapshot it. So a failed try boots the VM fully, and
  # the next stops it again; without a snapshot after three, the fixture is
  # not green and the step fails.
  local try i done=
  for try in 1 2 3; do
    if [ "$try" -gt 1 ]; then
      say "No snapshot yet; booting $VM to stop it again (try $try of 3)"
      multipass start "$VM" || true   # the wrapper above waits for the daemon
    fi
    in_vm sudo sync || true
    if ! multipass stop "$VM"; then
      say "The VM did not stop; forcing it off"
      multipass stop --force "$VM"
    fi
    for i in $(seq 12); do
      stopped "$VM" && break
      sleep 5
      multipass stop --force "$VM" >/dev/null 2>&1
    done
    stopped "$VM" || continue
    [ -n "$(snapshot_of "$step")" ] && multipass delete --purge "$VM.$step"
    if multipass snapshot "$VM" --name "$step" --comment "$fp" >/dev/null; then
      done=1
      break
    fi
  done
  multipass start "$VM"
  if [ -z "$done" ]; then
    say "Multipass took no snapshot ${NUMBER[$step]} $step"
    return 1
  fi
  say "Snapshot ${NUMBER[$step]} $step took $((SECONDS - t0))s (stop, snapshot, boot)"
}


# readdress: a clone has a new IP address (and hostname). k3s takes the new
# address when it starts, keeping its node name (install.sh); this waits
# until the API answers and the node is Ready again.
readdress() {
  in_vm sudo bash -c '
    for i in $(seq 90); do
      kubectl get --raw /readyz >/dev/null 2>&1 && kubectl wait --for=condition=Ready node --all --timeout=5s >/dev/null 2>&1 && exit 0
      sleep 2
    done
    echo "k3s is not ready on the clone" >&2; kubectl get nodes >&2; exit 1'
}

# settle: after the clone boots, every workload of the platform rolls out
# again; this waits until each is available.
settle() {
  in_vm sudo bash -c '
    # Every pod running and ready since this boot (test/e2e/ready.sh).
    if ! garrison/test/e2e/ready.sh 600; then
      kubectl --namespace garrison get pods >&2
      exit 1
    fi
    # BuildKit runs outside the cluster, in Docker, on the builds network;
    # after a clone boots its name can be gone from that network, and
    # pipelines fail with "lookup garrison-buildkit ... server misbehaving".
    # It is restarted until a client on the builds network reaches it by name.
    systemctl is-enabled --quiet garrison-buildkit.service 2>/dev/null || exit 0
    for i in $(seq 1 6); do
      systemctl restart garrison-buildkit.service
      for j in $(seq 1 10); do
        sleep 3
        docker run --rm --network garrison-builds --entrypoint buildctl moby/buildkit:v0.34.1 \
          --addr tcp://garrison-buildkit:1234 debug workers >/dev/null 2>&1 && exit 0
      done
      echo "BuildKit is not reachable on the builds network; restarting it" >&2
    done
    echo "BuildKit still unreachable on the builds network" >&2
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

# A pool of JOBS slots: a job holds one while its VM runs. Fixtures go
# first, as scenarios wait for them: a scenario takes a slot only while no
# fixture waits for one (acquire fixture).
acquire() {
  local i
  [ "${1:-}" = fixture ] && touch "$ROOT/bin/slots/want-$BASHPID"
  while :; do
    if [ "${1:-}" = fixture ] || ! compgen -G "$ROOT/bin/slots/want-*" >/dev/null; then
      for i in $(seq "$JOBS"); do
        if mkdir "$ROOT/bin/slots/$i" 2>/dev/null; then
          SLOT=$i
          rm -f "$ROOT/bin/slots/want-$BASHPID"
          slot_log "+$SLOT"
          return 0
        fi
      done
    fi
    sleep 3
  done
}
release() { slot_log "-$SLOT"; rmdir "$ROOT/bin/slots/$SLOT" 2>/dev/null || true; }
# slot_log CHANGE: a slot taken (+N) or given back (-N), by which job and
# process, to bin/slots.log: what to read when more VMs run than JOBS.
slot_log() { echo "$(date +%T) ${VM#"$BASE_VM"} $BASHPID $1 [$(cd "$ROOT/bin/slots" && echo [0-9]*)]" >> "$ROOT/bin/slots.log"; }

# clone_from STEP DEST: DEST becomes a copy of the fixture STEP's VM at its
# snapshot. One clone at a time per source, which is stopped for it.
# stopped NAME: whether Multipass lists the VM as stopped.
stopped() { [ "$(multipass info "$1" --format csv 2>/dev/null | awk -F, 'NR==2 {print $2}')" = Stopped ]; }
clone_from() {
  local src; src=$(vm_of "$1")
  (
    flock 9
    # A stop can fail without the VM stopping (Multipass's five minutes, or
    # its daemon brought back meanwhile); restore and clone then refuse.
    # It is about to be restored to its snapshot, so forcing it off is safe.
    if ! multipass stop "$src" >/dev/null 2>&1 || ! stopped "$src"; then
      multipass stop --force "$src" >/dev/null 2>&1 || true
    fi
    multipass restore --destructive "$src.$1" >/dev/null
    multipass clone "$src" --name "$2" >/dev/null
  ) 9>"$ROOT/bin/lock-$src"
}

# free_gb: the memory the host has available, in whole GB.
free_gb() { awk '/^MemAvailable:/ { print int($2 / 1048576) }' /proc/meminfo; }

# running_vms: how many VMs of ours run (or boot) now.
running_vms() {
  multipass list --format csv 2>/dev/null | awk -F, -v p="$BASE_VM" \
    'NR > 1 && ($1 == p || index($1, p "-") == 1) && $2 !~ /^(Stopped|Deleted|Suspended)$/' | wc -l
}

# start_vm NAME: boots the VM once fewer than JOBS of ours run and the host
# has FREE_GB available, one boot at a time. The count comes first: a VM
# just booted has not yet taken its 8G, so free memory alone let a seventh
# boot while six ran (and the slots, which should hold them to JOBS, did
# not).
start_vm() {
  (
    flock 9
    local said=
    while [ "$(running_vms)" -ge "$JOBS" ] || [ "$(free_gb)" -lt "$FREE_GB" ]; do
      [ -n "$said" ] || { say "Waiting to boot $1: $(running_vms) VM(s) of ours run (at most $JOBS), ${FREE_GB}G of free memory needed (now $(free_gb)G)"; said=1; }
      sleep 10
    done
    multipass start "$1" >/dev/null
  ) 9>"$ROOT/bin/lock-memory"
}

# stop_leftovers: a run that ended without cleaning up (the computer
# crashed, or the run was killed) leaves its VMs running and holding memory.
# No run drives them now (the lock in main), so each is stopped: a fixture's
# is restored from its snapshot before it is used, a scenario's replaced
# when it runs again, and a failed one stays to look at. One at a time, as
# Multipass's daemon has crashed when several changed at once.
stop_leftovers() {
  local v
  for v in $(multipass list --format csv 2>/dev/null | awk -F, -v p="$BASE_VM" \
      'NR > 1 && ($1 == p || index($1, p "-") == 1) && $2 !~ /^(Stopped|Deleted|Suspended)$/ { print $1 }'); do
    say "Stopping $v, left running by an earlier run"
    multipass stop "$v" >/dev/null 2>&1 || multipass stop --force "$v" >/dev/null 2>&1 || true
  done
}

# fit_jobs: at most as many jobs as the available memory holds VMs of 8G
# (FREE_GB each, the first one's margin kept for the host).
fit_jobs() {
  local fit=$(( $(free_gb) / FREE_GB ))
  [ "$fit" -ge 1 ] || fit=1
  if [ "$fit" -lt "$JOBS" ]; then
    say "$(free_gb)G of memory available: $fit VM(s) at once instead of $JOBS"
    JOBS=$fit
  fi
}

# fresh STEP: $VM becomes a new clone of STEP's parent, booted, with its
# swarm formed on the clone's address and every service running again.
fresh() {
  exists && multipass delete --purge "$VM"
  say "Cloning ${NUMBER[${PARENT[$1]}]} ${PARENT[$1]} into $VM"
  clone_from "${PARENT[$1]}" "$VM" || return 1
  start_vm "$VM" || return 1
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
    start_vm "$VM"
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
    rc=$?
    if [ $rc != 0 ]; then
      status "$s" failed
      return $rc
    fi
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

# lost_to_multipass LOG DAEMON: whether a job failed because Multipass's
# daemon crashed under it (1.17, see AGENTS.md): its process is no longer
# DAEMON, and the log shows a command it cut off or refused. Such a job runs
# once more; a test that failed by itself, with no such line, does not.
lost_to_multipass() {
  [ "$(pidof multipassd || true)" != "$2" ] &&
    grep -qE "Multipass's daemon restarted during|cannot connect to the multipass socket" "$1"
}

# fixture_job STEP: a fixture below the trunk, kept from its snapshot when
# valid, else made in a new clone of its parent and saved; then what grows
# from it. Logs to bin/steps-STEP.log.
fixture_job() {
  set +e
  local s=$1 log=$ROOT/bin/steps-$1.log rc=0 try daemon
  VM=$(vm_of "$s")
  if exists && valid "$s"; then
    status "$s" "green already"
  else
    acquire fixture
    status "$s" "running (log: ${log#"$ROOT/"})"
    : > "$log"
    for try in 1 2; do
      daemon=$(pidof multipassd || true)
      {
        fresh "$s"; rc=$?
        if [ $rc = 0 ]; then run_step "$s"; rc=$?; fi
        if [ $rc = 0 ]; then save "$s"; rc=$?; fi
        multipass stop "$VM" >/dev/null 2>&1
      } >> "$log" 2>&1
      [ $rc = 0 ] && break
      [ $try = 1 ] && lost_to_multipass "$log" "$daemon" || break
      status "$s" "running again: Multipass's daemon crashed under it (log: ${log#"$ROOT/"})"
      echo $'\n==> Running it again: Multipass\'s daemon crashed under it\n' >> "$log"
    done
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
  local s=$1 log=$ROOT/bin/steps-$1.log fp rc=0 try daemon
  VM=$(vm_of "$s")
  fp=$(fingerprint "$s")
  if [ "$(green_of "$s")" = "$fp" ]; then
    status "$s" "green already"
    return 0
  fi
  rm -f "$ROOT/bin/green/$s"
  acquire
  status "$s" "running (log: ${log#"$ROOT/"})"
  : > "$log"
  for try in 1 2; do
    daemon=$(pidof multipassd || true)
    {
      fresh "$s"; rc=$?
      if [ $rc = 0 ]; then run_step "$s"; rc=$?; fi
      [ $rc = 0 ] || service_logs "$s"
      multipass stop "$VM" >/dev/null 2>&1
    } >> "$log" 2>&1
    [ $rc = 0 ] && break
    [ $try = 1 ] && lost_to_multipass "$log" "$daemon" || break
    status "$s" "running again: Multipass's daemon crashed under it (log: ${log#"$ROOT/"})"
    echo $'\n==> Running it again: Multipass\'s daemon crashed under it\n' >> "$log"
  done
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

# service_logs STEP: the logs of every workload of the platform in the job's
# VM, to bin/logs/STEP/<workload>.log, kept from before the VM stops: a pod
# starts afresh when the VM boots again, without its old logs.
service_logs() {
  local dir=$ROOT/bin/logs/$1 w
  rm -rf "$dir" && mkdir -p "$dir"
  for w in $(in_vm sudo kubectl --namespace garrison get deployments,daemonsets --output name 2>/dev/null); do
    in_vm sudo timeout 60 kubectl --namespace garrison logs "$w" --all-containers --timestamps --tail 5000 > "$dir/${w#*/}.log" 2>&1
  done
  echo "==> the services' logs are in ${dir#"$ROOT/"}/"
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
  printf '%-26s %-9s %-7s %-8s %s\n' STEP KIND STATE TOOK VM
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
    printf '%-26s %-9s %-7s %-8s %s\n' "$indent${NUMBER[$s]} $s" "$kind" "$state" "$took" "$(vm_of "$s")"
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
# One run at a time: it may then stop whatever VM of ours is still running.
# Opened without emptying it: a run refused here keeps the running one's ID.
exec 8>>"$ROOT/bin/lock-run"
flock -n 8 || { echo "another test/vm/steps.sh is running (bin/lock-run)" >&2; exit 1; }
echo $$ > "$ROOT/bin/lock-run"   # its process ID, for test/vm/watch.py
# The run's output also goes to bin/steps.out, for test/vm/watch.py.
exec > >(tee "$ROOT/bin/steps.out" 8>&-) 2>&1
stop_leftovers
fit_jobs
[ -z "$from" ] || forget "$from"
rm -rf "$ROOT/bin/slots" "$ROOT/bin/slots.log" && mkdir -p "$ROOT/bin/slots"
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
    xdg-open "$url" >/dev/null 2>&1 8>&- &   # without the run lock
    echo "Opened $url"
  else
    echo "Open $url"
  fi
fi
