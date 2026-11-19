# media: a namespace's media library, its file browser, and the agents
# making pictures and videos into it with ComfyUI (a fake one here, outside
# Garrison, as people run theirs) and cutting a short from them. Talks to the
# real LLM. Sourced by smoke.sh, whose helpers it uses.

section_media() {
echo "# media: a library, its file browser, and agents making media into it"
lib=/garrison/ns/media/shares/media
check "alice signs in to the chat"         chat_login alice

echo "# the namespace media, with its file browser"
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"media","visibility":"limited","description":"Our pictures and videos"}' "$api/orgs")
check "alice creates the namespace media"  bash -c "[ $code = 201 ] || [ $code = 422 ]"
media_config() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/media/config/raw/members.yaml?ref=main")" = 200 ]; }
check "media has its config"               until_true 120 media_config
# The file browser exactly as the namespace's README gives it.
python3 -c '
import re,sys
print(re.search(r"## Media\n.*?```yaml\n(.*?)```", open(sys.argv[1]).read(), re.S).group(1), end="")' templates/namespace-config/README.md > "$tmp/files.yaml"
check "alice adds the file browser of its README" set_stack media files "$tmp/files.yaml"
files_up() { garrison applied | grep -q "^app media/files@main files "; }
check "the deployer deploys it"            until_true 600 files_up
check "which makes the media library"      zfs list "${lib#/}"

echo "# ComfyUI, outside Garrison"
mkdir -p "$tmp/comfy" && cp test/e2e/media/picture.png test/e2e/media/clip.mp4 "$tmp/comfy/"
python3 test/e2e/media/comfy.py 8188 "$tmp/comfy" & comfy_pid=$!
trap 'kill $comfy_pid 2>/dev/null; rm -rf "$tmp"' EXIT
host_ip=$(ip route get 1.1.1.1 | awk '{for (i=1;i<NF;i++) if ($i=="src") print $(i+1)}')
sed -i '/^COMFYUI_URL=/d' $ETC/install.env && echo "COMFYUI_URL=http://$host_ip:8188" >> $ETC/install.env
docker service update --force --detach=false -q control-plane_agents >/dev/null 2>&1
agents_say() { docker service logs --since 3m control-plane_agents 2>&1 | grep -q "generate_media uses ComfyUI at http://$host_ip:8188"; }
check "the agent service uses it"          until_true 120 agents_say
check "the project media/demo goes to Gitea" installer/push-repo.sh --update media/demo test/e2e/media/demo

echo "# @media makes pictures and a video in the project's styles"
media_channel() { channel_of media alice >/dev/null; }
check "media has its channel"              until_true 120 media_channel
answer=$(ask_agent media alice "For the project media/demo: make two pictures of the fox riding a bike in the style comic, and one short video of the fox waving in the style motion. Do not ask back." media)
made() { [ "$(ls "$lib"/demo/*.png 2>/dev/null | wc -l)" -ge 2 ] && ls "$lib"/demo/*.mp4 >/dev/null 2>&1; }
if ! made; then
  printf 'retry @media did not make them; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent media alice "Please call generate_media now: project media/demo, style comic, prompt 'the fox riding a bike', count 2. Then generate_media with project media/demo, style motion, prompt 'the fox waving', count 1." media)
fi
check "two pictures and a video, in the library" made
first=$(ls "$lib"/demo/comic-*.png 2>/dev/null | head -n1)
check "each with a .json of how it was made" bash -c 'grep -q "\"style\": \"comic\"" "$0.json" && grep -q "\"project\": \"media/demo\"" "$0.json"' "$first"
check "ComfyUI got the style's prompt, size and length in their nodes" python3 -c '
import json,sys
runs=[json.loads(l) for l in open(sys.argv[1])]
pic=any(r.get("6",{}).get("inputs",{}).get("text","").endswith(", comic style, bold ink lines, flat bright colours") and r["5"]["inputs"]["width"] == 768 and r["7"]["inputs"]["text"] == "photo, blurry, scary" for r in runs)
vid=any(r.get("12",{}).get("inputs",{}).get("length") == 49 for r in runs)
sys.exit(0 if pic and vid else 1)' "$tmp/comfy/prompts.jsonl"
check "alice signs in to her dashboard"    until_true 60 home_login alice
check "the feed has what it made, for media's people" in_feed alice ns=media "e['kind'] == 'media' and e['agent'] == 'media/media'"

echo "# @media cuts a short from them"
answer=$(ask_agent media alice "Make a short vertical video of at most 15 seconds from the pictures and the video of the project demo. Do not ask back." media)
short() { ls "$lib"/demo/shorts/short-*.mp4 >/dev/null 2>&1; }
if ! short; then
  printf 'retry @media made no short; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent media alice "Please make it now with ffmpeg: the files in /data/media/demo/, 1080x1920, at most 15 seconds, written to /data/media/demo/shorts/short-<YYYYMMDD-HHMMSS>.mp4." media)
fi
check "a short in the library"             short
sandbox_image=$(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep '/garrison/sandbox:' | head -n1)
size=$(docker run --rm -v "$lib/demo/shorts:/s:ro" --entrypoint sh "$sandbox_image" -c 'ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$(ls /s/short-*.mp4 | head -n1)"' 2>/dev/null)
check "vertical, 9:16"                     python3 -c 'import sys; w,h=map(int,sys.argv[1].split(",")); sys.exit(0 if w*16 == h*9 else 1)' "$size"

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
past() { docker exec "$(docker ps -q -f name=files_web | head -n1)" wget -S -O /dev/null --header 'X-Garrison-User: alice' --header 'X-Auth-Request-Preferred-Username: alice' http://127.0.0.1:8080/api/resources/ 2>&1 | grep -q ' 403 '; }
check "past the sign-in, it takes nothing" past
kill $comfy_pid 2>/dev/null || true

echo "# a chain of command: software updates"
# An app of media pinned to an old release. alice asks the commander about
# updates; it hands the chain software-updates to its lead, the sentry,
# which asks the scout (the real web) and then the coder, in that order.
cat > "$tmp/greeter.yaml" <<'YAML'
stack: greeter
namespace: media
entrypoint: web
services:
  web: { image: traefik/whoami:v1.10.0, port: 80 }
environments:
  main: {}
YAML
check "media runs traefik/whoami:v1.10.0"  set_stack media greeter "$tmp/greeter.yaml"
asked_from=$(feed alice "/api/events?limit=1" | python3 -c 'import json,sys; e=json.load(sys.stdin); print(e[0]["id"] if e else 0)')
answer=$(ask_agent media alice "Are there software updates for the apps of the namespace media? If so, have them proposed." commander)
# update_pr: the open pull request in media/config, by alice, that changes
# only the tag of whoami in stacks/greeter.yaml to a newer one.
update_pr() {
  local n
  for n in $(gitea_api "" "$api/repos/media/config/pulls?state=open&limit=50" | python3 -c '
import json,sys
for p in json.load(sys.stdin):
    if p["user"]["login"] == "alice": print(p["number"])'); do
    gitea_api "" "$api/repos/media/config/pulls/$n.diff" | python3 -c '
import re,sys
d=sys.stdin.read()
minus=[l for l in d.splitlines() if l.startswith("-") and not l.startswith("---")]
plus=[l for l in d.splitlines() if l.startswith("+") and not l.startswith("+++")]
new=re.findall(r"traefik/whoami:v(\d+)\.(\d+)(?:\.(\d+))?", "\n".join(plus))
ok = "stacks/greeter.yaml" in d and len(minus) == 1 and len(plus) == 1 and "whoami:v1.10.0" in minus[0] and new and tuple(int(x or 0) for x in new[0]) > (1, 10, 0)
sys.exit(0 if ok else 1)' && { echo "$n"; return 0; }
  done
  return 1
}
if ! until_true 900 update_pr >/dev/null; then
  printf 'retry no update was proposed; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent media alice "Please hand this to the sentry as the chain software-updates says: check stacks/greeter.yaml of media/config (traefik/whoami:v1.10.0) for a newer release, and have the coder propose it." commander)
fi
check "an update is proposed: only the tag of whoami, newer" until_true 900 update_pr
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
