#!/usr/bin/env bash
# test/e2e/ask-bank.sh [QUESTIONS] [OUT]: asks the agents every question of
# test/e2e/questions.tsv (person, agent, question), each in a fresh private
# chat with an id, as the browser's, so it runs in a sandbox, and writes one JSON line per answer to OUT (/var/tmp/ask-bank.jsonl):
# who asked whom what, the answer, how long it took, and flags for answers
# that went wrong before (asking back without naming the choices, asking leave
# for a question that only looks, a cut-off message, a made-up address). Not
# a test that passes or fails: its answers are read, and the prompts made
# better. Runs on the machine, as root, from the repo root, from the fixture
# team (test/vm/ask-bank.sh runs it in a VM and fetches OUT).
set -uo pipefail
. test/e2e/lib.sh
bank=${1:-test/e2e/questions.tsv}
out=${2:-/var/tmp/ask-bank.jsonl}
: > "$out"
declare -A in=()
n=0
# Right after a VM starts, the agents answer a while later than the chat:
# the questions wait for a first answer.
chat_login bob >/dev/null 2>&1 || true
ready() { ask_chat bob garrison/commander "Answer with one word: ready." >/dev/null 2>&1; }
until_true 600 ready || echo "warn  the agents do not answer yet; asking anyway" >&2
while IFS=$'\t' read -r who agent question; do
  [ -n "$who" ] && [ "${who:0:1}" != "#" ] || continue
  if [ -z "${in[$who]:-}" ]; then
    chat_login "$who" || echo "warn  $who could not sign in to the chat" >&2
    in[$who]=1
  fi
  n=$((n + 1))
  t0=$SECONDS
  answer=$(ask_chat "$who" "garrison/$agent" "$question" 2>&1) || answer="(no answer: $answer)"
  took=$((SECONDS - t0))
  python3 - "$out" "$n" "$who" "$agent" "$question" "$answer" "$took" "$DOMAIN" <<'PY'
import json, re, sys
out, n, who, agent, q, a, took, domain = sys.argv[1:]
low = a.lower()
flags = []
if re.search(r"which unit|which app|which one", low) and not re.search(r"\b(todo|garrison)\b", low):
    flags.append("asks back without naming the choices")
if re.search(r"want me to|shall i|should i (ask|hand|have)|do you want me", low):
    flags.append("asks leave")
if re.search(r"\bask (@?\w+ )?(in|there)|mention @\w+|talk to \*\*@|ask \*\*@", low) and agent == "commander":
    flags.append("sends the person away instead of asking the agent")
if re.search(r"cut off|incomplete|got truncated", low):
    flags.append("thinks the message was cut off")
if re.search(r"<domain>|example\.(com|org)|yourdomain|your-domain", low) or agent != "scout" and re.search(r"https?://[a-z0-9.-]+", low) and not all(domain in u for u in re.findall(r"https?://[a-z0-9.-]+", low)):
    flags.append("an address that is not this installation's")
if a.startswith("(no answer") or not a.strip():
    flags.append("no answer")
with open(out, "a") as f:
    f.write(json.dumps({"n": int(n), "who": who, "agent": agent, "question": q, "answer": a, "seconds": int(took), "flags": flags}) + "\n")
print(f"{int(n):3d} {who:6s} {agent:10s} {int(took):4d}s {'; '.join(flags) or 'ok'}  | {q}", flush=True)
PY
done < "$bank"
echo "asked $n questions; the answers are in $out"
