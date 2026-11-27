# animalfacts: a YouTube channel as a unit, made the way a person would make
# it: alice asks @designer to set up the project with no workflow of her own,
# merges what it proposes, and then asks for five narrated shorts of about 30
# seconds. Needs the real ComfyUI and text-to-speech service on the computer
# running the VM (COMFYUI=host) and a text-to-speech service such as Kokoro
# (TTS_URL; host: port 8880 of that computer); it draws on its GPU and takes long.
# Checks that the workflows name only models ComfyUI has, that every short
# looks and sounds alike (one picture style, one video style, one voice),
# that each is 9:16, about 30 s, with sound, and that the links it gives are
# real. The shorts land in ~/animalfacts.tar for a person to watch.
# Sourced by smoke.sh, whose helpers it uses.

section_animalfacts() {
echo "# animalfacts: a channel of narrated animal shorts, set up and made by @designer"
if [ "${COMFYUI:-fake}" != host ] || [ -z "${TTS_URL:-}" ]; then
  echo "skip: needs a real ComfyUI on the host and a text-to-speech service (COMFYUI=host TTS_URL=...)"
  return 0
fi
local host_ip lib_ds lib answers pa tts
host_ip=$(ip route | awk '/^default/ { print $3; exit }')
tts=$TTS_URL
[ "$tts" != host ] || tts=http://$host_ip:8880
lib_ds=garrison/ns/animalfacts/shares/animalfacts.shares.media
lib=$tmp/animalfacts
answers=$tmp/animalfacts-answers
: > "$answers"
pa=$api/repos/animalfacts/shorts

check "a real ComfyUI answers on the host, with a GPU" bash -c \
  'curl -sf --max-time 10 "http://$0:8188/system_stats" | grep -q "\"type\": \"cuda\""' "$host_ip"
check "a text-to-speech service answers"   bash -c \
  'curl -sf --max-time 10 "$0/v1/voices" | grep -q voices' "$tts"
check "alice signs in to the chat"         chat_login alice

echo "# the unit animalfacts, with its file browser"
code=$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"username":"animalfacts","visibility":"limited","description":"Our YouTube channel Animal Facts"}' "$api/orgs")
check "alice creates the unit animalfacts" bash -c "[ $code = 201 ] || [ $code = 422 ]"
af_config() { [ "$(gitea_api "" -o /dev/null -w '%{http_code}' "$api/repos/animalfacts/config/raw/members.yaml?ref=main")" = 200 ]; }
check "animalfacts has its config"         until_true 120 af_config
python3 -c '
import re,sys
print(re.search(r"## Media\n.*?```yaml\n(.*?)```", open(sys.argv[1]).read(), re.S).group(1), end="")' templates/unit-config/README.md > "$tmp/af-files.yaml"
check "alice adds the file browser of its README" set_stack animalfacts files "$tmp/af-files.yaml"
af_files_up() { garrison applied | grep -q "^app animalfacts/files@main files "; }
check "the deployer deploys it"            until_true 600 af_files_up
check "which makes the media library"      zfs list "$lib_ds"
mkdir -p "$lib" && mount -t zfs "$lib_ds" "$lib"

echo "# ComfyUI and the voice, outside Garrison"
sed -i '/^COMFYUI_URL=/d; /^TTS_URL=/d' $ETC/install.env
{ echo "COMFYUI_URL=http://$host_ip:8188"; echo "TTS_URL=$tts"; } >> $ETC/install.env
kubectl --namespace garrison rollout restart deployment/agents >/dev/null && kubectl --namespace garrison rollout status deployment/agents --timeout 300s >/dev/null 2>&1
af_agents_say() {
  local l; l=$(kubectl --namespace garrison logs deployment/agents --since=3m 2>&1)
  grep -q "generate_media uses ComfyUI at http://$host_ip:8188" <<<"$l" && grep -qF "generate_speech uses the text-to-speech service at $tts" <<<"$l"
}
check "the agent service uses both"        until_true 120 af_agents_say
check "the project animalfacts/shorts, only its README" installer/push-repo.sh --update animalfacts/shorts test/e2e/media/animalfacts
af_channel() { channel_of animalfacts alice >/dev/null; }
check "animalfacts has its channel"        until_true 120 af_channel

echo "# @designer sets the project up: no workflow given, it writes them"
af_setup_pr() {
  gitea_api "" "$pa/pulls?state=open&limit=20" | python3 -c '
import json,sys
for p in json.load(sys.stdin):
    print(p["number"]); sys.exit(0)
sys.exit(1)'
}
answer=$(ASK_WAIT=660 ask_agent animalfacts alice "Set up the project animalfacts/shorts for our channel (read its README): a picture style for key pictures and a video clip style that starts from a key picture, so every clip of a short shows the same animal, both in the channel's one look (our shorts show video clips only, no still pictures), and a calm, friendly narrator voice. We have no ComfyUI workflow, so write them for the models our ComfyUI has. Propose it all as one pull request. Do not ask back." designer)
echo "$answer" >> "$answers"
pr=$(until_true 60 af_setup_pr >/dev/null && af_setup_pr)
if [ -z "$pr" ]; then
  printf 'retry @designer proposed nothing; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ASK_WAIT=660 ask_agent animalfacts alice "Please call media_models now, then propose to animalfacts/shorts in one pull request: workflows/key.json (the Z-Image example) with styles/key.yaml at 704x1280, workflows/clip.json (the Wan 2.2 image-to-video example) with styles/clip.yaml, both in the README's look, and narration.yaml with a calm voice." designer)
  echo "$answer" >> "$answers"
  pr=$(until_true 120 af_setup_pr >/dev/null && af_setup_pr)
fi
check "it proposes the setup as a pull request" test -n "$pr"
# What the pull request adds, as it would be on main. By its commit: an
# agent's branch has a / in its name (change/...), which a path cannot take.
af_pr_files() {
  local head; head=$(gitea_api "" "$pa/pulls/$pr" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')
  rm -rf "$tmp/af-pr" && mkdir -p "$tmp/af-pr"
  gitea_api "" "$pa/git/trees/$head?recursive=true" | python3 -c '
import json,sys
for e in json.load(sys.stdin).get("tree", []):
    if e["type"] == "blob": print(e["path"])' | while read -r f; do
    mkdir -p "$tmp/af-pr/$(dirname "$f")"
    gitea_api "" "$pa/raw/$f?ref=$head" > "$tmp/af-pr/$f"
  done
}
af_pr_files
# models_ok: every model file a workflow names is one ComfyUI has.
models_ok() {
  python3 - "$tmp/af-pr" "$host_ip" <<'PY'
import glob, json, sys, urllib.request
root, host = sys.argv[1], sys.argv[2]
have = set()
for folder in json.load(urllib.request.urlopen(f"http://{host}:8188/models", timeout=10)):
    try:
        have |= set(json.load(urllib.request.urlopen(f"http://{host}:8188/models/{folder}", timeout=10)))
    except Exception:
        pass
bad, named = [], 0
for f in glob.glob(root + "/workflows/*.json"):
    for node in json.load(open(f)).values():
        for k, v in node.get("inputs", {}).items():
            if isinstance(v, str) and v.endswith((".safetensors", ".ckpt", ".pt", ".pth", ".gguf", ".bin")):
                named += 1
                if v not in have: bad.append(f"{f.split('/')[-1]}: {k}={v}")
if bad or not named:
    print("      not on ComfyUI:", bad or "no model named"); sys.exit(1)
PY
}
check "its workflows name only models ComfyUI has" models_ok
# kinds: a picture style and a video style, each naming a workflow it adds.
af_kinds() {
  python3 - "$tmp/af-pr" <<'PY'
import glob, json, os, re, sys
root = sys.argv[1]
kinds = set()
for f in glob.glob(root + "/styles/*.yaml"):
    text = open(f).read()
    m = re.search(r"^workflow:\s*[\"']?([^\s\"'#]+)", text, re.M)
    wf = os.path.join(root, m.group(1)) if m else ""
    if not os.path.isfile(wf) or not re.search(r"^\s+prompt:", text, re.M): continue
    classes = {n.get("class_type", "") for n in json.load(open(wf)).values()}
    video = any("Video" in c for c in classes if c.startswith("Save"))
    if video and re.search(r"^\s+image:", text, re.M) and "LoadImage" in classes:
        kinds.add("video from a picture")
    elif not video and "SaveImage" in classes:
        kinds.add("picture")
print("      styles:", sorted(kinds))
sys.exit(0 if {"picture", "video from a picture"} <= kinds else 1)
PY
}
check "a picture style, and a video style that starts from a picture" af_kinds
check "and the project's voice"            bash -c 'grep -qE "^voice: *[a-z]{2}_[a-z]+" "$0/narration.yaml"' "$tmp/af-pr"
check "alice merges it"                    test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$pa/pulls/$pr/merge")" = 200

echo "# five narrated shorts of about 30 seconds"
# af_merge_fixes: alice merges what @designer proposed in the project since,
# as a person would when it says a fix waits for her.
af_merge_fixes() {
  local n
  for n in $(gitea_api "" "$pa/pulls?state=open&limit=20" | python3 -c '
import json,sys
for p in json.load(sys.stdin): print(p["number"])'); do
    echo "      alice merges #$n: $(gitea_api "" "$pa/pulls/$n" | python3 -c 'import json,sys; print(json.load(sys.stdin)["title"])')"
    gitea_api alice -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$pa/pulls/$n/merge"
  done
}
af_shorts() { find "$lib/shorts/shorts" -name '*.mp4' -size +0 2>/dev/null | wc -l; }
for animal in octopus axolotl hummingbird "sea otter" pangolin; do
  af_merge_fixes
  before=$(af_shorts)
  answer=$(ASK_WAIT=1860 ask_agent animalfacts alice "Make the next short for the channel, about the $animal: one surprising, true fact, spoken by the project's narrator, about 30 seconds, made of video clips of the same animal (one key picture, then every clip from it), no still pictures shown. Do not ask back." designer)
  echo "$answer" >> "$answers"
  if [ "$(af_shorts)" -le "$before" ]; then
    printf 'retry @designer made no short of the %s; it said: %s\n' "$animal" "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
    af_merge_fixes
    answer=$(ASK_WAIT=1860 ask_agent animalfacts alice "Please make the $animal short now: generate_speech with the fact (about 80 words), then one key picture of the $animal with the picture style, then generate_media with the video style, count 6, image the key picture, then cut the clips with ffmpeg to the narration's length, 1080x1920, crossfades, loudness -14 LUFS, with the narration as its sound, into /data/media/shorts/shorts/." designer)
    echo "$answer" >> "$answers"
  fi
  check "a short of the $animal"           test "$(af_shorts)" -gt "$before"
done
check "five shorts in the library"         test "$(af_shorts)" -ge 5

echo "# the dashboard's agent pages: each turn of @designer, step by step"
# Its turns in the channel are for animalfacts' people: alice sees them with
# their tool calls; bob, in no unit here, sees none of them.
af_run() { feed alice "/agents/animalfacts/designer" | grep -o 'href="/runs/[0-9]*"' | head -n1 | grep -o '[0-9]*'; }
run_id=$(until_true 60 af_run >/dev/null && af_run)
check "alice's Agents page lists @designer's turns" test -n "$run_id"
check "a turn shows its thinking and tool calls" bash -c 'grep -q "\"kind\":\"tool\"" <<<"$0" && grep -q "\"kind\":\"thinking\"" <<<"$0" && grep -q "generate_" <<<"$0"' "$(feed alice "/api/runs/$run_id")"
# bob sees his own turns (as in the fixture agent), none of animalfacts'.
check "bob, not in animalfacts, sees none of it" bash -c '! grep -q "generate_" <<<"$0" && ! grep -q "animalfacts" <<<"$1"' "$(feed bob "/api/runs/$run_id")" "$(feed bob "/agents")"

# Each short: 9:16, 25 to 35 seconds, with a sound track.
af_probe() {
  image_pod "af-probe-$RANDOM" -v "$lib/shorts:/d" -- sh -c '
    for f in $(find /d/shorts -name "*.mp4" -size +0); do
      v=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$f")
      a=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_type -of csv=p=0 "$f")
      d=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$f")
      echo "${f##*/} $v ${a:-none} $d"
    done' 2>/dev/null
}
af_probe > "$tmp/af-probe"
sed 's/^/      /' "$tmp/af-probe"
check "every short is 9:16"                python3 -c '
import sys
rows=[l.split() for l in open(sys.argv[1]) if l.strip()]
sys.exit(0 if rows and all((lambda w,h: w*16 == h*9)(*map(int, r[1].split(",")[:2])) for r in rows) else 1)' "$tmp/af-probe"
check "every short has a sound track"      bash -c '! grep -q " none " "$0" && [ -s "$0" ]' "$tmp/af-probe"
check "every short is 25 to 35 seconds"    python3 -c '
import sys
rows=[l.split() for l in open(sys.argv[1]) if l.strip()]
sys.exit(0 if rows and all(25 <= float(r[3]) <= 35 for r in rows) else 1)' "$tmp/af-probe"

# The same look and voice in all of them: what generate_media and
# generate_speech made, by the .json beside each file.
af_alike() {
  python3 - "$lib/shorts" "$1" <<'PY'
import glob, json, os, sys
root, what = sys.argv[1], sys.argv[2]
made = [json.load(open(f)) for f in glob.glob(root + "/*.json")]
voices = {m.get("voice") for m in made if "narration" in m}
froms = {m.get("voice_from") for m in made if "narration" in m}
if what == "styles":
    by_kind = {}
    for f in glob.glob(root + "/*.json"):
        m = json.load(open(f)); base = f[:-5]
        if "style" not in m: continue
        by_kind.setdefault(os.path.splitext(base)[1], set()).add(m["style"])
    print("      styles by file kind:", {k: sorted(v) for k, v in by_kind.items()})
    sys.exit(0 if by_kind and all(len(v) == 1 for v in by_kind.values()) else 1)
if what == "voice":
    n = sum(1 for m in made if "narration" in m)
    print(f"      {n} narrations, voices {sorted(v for v in voices if v)}, from {sorted(f for f in froms if f)}")
    sys.exit(0 if n >= 5 and len(voices) == 1 and froms == {"narration.yaml"} else 1)
PY
}
check "one video style in all of them"     af_alike styles
check "one voice in all of them, the project's" af_alike voice
# Moving pictures, not stills: no stretch over 2 seconds without motion;
# and sound at YouTube's loudness, 48 kHz stereo.
af_motion() {
  image_pod "af-motion-$RANDOM" -v "$lib/shorts:/d" -- sh -c '
    for f in $(find /d/shorts -name "*.mp4" -size +0); do
      frozen=$(ffmpeg -nostats -i "$f" -vf "freezedetect=n=0.003:d=2" -an -f null - 2>&1 | grep -c freeze_start)
      lufs=$(ffmpeg -nostats -i "$f" -vn -af ebur128 -f null - 2>&1 | sed -n "s/^ *I: *\(-*[0-9.]*\) LUFS/\1/p" | tail -n1)
      audio=$(ffprobe -v error -select_streams a:0 -show_entries stream=sample_rate,channels -of csv=p=0 "$f")
      echo "${f##*/} frozen=$frozen lufs=$lufs audio=$audio"
    done' 2>/dev/null
}
af_motion > "$tmp/af-motion"
sed 's/^/      /' "$tmp/af-motion"
check "every short moves: no still stretch over 2 s" bash -c '[ -s "$0" ] && ! grep -v " frozen=0 " "$0" | grep -q .' "$tmp/af-motion"
check "and sounds at -14 LUFS, 48 kHz stereo" python3 -c '
import re,sys
rows=[l for l in open(sys.argv[1]) if l.strip()]
ok=rows and all(-17 <= float(re.search(r"lufs=(-?[0-9.]+)", r).group(1)) <= -11 and "audio=48000,2" in r for r in rows)
sys.exit(0 if ok else 1)' "$tmp/af-motion"
# The same animal in every clip of a short: the clips it is made of (its
# .json lists them) all started from one key picture.
af_one_character() {
  python3 - "$lib/shorts" <<'PY'
import glob, json, os, sys
root = sys.argv[1]
bad, n = [], 0
for f in glob.glob(root + "/shorts/*.json"):
    files = json.load(open(f)).get("files") or []
    clips = [os.path.join(root, os.path.basename(c)) for c in files if str(c).endswith(".mp4")]
    images = set()
    for c in clips:
        try: images.add(json.load(open(c + ".json")).get("image"))
        except Exception: images.add(None)
    n += 1
    if not clips or None in images or len(images) != 1: bad.append(f"{os.path.basename(f)}: {sorted(map(str, images))}")
print(f"      {n} shorts;", "; ".join(bad) or "each from one key picture")
sys.exit(0 if n and not bad else 1)
PY
}
check "every clip of a short starts from its one key picture" af_one_character

echo "# a public website of the channel, from the template channel"
# site_pr: alice's open pull request in animalfacts/config adding a stack
# that mounts the library and comes from a repo made from the template
# channel; prints "<number> <stack>".
site_pr() {
  local n f
  for n in $(gitea_api "" "$api/repos/animalfacts/config/pulls?state=open&limit=20" | python3 -c '
import json,sys
for p in json.load(sys.stdin):
    if p["user"]["login"] == "alice": print(p["number"])'); do
    for f in $(gitea_api "" "$api/repos/animalfacts/config/pulls/$n/files" | python3 -c '
import json,sys
for f in json.load(sys.stdin):
    if f["filename"].startswith("stacks/") and f["filename"].endswith(".yaml"): print(f["filename"])'); do
      local head body repo
      head=$(gitea_api "" "$api/repos/animalfacts/config/pulls/$n" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')
      body=$(gitea_api "" "$api/repos/animalfacts/config/raw/$f?ref=$head")
      grep -q 'share: *media' <<<"$body" || continue
      repo=$(sed -n 's/^source: *"\{0,1\}\([A-Za-z0-9._-]*\).*/\1/p' <<<"$body")
      [ -n "$repo" ] && gitea_api "" "$api/repos/animalfacts/$repo/raw/README.md" | grep -q "public page of the unit" || continue
      echo "$n $(basename "$f" .yaml)"; return 0
    done
  done
  return 1
}
answer=$(ASK_WAIT=660 ask_agent animalfacts alice "Make a public website for our channel Animal Facts that lists all our shorts with what is said in them, for everyone without a sign-in. Call it animalfacts-site. Do not ask back." operator)
echo "$answer" >> "$answers"
if ! until_true 120 site_pr >/dev/null; then
  printf 'retry the operator proposed no site; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  # A new name: a repo made without the template cannot be given it after.
  answer=$(ASK_WAIT=660 ask_agent animalfacts alice "Please create the repo animalfacts-page in animalfacts with the template channel, then propose its stack from its README (public, environments main: {}) in animalfacts/config." operator)
  echo "$answer" >> "$answers"
fi
read -r site_n site <<<"$(until_true 120 site_pr >/dev/null && site_pr)"
check "the operator makes the site and proposes its stack" test -n "$site"
site_merged() {
  local cfg=$api/repos/animalfacts/config st i
  gitea_api "" -o /dev/null -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"Looks right."}' "$cfg/pulls/$site_n/reviews"
  for i in $(seq 60); do
    st=$(gitea_api "" "$cfg/commits/$(gitea_api "" "$cfg/pulls/$site_n" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"]["sha"])')/status" | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("statuses") or []:
    if s["context"] == "garrison/approval": print(s["status"]); break' 2>/dev/null)
    [ "$st" = success ] && break
    sleep 2
  done
  for i in $(seq 15); do
    [ "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$cfg/pulls/$site_n/merge")" = 200 ] && return 0
    sleep 2
  done
  return 1
}
check "alice merges it"                    site_merged
site_up() { garrison applied | grep -q "^app animalfacts/$site@main "; }
check "it is built and deployed"           until_true 1200 site_up
# Everyone, without a sign-in: no cookies.
site_page() { req "$site" "https://$site.$DOMAIN/" > "$tmp/site.html" && grep -q "<article" "$tmp/site.html"; }
check "it opens without a sign-in"         until_true 120 site_page
check "it lists the five shorts"           test "$(grep -c '<article' "$tmp/site.html")" -ge 5
site_texts() {
  python3 - "$tmp/site.html" "$lib/shorts" <<'PY'
import glob, html, json, re, sys
page = html.unescape(open(sys.argv[1]).read())
said = [json.load(open(f)).get("narration", "") for f in glob.glob(sys.argv[2] + "/narration-*.json")]
said = [s.strip() for s in said if s.strip()]
shown = sum(1 for s in said if s[:40] in page)
print(f"      {shown} of {len(said)} narrations on the page")
sys.exit(0 if said and shown >= min(5, len(said)) else 1)
PY
}
check "with what is said in each"          site_texts
video=$(grep -o 'src="/video/[^"]*"' "$tmp/site.html" | head -n1 | sed 's/src="//; s/"$//')
check "a short plays, and seeks"           test "$(req "$site" -o /dev/null -w '%{http_code}' -r 0-1023 "https://$site.$DOMAIN$video")" = 206
check "but no clip of the library, only shorts" test "$(req "$site" -o /dev/null -w '%{http_code}' "https://$site.$DOMAIN/video/shorts/$(basename "$(ls "$lib"/shorts/*.mp4 | head -n1)")")" = 404
req "$site" "https://$site.$DOMAIN/" > /home/ubuntu/animalfacts-site.html 2>/dev/null || true

echo "# storyboards: stories written by agents, reviewed by alice, made into shorts by themselves"
sb=$api/repos/animalfacts/storyboards
answer=$(ask_agent animalfacts alice "Create the repo storyboards in animalfacts from the template storyboards. Do not ask back." operator)
echo "$answer" >> "$answers"
sb_repo() { gitea_api "" "$sb/raw/README.md" | grep -q "chain of command \`storyboards\`"; }
if ! until_true 60 sb_repo; then
  printf 'retry the operator made no storyboards repo; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ask_agent animalfacts alice "Please call create_repo now: unit animalfacts, name storyboards, template storyboards." operator)
fi
check "the operator makes storyboards from its template" until_true 60 sb_repo
# The routine that makes a merged story's short, as the repo's README gives it.
gitea_api "" -f "$api/repos/animalfacts/config/raw/routines.yaml" > "$tmp/af-routines.yaml" 2>/dev/null || : > "$tmp/af-routines.yaml"
cat >> "$tmp/af-routines.yaml" <<'YAML'
- agent: designer
  name: shorts-from-stories
  on_push: { repo: storyboards, branch: main, paths: [stories/] }
  owner: alice
  task: Make the short of the next story of animalfacts/storyboards that has none yet.
YAML
check "alice adds the routine of its README" set_config animalfacts routines.yaml "$tmp/af-routines.yaml"
# The routine must see main before the merge, so the merge is news to it.
sleep 90
sb_pr() {
  gitea_api "" "$sb/pulls?state=open&limit=20" | python3 -c '
import json,sys
for p in json.load(sys.stdin):
    print(p["number"], p["head"]["sha"]); sys.exit(0)
sys.exit(1)'
}
answer=$(ASK_WAIT=900 ask_agent animalfacts alice "Write two new stories for animalfacts/storyboards. Do not ask back." commander)
echo "$answer" >> "$answers"
if ! until_true 900 sb_pr >/dev/null; then
  printf 'retry no stories were proposed; it said: %s\n' "$(head -c 300 <<<"$answer" | tr '\n' ' ')"
  answer=$(ASK_WAIT=900 ask_agent animalfacts alice "Please run the chain storyboards for animalfacts/storyboards now: two new stories, proposed as one pull request." coder)
  echo "$answer" >> "$answers"
fi
read -r sb_n sb_sha <<<"$(until_true 120 sb_pr >/dev/null && sb_pr)"
check "the chain proposes new stories"     test -n "$sb_n"
# Each story as the README asks: sources, 75 to 90 spoken words, 5 to 7
# scenes, all clips; and only stories/ changed.
sb_stories_ok() {
  local f
  rm -rf "$tmp/sb" && mkdir -p "$tmp/sb"
  for f in $(gitea_api "" "$sb/pulls/$sb_n/files" | python3 -c '
import json,sys
for f in json.load(sys.stdin): print(f["filename"])'); do
    case $f in stories/*.md) gitea_api "" "$sb/raw/$f?ref=$sb_sha" > "$tmp/sb/${f#stories/}" ;; *) echo "      not a story: $f"; return 1 ;; esac
  done
  python3 - "$tmp/sb" <<'PY'
import glob, re, sys
files = glob.glob(sys.argv[1] + "/*.md")
bad = []
for f in files:
    t = open(f).read()
    name = f.split("/")[-1]
    narr = re.search(r"^## Narration\s*\n(.*?)(?=^## |\Z)", t, re.S | re.M)
    scenes = re.findall(r"^\s*\d+\.\s*(picture|clip):", t, re.M | re.I)
    words = len(narr.group(1).split()) if narr else 0
    if not re.search(r"^title:", t, re.M): bad.append(f"{name}: no title")
    if not re.search(r"https://", t): bad.append(f"{name}: no source")
    if not 65 <= words <= 100: bad.append(f"{name}: {words} words")
    if not 5 <= len(scenes) <= 7 or any(s.lower() != "clip" for s in scenes): bad.append(f"{name}: scenes {scenes}")
print(f"      {len(files)} stories", "; ".join(bad))
sys.exit(0 if len(files) >= 2 and not bad else 1)
PY
}
check "two stories: sources, narration, clip scenes" sb_stories_ok
sb_merged=$(gitea_api "" "$sb/pulls/$sb_n" | python3 -c 'import json,sys; print(json.load(sys.stdin)["merged"])')
check "they wait for a person: not merged by the chain" test "$sb_merged" = False
sb_first=$(ls "$tmp/sb" | sort | head -n1); sb_first=${sb_first%.md}
check "alice reviews and merges them"      test "$(gitea_api alice -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' "$sb/pulls/$sb_n/merge")" = 200
sb_short() { [ -s "$lib/shorts/shorts/$sb_first.mp4" ] && grep -q "storyboards/stories/$sb_first.md" "$lib/shorts/shorts/$sb_first.mp4.json"; }
check "the routine has @designer make the first story's short" until_true 1900 sb_short
sb_on_site() { req "$site" "https://$site.$DOMAIN/" | grep -q "/video/shorts/$sb_first.mp4"; }
check "and the website shows it by itself" until_true 120 sb_on_site
for f in "$tmp/sb"/*.md; do cp "$f" "$lib/shorts/story-${f##*/}" 2>/dev/null || true; done

echo "# the links it gives are real"
af_links() {
  python3 - "$answers" "$DOMAIN" "$site" <<'PY'
import re, sys
text, domain = open(sys.argv[1]).read(), sys.argv[2]
hosts = {h.lower() for h in re.findall(r"https?://([A-Za-z0-9.-]+)", text)}
known = {domain, "git." + domain, "chat." + domain, "files." + domain} | {h + "." + domain for h in sys.argv[3:] if h}
bad = hosts - known
if bad: print("      made up:", sorted(bad))
sys.exit(1 if bad else 0)
PY
}
check "no made-up address"                 af_links
check "it links the file browser"          grep -q "https://files.$DOMAIN" "$answers"
check "alice signs in to her dashboard"    until_true 60 home_login alice
check "the feed has what it made, for animalfacts' people" in_feed alice ns=animalfacts "e['kind'] == 'media' and e['agent'] == 'animalfacts/designer'"

# For a person to watch: the shorts, their narrations and what each was made of.
tar -C "$lib/shorts" -cf /home/ubuntu/animalfacts.tar . 2>/dev/null && chmod 644 /home/ubuntu/animalfacts.tar
umount "$lib" 2>/dev/null || true
}
