#!/usr/bin/env python3
"""agents-live.py [VM]: the agents at work in a test VM, live: each sandbox's
thinking, what it says, its tool calls and their results, as they happen.

    test/vm/agents-live.py                       # garrison-k3s
    test/vm/agents-live.py garrison-k3s-media    # a scenario's VM
    test/vm/agents-live.py VM --all              # also what ran before now

It reads the sandbox pods' logs (Pi's JSON events) through
`multipass exec ... kubectl logs -f`, one follower per pod, and picks up new
pods as they start. Read-only: it changes nothing in the VM.
"""
import json
import subprocess
import sys
import textwrap
import threading
import time

VM = next((a for a in sys.argv[1:] if not a.startswith("-")), "garrison-k3s")
SINCE = [] if "--all" in sys.argv else ["--since=1s"]
NS = "garrison-sandboxes"
COLOURS = ["36", "35", "33", "32", "34", "31"]
lock = threading.Lock()
width = 110


def say(who, colour, mark, text, limit=None):
    text = " ".join((text or "").split())
    if limit and len(text) > limit:
        text = text[:limit] + " …"
    lines = textwrap.wrap(text, width, break_long_words=True) or [""]
    with lock:
        stamp = time.strftime("%H:%M:%S")
        print(f"\033[2m{stamp}\033[0m \033[{colour}m{who:<22}\033[0m {mark} {lines[0]}")
        for line in lines[1:]:
            print(" " * 33 + line)
        sys.stdout.flush()


def short(args):
    s = json.dumps(args, ensure_ascii=False)
    return s if len(s) < 400 else s[:400] + " …"


def follow(pod, who, colour):
    cmd = ["multipass", "exec", VM, "--", "sudo", "kubectl", "-n", NS, "logs", "-f", pod] + SINCE
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    for line in p.stdout:
        try:
            e = json.loads(line)
        except ValueError:
            continue
        t = e.get("type")
        ev = e.get("assistantMessageEvent") or {}
        k = ev.get("type", "")
        if t == "turn_start":
            say(who, colour, "▶", "\033[2mturn\033[0m")
        elif k == "thinking_end":
            say(who, colour, "💭", ev.get("content", "") or last(ev, "thinking"), 1200)
        elif k == "text_end":
            say(who, colour, "💬", ev.get("content", "") or last(ev, "text"), 2000)
        elif t == "tool_execution_start":
            say(who, colour, "🔧", f"{e.get('toolName')} {short(e.get('args'))}")
        elif t == "tool_execution_end":
            r = e.get("result") or {}
            text = " ".join(c.get("text", "") for c in r.get("content", []) if isinstance(c, dict))
            say(who, colour, "✗" if e.get("isError") else "✓", f"{e.get('toolName')}: {text}", 600)
    p.wait()


def last(ev, kind):
    """The last part of kind in the message so far, for events without content."""
    for part in reversed((ev.get("partial") or {}).get("content") or []):
        if part.get("type") == kind:
            return part.get(kind, "")
    return ""


def pods():
    out = subprocess.run(["multipass", "exec", VM, "--", "sudo", "kubectl", "-n", NS, "get", "pods",
                          "-o", "jsonpath={range .items[?(@.status.phase=='Running')]}{.metadata.name} {.metadata.labels.garrison\\.agent} {.metadata.labels.garrison\\.person}{'\\n'}{end}"],
                         capture_output=True, text=True).stdout
    return [l.split() for l in out.splitlines() if len(l.split()) == 3]


def main():
    print(f"Agents in {VM}, live (Ctrl-C to stop). 💭 thinking, 💬 says, 🔧 calls a tool, ✓/✗ its result.")
    seen = {}
    while True:
        for name, agent, person in pods():
            if name not in seen:
                colour = COLOURS[len(seen) % len(COLOURS)]
                who = f"{agent.replace('_', '/')} ({person})"
                seen[name] = threading.Thread(target=follow, args=(name, who, colour), daemon=True)
                seen[name].start()
                say(who, colour, "●", f"\033[2msandbox {name}\033[0m")
        time.sleep(5)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        pass
