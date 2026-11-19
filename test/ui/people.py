"""Joining and leaving a namespace, as each person sees it in Open WebUI.
alice, todo's only person, asks the commander, vaguely, to bring bob and
carol in; it asks which roles, and she answers. Then bob and carol find
todo's channel in their sidebar, with its how-to and its history. Later
alice says, as vaguely, that bob has left: he loses the channel, and its
messages are refused to him, while alice and carol keep it. See
docs/scenarios.md, "Joining and leaving". Run by test/ui/run.sh people.
"""

import yaml

from lib import Person, browser, check, done, gitea, newest_pr, raw, section, until

NS = "todo"
COMMANDER = "garrison/commander"
b = browser()
alice, bob, carol = Person(b, "alice"), Person(b, "bob"), Person(b, "carol")


def roster(ref: str = "main") -> dict:
    d = yaml.safe_load(raw(NS, "config", "members.yaml", ref) or "") or {}
    return {"admins": sorted(d.get("admins") or []), "members": sorted(d.get("members") or [])}


def pr_head(n: int) -> str:
    return gitea(f"/repos/{NS}/config/pulls/{n}")["head"]["sha"]


def members_pr(after: int):
    """The newest open pull request of todo/config by alice changing members.yaml."""
    return newest_pr(NS, "config", "alice", file="members.yaml", after=after)


def last_pr() -> int:
    prs = gitea(f"/repos/{NS}/config/pulls?state=all&limit=1") or []
    return prs[0]["number"] if prs else 0


def asks_back(text: str, *words: str) -> bool:
    """An answer that asks the person, naming at least one of words."""
    low = text.lower()
    return "?" in text and any(w in low for w in words)


section("1. before: todo is alice's alone")
for p in (alice, bob, carol):
    check(f"{p.name} signs in to Open WebUI", p.sign_in_chat)
check("alice has todo's channel in her sidebar", alice.sidebar_has, NS)
cid = check("she opens it", alice.channel, NS)
check("its how-to is pinned", lambda: any(m.get("is_pinned") and "How to ask the agents" in (m.get("content") or "")
                                          for m in alice.owui_list(f"/api/v1/channels/{cid}/messages?skip=0&limit=50")))
check("bob does not have it", lambda: not bob.sidebar_has(NS))
check("nor carol", lambda: not carol.sidebar_has(NS))
check("Open WebUI refuses bob its messages", lambda: not bob.reads(cid))

section("2. alice asks the commander, vaguely, to bring them in")
before = last_pr()
alice.channel(NS)
asked = alice.say("commander", "Can bob and carol help out here?")
first = check("the commander answers", alice.next_answer, asked, COMMANDER, 0) or ""
print(f"it said: {first[:300]!r}", flush=True)
check("it asks back, about their roles", lambda: asks_back(first, "admin", "member", "role"))
check("and proposes nothing yet", lambda: members_pr(before) is None)
alice.reply(asked, "bob as a member, carol as an admin.")
check("told, it goes on", alice.next_answer, asked, COMMANDER, 1)
pr = check("a pull request on todo/config changes members.yaml, as alice", until, 600, lambda: members_pr(before), every=10)
if not pr:
    print("retry the agent proposed nothing; asking plainly", flush=True)
    alice.reply(asked, "Please propose it now: bob under members, carol under admins, alice stays admin.")
    pr = check("asked plainly, it proposes it", until, 600, lambda: members_pr(before), every=10)
check("bob a member, carol an admin, alice still an admin",
      lambda: roster(pr_head(pr)) == {"admins": ["alice", "carol"], "members": ["bob"]})
# alice is todo's only person: her own change, which keeps her its admin, needs nobody else.
check("as todo's only person, her own change passes the check", until, 120,
      lambda: gitea(f"/repos/{NS}/config/commits/{pr_head(pr)}/status").get("state") == "success", every=5)
check("alice merges it in Gitea", alice.sign_in_gitea)
check("with its button", alice.merge, NS, "config", pr)

section("3. after: bob and carol see todo")
check("bob finds todo's channel in his sidebar", until, 120, bob.sidebar_has, NS, every=10)
check("so does carol", until, 120, carol.sidebar_has, NS, every=10)
check("bob opens it", bob.channel, NS)
check("he reads its history: alice's question", lambda: any("bob and carol help out" in (m.get("content") or "")
                                                            for m in bob.owui_list(f"/api/v1/channels/{cid}/messages?skip=0&limit=50")))
check("and its pinned how-to", lambda: any(m.get("is_pinned") for m in bob.owui_list(f"/api/v1/channels/{cid}/messages?skip=0&limit=50")))
answer = check("bob asks the operator there, which works in todo for him", bob.ask, NS, "operator",
               "Which stacks does this namespace have? Use your tools and list their names.") or ""
check("its answer names todo's stack", lambda: "todo" in answer.lower())

section("4. alice says, as vaguely, that bob has left")
before = last_pr()
alice.channel(NS)
asked = alice.say("commander", "bob isn't on the project any more.")
seen = 0
first = check("the commander answers", alice.next_answer, asked, COMMANDER, 0) or ""
print(f"it said: {first[:300]!r}", flush=True)
seen = 1
pr = None
try:
    pr = until(60, lambda: members_pr(before), every=10)
except AssertionError:
    # It asked first, as it may: alice confirms.
    alice.reply(asked, "Yes, remove him from todo.")
    check("told, it goes on", alice.next_answer, asked, COMMANDER, seen)
pr = pr or check("a pull request removes bob from members.yaml, as alice", until, 600, lambda: members_pr(before), every=10)
check("bob is out, carol and alice stay admins",
      lambda: roster(pr_head(pr)) == {"admins": ["alice", "carol"], "members": []})
check("carol approves it in Gitea", carol.sign_in_gitea)
check("with Review, Approve", carol.approve, NS, "config", pr)
check("the approval check passes", until, 120,
      lambda: gitea(f"/repos/{NS}/config/commits/{pr_head(pr)}/status").get("state") == "success", every=5)
check("alice merges it", alice.merge, NS, "config", pr)

section("5. after: bob no longer sees todo")
check("todo's channel is gone from bob's sidebar", until, 120, lambda: not bob.sidebar_has(NS), every=10)
check("Open WebUI refuses him its messages, even by its address", lambda: not bob.reads(cid))
check("alice still has it", alice.sidebar_has, NS)
check("so does carol", carol.sidebar_has, NS)
check("and carol reads its history", lambda: carol.reads(cid))

done()
