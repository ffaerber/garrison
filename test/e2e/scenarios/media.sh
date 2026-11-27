# media: a unit's media library, its file browser, and the agents
# making pictures and videos into it with ComfyUI (a fake one here, outside
# Garrison, as people run theirs) and cutting a short from them. Talks to the
# real LLM. Sourced by smoke.sh, whose helpers it uses.

section_media() {
echo "# media: a library, its file browser, and agents making media into it"
# The library's dataset; OpenEBS mounts it only into pods, so the checks
# read it through a mount of their own on the machine, as restores do.
lib_ds=garrison/ns/media/shares/media.shares.media
lib=$tmp/library
check "alice signs in to the chat"         chat_login alice

echo "# the unit media, with its file browser"
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"media","visibility":"limited","description":"Our pictures and videos"}' "$api/orgs")
check "alice creates the unit media"  bash -c "[ $code = 201 ] || [ $code = 422 ]"
media_config() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/media/config/raw/members.yaml?ref=main")" = 200 ]; }
check "media has its config"               until_true 120 media_config
# The file browser exactly as the unit's README gives it.
python3 -c '
import re,sys
print(re.search(r"## Media\n.*?```yaml\n(.*?)```", open(sys.argv[1]).read(), re.S).group(1), end="")' templates/unit-config/README.md > "$tmp/files.yaml"
check "alice adds the file browser of its README" set_stack media files "$tmp/files.yaml"
files_up() { garrison applied | grep -q "^app media/files@main files "; }
check "the deployer deploys it"            until_true 600 files_up
check "which makes the media library"      zfs list "$lib_ds"
mkdir -p "$lib" && mount -t zfs "$lib_ds" "$lib"

echo "# ComfyUI, outside Garrison"
# COMFYUI=host: a real one on the computer running the VM, at the VM's
# gateway, as the local Ollama is; it really draws, on its GPU. Otherwise a
# fake one in the VM, which hands back a fixed picture and clip.
comfy_pid=
if [ "${COMFYUI:-fake}" = host ]; then
  host_ip=$(ip route | awk '/^default/ { print $3; exit }')
  check "a real ComfyUI answers on the host, with a GPU" bash -c \
    'curl -sf --max-time 10 "http://$0:8188/system_stats" | grep -q "\"type\": \"cuda\""' "$host_ip"
else
  mkdir -p "$tmp/comfy" && cp test/e2e/media/picture.png test/e2e/media/clip.mp4 "$tmp/comfy/"
  python3 test/e2e/media/comfy.py 8188 "$tmp/comfy" & comfy_pid=$!
  host_ip=$(ip route get 1.1.1.1 | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}')
fi
trap '[ -z "$comfy_pid" ] || kill $comfy_pid 2>/dev/null; rm -rf "$tmp"' EXIT
sed -i '/^COMFYUI_URL=/d' $ETC/install.env && echo "COMFYUI_URL=http://$host_ip:8188" >> $ETC/install.env
kubectl --namespace garrison rollout restart deployment/agents >/dev/null && kubectl --namespace garrison rollout status deployment/agents --timeout 300s >/dev/null 2>&1
agents_say() { kubectl --namespace garrison logs "deployment/agents" --since=3m 2>&1 | grep -q "generate_media uses ComfyUI at http://$host_ip:8188"; }
check "the agent service uses it"          until_true 120 agents_say
check "the project media/demo goes to Gitea" installer/push-repo.sh --update media/demo test/e2e/media/demo

echo "# @designer makes pictures and a video in the project's styles"
media_channel() { channel_of media alice >/dev/null; }
check "media has its channel"              until_true 120 media_channel
answer=$(ask_agent media alice "For the project media/demo: make two pictures of the fox riding a bike in the style comic, and one short video of the fox waving in the style motion. Do not ask back." designer)
made() { [ "$(ls "$lib"/demo/*.png 2>/dev/null | wc -l)" -ge 2 ] && ls "$lib"/demo/*.mp4 >/dev/null 2>&1; }
if ! made; then
  printf 'retry @designer did not make them; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent media alice "Please call generate_media now: project media/demo, style comic, prompt 'the fox riding a bike', count 2. Then generate_media with project media/demo, style motion, prompt 'the fox waving', count 1." designer)
fi
check "two pictures and a video, in the library" made
first=$(ls "$lib"/demo/comic-*.png 2>/dev/null | head -n1)
check "each with a .json of how it was made" bash -c 'grep -q "\"style\": \"comic\"" "$0.json" && grep -q "\"project\": \"media/demo\"" "$0.json"' "$first"
if [ -n "$comfy_pid" ]; then
  check "ComfyUI got the style's prompt, size and length in their nodes" python3 -c '
import json,sys
runs=[json.loads(l) for l in open(sys.argv[1])]
pic=any(r.get("6",{}).get("inputs",{}).get("text","").endswith(", comic style, bold ink lines, flat bright colours") and r["5"]["inputs"]["width"] == 768 and r["7"]["inputs"]["text"] == "photo, blurry, scary" for r in runs)
vid=any(r.get("12",{}).get("inputs",{}).get("length") == 49 for r in runs)
sys.exit(0 if pic and vid else 1)' "$tmp/comfy/prompts.jsonl"
else
  # What the real one drew: the style's size, and its number of frames.
  probe() { image_pod "probe-$RANDOM" -v "$lib/demo:/d" -- ffprobe -v error -select_streams v:0 "$@"; }
  check "a picture, drawn in the style's 768x1024" test "$(probe -show_entries stream=width,height -of csv=p=0 "/d/${first##*/}")" = 768,1024
  video=$(ls "$lib"/demo/motion-*.mp4 2>/dev/null | head -n1)
  check "a video of the style's 49 frames" test "$(probe -count_frames -show_entries stream=nb_read_frames -of csv=p=0 "/d/${video##*/}")" = 49
fi
check "alice signs in to her dashboard"    until_true 60 home_login alice
check "the feed has what it made, for media's people" in_feed alice ns=media "e['kind'] == 'media' and e['agent'] == 'media/designer'"

echo "# @designer cuts a short from them"
# Cutting takes a while: wait as long as a turn of media may last (30
# minutes), so a retry does not queue behind the first request.
answer=$(ASK_WAIT=1860 ask_agent media alice "Make a short vertical video of at most 15 seconds from the pictures and the video of the project demo. Do not ask back." designer)
short() { ls "$lib"/demo/shorts/short-*.mp4 >/dev/null 2>&1; }
if ! short; then
  printf 'retry @designer made no short; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ASK_WAIT=1860 ask_agent media alice "Please make it now with ffmpeg: the files in /data/media/demo/, 1080x1920, at most 15 seconds, written to /data/media/demo/shorts/short-<YYYYMMDD-HHMMSS>.mp4." designer)
fi
check "a short in the library"             short
# The short with its .json beside it, the finished one; else the newest
# non-empty one, as an agent running ffmpeg itself writes no .json.
size=$(image_pod short-size -v "$lib/demo/shorts:/s" -- sh -c '
  f=$(ls /s/short-*.mp4.json 2>/dev/null | head -n1); f=${f%.json}
  [ -n "$f" ] || f=$(find /s -name "short-*.mp4" -size +0 | sort | tail -n1)
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$f"' 2>/dev/null)
check "no empty or broken file left in the library" bash -c '! find "$0" -name "short-*.mp4" -size 0 | grep -q .' "$lib/demo/shorts"
vertical() { [ -n "$size" ] && python3 -c 'import sys; w,h=map(int,sys.argv[1].split(",")[:2]); sys.exit(0 if w*16 == h*9 else 1)' "$size"; }
check "vertical, 9:16"                     vertical

echo "# the file browser"
files_login() {
  sso_login "$tmp/files-$1.jar" "https://files.$DOMAIN/" "$TEST_PASSWORD" "$tmp/files-$1.html" "$1" >/dev/null 2>&1
  web -b "$tmp/files-$1.jar" -X POST "https://files.$DOMAIN/api/login" > "$tmp/files-$1.token" && [ -s "$tmp/files-$1.token" ]
}
files_api() { local who=$1; shift; web -b "$tmp/files-$who.jar" -H "X-Auth: $(cat "$tmp/files-$who.token")" "$@"; }
check "alice opens it"                     until_true 120 files_login alice
check "bob opens it, though not in media"  until_true 120 files_login bob
check "bob sees the pictures"              bash -c 'grep -q "comic-" <<<"$0"' "$(files_api bob "https://files.$DOMAIN/api/resources/demo/")"
check "but may not upload"                 test "$(files_api bob -o /dev/null -w '%{http_code}' -X POST --data-binary x "https://files.$DOMAIN/api/resources/demo/bob.txt?override=false")" = 403
check "alice uploads"                      test "$(files_api alice -o /dev/null -w '%{http_code}' -X POST --data-binary hello "https://files.$DOMAIN/api/resources/demo/notes.txt?override=false")" = 200
check "and her agents see it"              test "$(cat "$lib/demo/notes.txt")" = hello
check "nobody makes a public link"         test "$(files_api alice -o /dev/null -w '%{http_code}' -X POST "https://files.$DOMAIN/api/share/demo/notes.txt")" = 403
# Another app could call it past Traefik, naming anyone: without the
# environment's ingress key the gate takes nothing.
# wget fails on the 403 it is meant to get: its output says which it was.
past() {
  local out
  out=$(in_app files web wget -S -O /dev/null --header 'X-Garrison-User: alice' --header 'X-Auth-Request-Preferred-Username: alice' http://127.0.0.1:8080/api/resources/ 2>&1)
  grep -q ' 403 ' <<<"$out"
}
check "past the sign-in, it takes nothing" past
kill $comfy_pid 2>/dev/null || true

echo "# a chain of command: software updates"
# Two apps of media pinned to old releases: whoami by semantic version, and
# Chrome's headless shell, whose majors come every four weeks, so that a
# version-number rule ("only report a new major") would never update it.
# alice asks the commander about updates; it hands the chain
# software-updates to its lead, the sentry, which asks the scout (the real
# web) and then the coder, in that order.
cat > "$tmp/greeter.yaml" <<'YAML'
stack: greeter
unit: media
entrypoint: web
services:
  web: { image: traefik/whoami:v1.10.0, port: 80 }
environments:
  main: {}
YAML
cat > "$tmp/shell.yaml" <<'YAML'
stack: shell
unit: media
entrypoint: chrome
services:
  chrome: { image: chromedp/headless-shell:150.0.7871.125, port: 9222, resources: { memory: 512Mi } }
environments:
  main: { private: true }
YAML
check "media runs traefik/whoami:v1.10.0"  set_stack media greeter "$tmp/greeter.yaml"
check "and chromedp/headless-shell:150.0.7871.125" set_stack media shell "$tmp/shell.yaml"
asked_from=$(feed alice "/api/events?limit=1" | python3 -c 'import json,sys; e=json.load(sys.stdin); print(e[0]["id"] if e else 0)')
answer=$(ask_agent media alice "Are there software updates for the apps of the unit media? If so, have them proposed." commander)
# update_pr FILE IMAGE OLD: an open pull request in media/config, by alice,
# whose only change to stacks/FILE.yaml is IMAGE's tag, from OLD to a newer
# one; other files of the same pull request may change too.
update_pr() {
  local n
  for n in $(gitea_api "" "$api/repos/media/config/pulls?state=open&limit=50" | python3 -c '
import json,sys
for p in json.load(sys.stdin):
    if p["user"]["login"] == "alice": print(p["number"])'); do
    gitea_api "" "$api/repos/media/config/pulls/$n.diff" | python3 -c '
import re,sys
file, image, old = sys.argv[1:4]
d = sys.stdin.read()
part = re.search(r"^diff --git a/stacks/%s\.yaml .*?(?=^diff --git |\Z)" % re.escape(file), d, re.S | re.M)
if not part: sys.exit(1)
lines = part.group(0).splitlines()
minus = [l for l in lines if l.startswith("-") and not l.startswith("---")]
plus = [l for l in lines if l.startswith("+") and not l.startswith("+++")]
num = lambda t: tuple(int(x) for x in re.findall(r"\d+", t))
new = re.findall(re.escape(image) + r":v?([0-9][0-9.]*)", "\n".join(plus))
ok = len(minus) == 1 and len(plus) == 1 and image + ":" + old in minus[0] and new and num(new[0]) > num(old)
sys.exit(0 if ok else 1)' "$1" "$2" "$3" && { echo "$n"; return 0; }
  done
  return 1
}
both_prs() { update_pr greeter traefik/whoami v1.10.0 && update_pr shell chromedp/headless-shell 150.0.7871.125; }
if ! until_true 900 both_prs >/dev/null; then
  printf 'retry not both updates were proposed; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent media alice "Please hand this to the sentry as the chain software-updates says: check stacks/greeter.yaml (traefik/whoami:v1.10.0) and stacks/shell.yaml (chromedp/headless-shell:150.0.7871.125) of media/config for newer releases, and have the coder propose them." commander)
fi
check "an update is proposed: only the tag of whoami, newer" until_true 900 update_pr greeter traefik/whoami v1.10.0
check "and of Chrome, a newer major: not only reported" until_true 300 update_pr shell chromedp/headless-shell 150.0.7871.125
# chain_order: the feed has commander → sentry, then sentry → scout before
# sentry → coder.
chain_order() {
  feed alice "/api/events?limit=500&ns=media" | python3 -c '
import json,sys
since=int(sys.argv[1] or 0)
es=[e for e in json.load(sys.stdin) if e.get("kind") == "ask" and e["id"] > since]
es.sort(key=lambda e: e["id"])
order=[(e.get("started_by",""), e.get("agent","")) for e in es]
def at(pair): return next((i for i,p in enumerate(order) if p == pair), -1)
lead=at(("agent media/commander","media/sentry")); scout=at(("agent media/sentry","media/scout")); coder=at(("agent media/sentry","media/coder"))
sys.exit(0 if 0 <= lead < scout < coder else 1)' "$asked_from"
}
check "in the chain's order: commander → sentry, then scout, then coder" until_true 60 chain_order
}
