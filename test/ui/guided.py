"""Vague requests in todo's channel, and the agents guiding the person to
what they mean. todo has two apps, todo/app (the todo list) and todo/quotes,
so "the background" is ambiguous: the commander asks which, and the change
goes to the app's code, not its stack. An address for the app is the
opposite: its stack, in todo/config, not its code. See docs/scenarios.md,
"Vague requests". Run by test/ui/run.sh guided.
"""

import re

from lib import Person, browser, check, done, gitea, newest_pr, pr_files, section, until

NS, REPO, STACK = "todo", "app", "todo"
COMMANDER = "garrison/commander"
b = browser()
bob, carol = Person(b, "bob"), Person(b, "carol")


def asks_back(text: str, *words: str) -> bool:
    """An answer that asks the person, naming at least one of words."""
    low = text.lower()
    return "?" in text and any(w in low for w in words)


def newest(repo: str, after: int = 0):
    return newest_pr(NS, repo, "bob", after=after)


def last(repo: str) -> int:
    prs = gitea(f"/repos/{NS}/{repo}/pulls?state=all&limit=1") or []
    return prs[0]["number"] if prs else 0


def blue(colour: str) -> bool:
    """Whether a CSS colour (#rgb, #rrggbb, rgb(…) or a name) is a blue."""
    c = colour.strip().lower()
    if re.fullmatch(r"#[0-9a-f]{3}", c):
        c = "#" + "".join(ch * 2 for ch in c[1:])
    m = re.fullmatch(r"#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})", c)
    if m:
        r, g, b_ = (int(x, 16) for x in m.groups())
    elif m := re.fullmatch(r"rgba?\((\d+),\s*(\d+),\s*(\d+).*\)", c):
        r, g, b_ = (int(x) for x in m.groups())
    else:
        return any(w in c for w in ("blue", "navy", "azure", "steelblue", "royalblue"))
    return b_ > r + 20 and b_ >= g


def bg_of(diff: str) -> list[str]:
    """The background colours a diff sets: --bg, background or background-color."""
    return re.findall(r"^\+.*?(?:--bg|background(?:-color)?)\s*:\s*([^;]+);", diff, re.M)


section("1. bob asks, vaguely, for a blue background")
check("bob signs in to Open WebUI", bob.sign_in_chat)
check("carol signs in to Gitea", carol.sign_in_gitea)
before = last(REPO)
bob.channel(NS)
asked = bob.say("commander", "Please make the background blue.")
first = check("the commander answers", bob.next_answer, asked, COMMANDER, 0) or ""
print(f"it said: {first[:300]!r}", flush=True)
check("it asks which app: todo has two", lambda: asks_back(first, "quotes", "which", "todo"))
check("and changes nothing yet", lambda: newest(REPO, before) is None and newest("quotes") is None)
bob.reply(asked, "The todo list, its page.")
check("told, it goes on", bob.next_answer, asked, COMMANDER, 1)
pr = check("a pull request on todo/app, as bob", until, 900, lambda: newest(REPO, before), every=10)
if not pr:
    print("retry nothing proposed yet; asking plainly", flush=True)
    bob.reply(asked, "Please propose it now: in todo/app, set the page's background colour (--bg in page.html) to a blue.")
    pr = check("asked plainly, a pull request on todo/app", until, 900, lambda: newest(REPO, before), every=10)
diff = (gitea(f"/repos/{NS}/{REPO}/pulls/{pr}.diff") if pr else "") or ""
def sets_blue():
    if not any(blue(c) for c in bg_of(diff)):
        raise AssertionError(f"it sets {bg_of(diff) or 'no background'}")
    return True


check("it sets the background to a blue", sets_blue)
check("in the app's code, page.html, not its stack", lambda: pr_files(NS, REPO, pr) == ["page.html"])
check("todo/config is left alone", lambda: newest("config") is None)
check("carol approves it in Gitea", carol.approve, NS, REPO, pr)
check("and merges it", carol.merge, NS, REPO, pr)
check("the todo list turns blue, as carol sees it", until, 1200, lambda: blue(carol.background(STACK)), every=15)

section("2. bob asks for an address of the app's own")
before_cfg, before_app = last("config"), last(REPO)
bob.channel(NS)
asked = bob.say("commander", "Can the todo list have its own address, list.<our domain>?")
answer = check("the commander answers", bob.next_answer, asked, COMMANDER, 0) or ""
print(f"it said: {answer[:300]!r}", flush=True)
if not newest("config", before_cfg):
    bob.reply(asked, "Yes, please propose it.")
    check("told, it goes on", bob.next_answer, asked, COMMANDER, 1)
cfg = check("a pull request on todo/config, as bob", until, 900, lambda: newest("config", before_cfg), every=10)
# A stack's file name is its address: stacks/list.yaml answers at list.<domain>.
check("it changes the stacks, in todo/config", lambda: all(f.startswith("stacks/") for f in pr_files(NS, "config", cfg)))
check("for the address list", lambda: "stacks/list.yaml" in pr_files(NS, "config", cfg)
      or re.search(r"^\+.*\blist\b", gitea(f"/repos/{NS}/config/pulls/{cfg}.diff") or "", re.M))
check("and leaves the app's code alone", lambda: newest(REPO, before_app) is None)

done()
