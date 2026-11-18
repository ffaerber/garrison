"""The UI scenarios: everything a person does, clicked and typed in Gitea,
Open WebUI and the dashboard, from the namespace todo to its todo app deployed, a
screenshot, a scheduled job and a question a routine asks. See
docs/scenarios.md, "In the browser". Run by test/ui/run.sh.
"""

import os
import time
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

from lib import (CHAT, channel_id, DOMAIN, GIT, HOME, Person, approval, browser, check, done, gitea, newest_pr, pr_files,
                 raw, runs_of, section, until)

# The app is the template todo: the repo REPO, deployed as STACK at
# https://STACK.<domain>, as the deploy step of the VM test does it.
NS = os.environ.get("UI_NAMESPACE", "todo")
REPO, STACK = os.environ.get("UI_REPO", "app"), os.environ.get("UI_STACK", "todo")
HEADING = "<h1>Todo</h1>"
GREEN = "rgb(46, 125, 50)"  # #2e7d32
GREY = "rgb(246, 247, 249)"  # #f6f7f9, the template's --bg
ZONE = ZoneInfo(os.environ.get("TIMEZONE") or "UTC")

b = browser()
alice, bob, admin = Person(b, "alice"), Person(b, "bob"), Person(b, os.environ["ADMIN_USER"], os.environ["ADMIN_PASSWORD"])


def retry(person, channel, agent, ask, again, found):
    """Asks an agent; if found() has nothing, asks again more plainly, as a
    person would. Returns what found() returns."""
    answer = person.ask(channel, agent, ask)
    out = found()
    if not out:
        print(f"retry {agent} did not do it; it said: {answer[:300]!r}", flush=True)
        person.ask(channel, agent, again)
        out = found()
    return out


section("1. people sign in through Keycloak")
check("alice signs in to Gitea", alice.sign_in_gitea)
check("to Open WebUI, without signing in again", alice.sign_in_chat)
check("and to the dashboard", alice.sign_in_home)
check("bob signs in to Gitea", bob.sign_in_gitea)
check("and to Open WebUI", bob.sign_in_chat)
check("the admin signs in to Gitea", admin.sign_in_gitea)

section(f"2. alice makes the namespace {NS} in Gitea's form")
check("she fills in New Organisation", alice.create_org, NS)
check("within seconds it has its config, alice its first admin", until, 20,
      lambda: "alice" in (raw(NS, "config", "members.yaml") or ""))
def config_listed():
    alice.page.goto(f"{GIT}/{NS}")
    return alice.page.get_by_role("link", name="config").count() > 0


check("Gitea shows its repo config", until, 20, config_listed)
check("Open WebUI shows its channel in alice's sidebar", until, 20, lambda: alice.channel(NS))
check("bob, not in it, does not see it", lambda: not bob.has_channel(NS))


def channel_has(person, text):
    cid = next(c["id"] for c in person.owui("/api/v1/channels/") if c["name"] == NS)
    return next((m for m in person.owui(f"/api/v1/channels/{cid}/messages?skip=0&limit=50") if text in (m.get("content") or "")), None)


check("the operator welcomes her there, with what to ask", until, 120, channel_has, alice, f"Welcome to {NS}", every=5)
alice.channel(NS)
alice.snap("welcome in the channel")


def set_up_in_feed():
    alice.page.goto(f"{HOME}/?ns={NS}")
    return f"The namespace {NS} is set up" in alice.page.content()


check("alice's dashboard says it is set up", until, 20, set_up_in_feed)
alice.snap(f"feed: {NS} is set up")

section("3. alice sets a secret in the dashboard")
p = alice.page
p.goto(f"{HOME}/secrets?ns={NS}")
check(f"the Secrets page offers {NS}", lambda: p.locator("form.setform").count() > 0)
p.fill("form.setform input[name='name']", "api-token")
p.fill("form.setform textarea[name='value']", "tok-ui-123456")
p.locator("form.setform").get_by_role("button", name="Save").click()
check("she saves it", lambda: "Saved api-token" in p.content())
check("the page lists its name, never its value",
      lambda: p.locator("#names code", has_text="api-token").count() == 1 and "tok-ui-123456" not in p.content())
check(f"it is in {NS}'s config, encrypted", lambda: "ENC[" in (raw(NS, "config", "secrets/api-token.yaml") or ""))
alice.snap("secrets")

section(f"4. alice has the operator, in {NS}'s channel, make the todo app; alone in {NS}, she merges it")
stack_pr = check("the operator makes the repo from the template and proposes the stack", retry, alice, NS, "operator",
                 f"Make the repo {REPO} in {NS} from the template todo, then propose its stack stacks/{STACK}.yaml in {NS}/config "
                 "as its README says, with the environments main and dev. You have everything you need; do not ask back.",
                 f"Please do it now: create_repo {REPO} in {NS} with template todo if it is missing, then propose stacks/{STACK}.yaml in "
                 f"{NS}/config with propose_change: the yaml block of the README.md of {NS}/{REPO}, as it is.",
                 lambda: newest_pr(NS, "config", "alice", file=f"stacks/{STACK}.yaml"))
check("the repo has the template's todo app", lambda: HEADING in (raw(NS, REPO, "page.html") or ""))
# alice is the namespace's only person: nobody else could approve, so her own needs no other approval.
check(f"as {NS}'s only person, her own passes the approval check", until, 60, lambda: approval(NS, stack_pr or 0) == "success")


def card_needs_no_approval():
    alice.page.goto(f"{HOME}/c/{NS}/config/{stack_pr or 0}")
    return "It needs no other approval here" in alice.page.content()


check("its card on the dashboard says it needs no other approval", until, 30, card_needs_no_approval)
alice.snap("card of the stack")
check("alice merges it with the button", alice.merge, NS, "config", stack_pr or 0)
# Behind the sign-in: alice's browser is signed in to Keycloak, so it passes.
check(f"https://{STACK}.{DOMAIN} shows her todo list", until, 900, alice.page_shows, STACK, HEADING, "alice", every=10)
check("on light grey", lambda: alice.background(STACK) == GREY)
alice.snap("todo on main")

section("5. dev: a branch in Gitea, a colour by the operator, a merge into main")
check("alice makes the branch dev in Gitea", alice.create_branch, NS, REPO, "dev")
check(f"https://dev-{STACK}.{DOMAIN} comes up", until, 600, alice.page_shows, f"dev-{STACK}", HEADING, every=10)
colour_pr = check("the operator proposes a green background for dev", retry, alice, NS, "operator",
                  f"Change the background colour of the todo list of {NS}/{REPO} to #2e7d32, on its branch dev: read page.html there and "
                  "propose it with --bg: #2e7d32 instead of #f6f7f9, with base dev. Change nothing else. Do not ask back.",
                  f"Please propose it now: page.html of {NS}/{REPO} on branch dev, with --bg: #2e7d32 instead of --bg: #f6f7f9, "
                  "as a pull request with base dev.",
                  lambda: newest_pr(NS, REPO, "alice", base="dev", file="page.html"))
check("changing page.html alone", lambda: pr_files(NS, REPO, colour_pr or 0) == ["page.html"])
check("alice merges it into dev", alice.merge, NS, REPO, colour_pr or 0)
check(f"dev-{STACK} turns green", until, 900, lambda: alice.background(f"dev-{STACK}") == GREEN, every=10)
check(f"{STACK}, on main, is still grey", lambda: alice.background(STACK) == GREY)
main_pr = check("alice opens a pull request from dev to main in Gitea", alice.open_pr, NS, REPO, "dev", "main", "Green background")
check("and merges it", alice.merge, NS, REPO, main_pr or 0)
check(f"{STACK}, on main, turns green", until, 900, lambda: alice.background(STACK) == GREEN, every=10)
alice.snap("todo is green")

section("6. the operator takes a screenshot of the todo list")
shot_pr = check("it proposes the screenshot in a pull request", retry, alice, NS, "operator",
                f"Take a screenshot of https://{STACK}.{DOMAIN}/ and propose it as docs/{STACK}.png in the repo {NS}/{REPO}.",
                f"Please call screenshot now: url https://{STACK}.{DOMAIN}/, namespace {NS}, repo {REPO}, path docs/{STACK}.png.",
                lambda: newest_pr(NS, REPO, "alice", file=f"docs/{STACK}.png"))


def png_shows():
    p = alice.page
    p.goto(f"{GIT}/{NS}/{REPO}/pulls/{shot_pr}/files")
    img = p.locator(f"img[src*='{STACK}.png']").first
    img.wait_for()
    return p.evaluate("i => i.complete && i.naturalWidth > 300", img.element_handle())


check("Gitea shows the image in its files", png_shows)
alice.snap("screenshot in Gitea")
gitea(f"/repos/{NS}/{REPO}/pulls/{shot_pr or 0}", "PATCH", {"state": "closed"})

section(f"7. bob joins {NS}; main of its app then takes only approved pull requests")
member_pr = check("the operator proposes bob as a member", retry, alice, NS, "operator",
                  f"Add bob as a member of the namespace {NS}; alice stays its admin. Do not ask back.",
                  f"Please propose members.yaml in {NS}/config now: admins [alice], members [bob].",
                  lambda: newest_pr(NS, "config", "alice", file="members.yaml"))
check("the admin approves it", admin.approve, NS, "config", member_pr or 0)
check("the approval check passes", until, 60, lambda: approval(NS, member_pr or 0) == "success")
check("alice merges it", alice.merge, NS, "config", member_pr or 0)
check(f"bob sees {NS}'s channel now", until, 30, lambda: bob.has_channel(NS))
page_now = (raw(NS, REPO, "page.html") or "")
BOBS = "<h1>Our todos</h1>"
time.sleep(25)  # the members sync puts the rule on main
edited = bob.edit_file(NS, REPO, "page.html", page_now.replace(HEADING, BOBS), message="Bob's heading")
check("bob's edit straight onto main is refused", lambda: BOBS not in (raw(NS, REPO, "page.html") or ""))
bob.edit_file(NS, REPO, "page.html", page_now.replace(HEADING, BOBS), new_branch="bob-heading", message="Bob's heading")
bob_pr = check("he proposes it instead", bob.open_pr, NS, REPO, "bob-heading", "main", "Bob's heading")
check("his page offers him no merge yet", lambda: not bob.can_merge(NS, REPO, bob_pr or 0))
check("alice approves it in Gitea", alice.approve, NS, REPO, bob_pr or 0)
check("then bob merges it", bob.merge, NS, REPO, bob_pr or 0)
check(f"{STACK} shows bob's heading", until, 900, alice.page_shows, STACK, BOBS, every=10)

section(f"8. scheduled agents: alice gives the operator two routines in {NS}, in Gitea's editor")
# The operator is garrison's; the namespace's routines.yaml says what it does there
# by itself.
now = datetime.now(ZONE)
greet_at, ask_at = now + timedelta(minutes=5), now + timedelta(minutes=6)
routines = f"""- agent: operator
  name: greeting
  schedule: "{greet_at.minute} {greet_at.hour} * * *"
  owner: alice
  task: Say in one sentence that the scheduled greeting of {NS} ran. Change nothing.
- agent: operator
  name: sundays
  schedule: "{ask_at.minute} {ask_at.hour} * * *"
  owner: bob
  task: "Call ask_person with exactly this question: Should done todos be cleared every Sunday, yes or no? Then end your turn. When you have the answer, say in one sentence what bob decided."
"""
alice.edit_file(NS, "config", "routines.yaml", routines, new_branch="routines", message="Two routines", new=True)
routines_pr = check("she proposes them", alice.open_pr, NS, "config", "routines", "main", "Two routines")
check("the admin approves", admin.approve, NS, "config", routines_pr or 0)
check("the approval check passes", until, 60, lambda: approval(NS, routines_pr or 0) == "success")
check("alice merges", alice.merge, NS, "config", routines_pr or 0)
print(f"info  greeting at {greet_at:%H:%M}, sundays at {ask_at:%H:%M} ({ZONE})", flush=True)


check(f"at its time, the greeting is posted in {NS}'s channel", until, 600, channel_has, alice, "Routine greeting, for alice", every=15)
alice.channel(NS)
alice.snap("greeting in the channel")

section("9. the routine asks bob; he answers from his dashboard")
question = check("the other routine asks bob in the channel", until, 600, channel_has, bob, "Should done todos be cleared every Sunday", every=15)
check("bob signs in to the dashboard", bob.sign_in_home)


def waiting_for_bob():
    bob.page.goto(f"{HOME}/")
    return "Waiting for you" in bob.page.content() and f"{NS}/operator asks bob" in bob.page.content()


check("it waits for him there", until, 30, waiting_for_bob)
bob.snap("waiting for you")
bob.page.locator("section.waiting a.answer").first.click()
check("its link opens the question's thread", lambda: bob.page.wait_for_url(f"{CHAT}/channels/**?thread={question['id'] if question else ''}") or True)
check("he answers in its thread", bob.reply, question["id"] if question else "", "Yes, clear them on Sundays.")


def went_on():
    cid = next(c["id"] for c in bob.owui("/api/v1/channels/") if c["name"] == NS)
    for m in bob.owui(f"/api/v1/channels/{cid}/messages/{question['id']}/thread?limit=50"):
        c = m.get("content") or ""
        if c.startswith("**operator**") and "<@U:" not in c and len(c) > 30:
            return c
    return None


check("the routine goes on with his answer, in the thread", until, 300, went_on, every=5)
bob.page.goto(f"{CHAT}/channels/{channel_id(bob.page.url)}?thread={question['id'] if question else ''}")
bob.snap("the routine went on")
# The question's link is gone; changes waiting for him may still be listed.
check("his dashboard no longer lists it", lambda: (bob.page.goto(f"{HOME}/"), bob.page.locator("section.waiting a.answer").count() == 0)[1])

section(f"10. the commander, by its name, in {NS}'s channel")
# The same agent in every channel, @commander; here it works in the namespace.
said = check("bob types @commander, picks it and asks it", bob.ask, NS, "commander",
             f"Which agent should I ask to change the todo app of {NS}/{REPO}? Only tell me whom; do not do it yourself.")
check("it answers, naming the coder or the operator", lambda: any(a in (said or "").lower() for a in ("coder", "operator")))
bob.snap("asked the commander")

section("11. a scheduled job of the app: a Gitea Actions workflow on a cron schedule")
# Its own job, in the app's repo, on the namespace's runner: no agent in it.
# Gitea starts scheduled workflows of the default branch, about every two
# minutes at most; the runner's image is Alpine, so the steps run in sh.
NIGHTLY = f"""name: nightly
on:
  schedule:
    - cron: "* * * * *"
defaults:
  run:
    shell: sh
jobs:
  report:
    runs-on: garrison-user
    steps:
      - run: echo "nightly report of {NS}/{REPO}"
"""
alice.edit_file(NS, REPO, ".gitea/workflows/nightly.yaml", NIGHTLY, new_branch="nightly", message="A nightly report", new=True)
nightly_pr = check("alice adds the workflow in Gitea's editor and proposes it", alice.open_pr, NS, REPO, "nightly", "main", "A nightly report")
check("bob approves it", bob.approve, NS, REPO, nightly_pr or 0)
check("alice merges it", alice.merge, NS, REPO, nightly_pr or 0)
run_page = check("at its time, the Actions tab shows a scheduled run", until, 300, alice.scheduled_run, NS, REPO, "nightly.yaml", every=15)
check("which succeeded", lambda: "Success" in (run_page or ""))
check("its log has the job's output", lambda: f"nightly report of {NS}/{REPO}" in alice.run_log(f'echo "nightly report of {NS}/{REPO}"'))
alice.snap("scheduled run")
check("alice disables the workflow in the Actions tab", alice.disable_workflow, NS, REPO, "nightly.yaml")
ran = runs_of(NS, REPO, "nightly.yaml")
check("then it runs no more", lambda: (time.sleep(150), runs_of(NS, REPO, "nightly.yaml") == ran)[1])

done()
