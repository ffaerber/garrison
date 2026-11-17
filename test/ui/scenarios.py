"""The UI scenarios: everything a person does, clicked and typed in Gitea,
Open WebUI and the dashboard, from a new namespace to a deployed web app, a
screenshot, a scheduled job and a question a routine asks. See
docs/scenarios.md, "In the browser". Run by test/ui/run.sh.
"""

import os
import time
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

from lib import (CHAT, channel_id, DOMAIN, GIT, HOME, Person, approval, browser, check, done, gitea, newest_pr, pr_files,
                 raw, runs_of, section, until)

NS, APP = os.environ.get("UI_NAMESPACE", "shop"), os.environ.get("UI_APP", "web")
GREEN = "rgb(46, 125, 50)"  # #2e7d32
WHITE = "rgb(255, 255, 255)"
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

section("2. alice makes the namespace shop in Gitea's form")
check("she fills in New Organisation", alice.create_org, NS)
check("within seconds it has its config, alice its first admin", until, 20,
      lambda: "alice" in (raw(NS, "config", "members.yaml") or ""))
def config_listed():
    alice.page.goto(f"{GIT}/{NS}")
    return alice.page.get_by_role("link", name="config").count() > 0


check("Gitea shows its repo config", until, 20, config_listed)
check("Open WebUI shows its channel in alice's sidebar", until, 20, lambda: alice.channel(NS))
check("bob, not in it, does not see it", lambda: not bob.has_channel(NS))


def set_up_in_feed():
    alice.page.goto(f"{HOME}/?ns={NS}")
    return f"The namespace {NS} is set up" in alice.page.content()


check("alice's dashboard says it is set up", until, 20, set_up_in_feed)
alice.snap("feed: shop is set up")

section("3. alice sets a secret in the dashboard")
p = alice.page
p.goto(f"{HOME}/secrets?ns={NS}")
check("the Secrets page offers shop", lambda: p.locator("form.setform").count() > 0)
p.fill("form.setform input[name='name']", "api-token")
p.fill("form.setform textarea[name='value']", "tok-ui-123456")
p.locator("form.setform").get_by_role("button", name="Save").click()
check("she saves it", lambda: "Saved api-token" in p.content())
check("the page lists its name, never its value",
      lambda: p.locator("#names code", has_text="api-token").count() == 1 and "tok-ui-123456" not in p.content())
check("it is in shop's config, encrypted", lambda: "ENC[" in (raw(NS, "config", "secrets/api-token.yaml") or ""))
alice.snap("secrets")

section("4. alice has the operator, in shop's channel, make the web app; the admin approves; alice merges")
stack_pr = check("the operator makes the repo from the template and proposes the stack", retry, alice, NS, "operator",
                 f"Make the repo {APP} in {NS} from the template static-web, then propose its stack stacks/{APP}.yaml in {NS}/config "
                 "as its README says, with the environments main and dev, both public (no private). You have everything you need; do not ask back.",
                 f"Please do it now: create_repo {APP} in {NS} with template static-web if it is missing, then propose stacks/{APP}.yaml in "
                 f"{NS}/config with propose_change: source {APP}, entrypoint web, service web with image \"{{{{registry}}}}/{NS}/{APP}:{{{{commit}}}}\" "
                 "and port 8080, environments main: {} and dev: {}.",
                 lambda: newest_pr(NS, "config", "alice", file=f"stacks/{APP}.yaml"))
check("the repo has the template's page", lambda: f"<h1>{APP}</h1>" in (raw(NS, APP, "index.html") or ""))
check("the approval check waits for someone else", until, 60, lambda: approval(NS, stack_pr or 0) == "pending")
check("alice's page offers her no merge yet", lambda: not alice.can_merge(NS, "config", stack_pr or 0))
check("the admin approves it in Gitea", admin.approve, NS, "config", stack_pr or 0)
check("the approval check passes", until, 60, lambda: approval(NS, stack_pr or 0) == "success")
check("alice merges it with the button", alice.merge, NS, "config", stack_pr or 0)
check(f"https://{APP}.{DOMAIN} shows the page", until, 900, alice.page_shows, APP, f"<h1>{APP}</h1>", every=10)
check("on white", lambda: alice.background(APP) == WHITE)
alice.snap("web on main")

section("5. dev: a branch in Gitea, a colour by the operator, a merge into main")
check("alice makes the branch dev in Gitea", alice.create_branch, NS, APP, "dev")
check(f"https://dev-{APP}.{DOMAIN} comes up", until, 600, alice.page_shows, f"dev-{APP}", f"<h1>{APP}</h1>", every=10)
colour_pr = check("the operator proposes a green background for dev", retry, alice, NS, "operator",
                  f"Change the background colour of the page of {NS}/{APP} to #2e7d32, on its branch dev: read index.html there and "
                  "propose it with the new colour, with base dev. Change nothing else. Do not ask back.",
                  f"Please propose it now: index.html of {NS}/{APP} on branch dev, with background-color: #2e7d32 instead of #ffffff, "
                  "as a pull request with base dev.",
                  lambda: newest_pr(NS, APP, "alice", base="dev", file="index.html"))
check("changing index.html alone", lambda: pr_files(NS, APP, colour_pr or 0) == ["index.html"])
check("alice merges it into dev", alice.merge, NS, APP, colour_pr or 0)
check(f"dev-{APP} turns green", until, 900, lambda: alice.background(f"dev-{APP}") == GREEN, every=10)
check(f"{APP}, on main, is still white", lambda: alice.background(APP) == WHITE)
main_pr = check("alice opens a pull request from dev to main in Gitea", alice.open_pr, NS, APP, "dev", "main", "Green background")
check("and merges it", alice.merge, NS, APP, main_pr or 0)
check(f"{APP}, on main, turns green", until, 900, lambda: alice.background(APP) == GREEN, every=10)
alice.snap("web is green")

section("6. the operator takes a screenshot of the page")
shot_pr = check("it proposes the screenshot in a pull request", retry, alice, NS, "operator",
                f"Take a screenshot of https://{APP}.{DOMAIN}/ and propose it as docs/{APP}.png in the repo {NS}/{APP}.",
                f"Please call screenshot now: url https://{APP}.{DOMAIN}/, namespace {NS}, repo {APP}, path docs/{APP}.png.",
                lambda: newest_pr(NS, APP, "alice", file=f"docs/{APP}.png"))


def png_shows():
    p = alice.page
    p.goto(f"{GIT}/{NS}/{APP}/pulls/{shot_pr}/files")
    img = p.locator(f"img[src*='{APP}.png']").first
    img.wait_for()
    return p.evaluate("i => i.complete && i.naturalWidth > 300", img.element_handle())


check("Gitea shows the image in its files", png_shows)
alice.snap("screenshot in Gitea")
gitea(f"/repos/{NS}/{APP}/pulls/{shot_pr or 0}", "PATCH", {"state": "closed"})

section("7. bob joins shop; main of its app then takes only approved pull requests")
member_pr = check("the operator proposes bob as a member", retry, alice, NS, "operator",
                  f"Add bob as a member of the namespace {NS}; alice stays its admin. Do not ask back.",
                  f"Please propose members.yaml in {NS}/config now: admins [alice], members [bob].",
                  lambda: newest_pr(NS, "config", "alice", file="members.yaml"))
check("the admin approves it", admin.approve, NS, "config", member_pr or 0)
check("the approval check passes", until, 60, lambda: approval(NS, member_pr or 0) == "success")
check("alice merges it", alice.merge, NS, "config", member_pr or 0)
check("bob sees shop's channel now", until, 30, lambda: bob.has_channel(NS))
page_now = (raw(NS, APP, "index.html") or "")
time.sleep(25)  # the members sync puts the rule on main
edited = bob.edit_file(NS, APP, "index.html", page_now.replace(f"<h1>{APP}</h1>", "<h1>bob's web</h1>"), message="Bob's heading")
check("bob's edit straight onto main is refused", lambda: "bob's web" not in (raw(NS, APP, "index.html") or ""))
bob.edit_file(NS, APP, "index.html", page_now.replace(f"<h1>{APP}</h1>", "<h1>bob's web</h1>"), new_branch="bob-heading", message="Bob's heading")
bob_pr = check("he proposes it instead", bob.open_pr, NS, APP, "bob-heading", "main", "Bob's heading")
check("his page offers him no merge yet", lambda: not bob.can_merge(NS, APP, bob_pr or 0))
check("alice approves it in Gitea", alice.approve, NS, APP, bob_pr or 0)
check("then bob merges it", bob.merge, NS, APP, bob_pr or 0)
check("web shows bob's heading", until, 900, alice.page_shows, APP, "bob's web", every=10)

section("8. scheduled agents: alice gives the operator two routines in shop, in Gitea's editor")
# The operator is garrison's; shop's routines.yaml says what it does in shop
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
  task: "Call ask_person with exactly this question: Should the shop open on Sundays, yes or no? Then end your turn. When you have the answer, say in one sentence what bob decided."
"""
alice.edit_file(NS, "config", "routines.yaml", routines, new_branch="routines", message="Two routines", new=True)
routines_pr = check("she proposes them", alice.open_pr, NS, "config", "routines", "main", "Two routines")
check("the admin approves", admin.approve, NS, "config", routines_pr or 0)
check("the approval check passes", until, 60, lambda: approval(NS, routines_pr or 0) == "success")
check("alice merges", alice.merge, NS, "config", routines_pr or 0)
print(f"info  greeting at {greet_at:%H:%M}, sundays at {ask_at:%H:%M} ({ZONE})", flush=True)


def channel_has(person, text):
    cid = next(c["id"] for c in person.owui("/api/v1/channels/") if c["name"] == NS)
    return next((m for m in person.owui(f"/api/v1/channels/{cid}/messages?skip=0&limit=50") if text in (m.get("content") or "")), None)


check("at its time, the greeting is posted in shop's channel", until, 600, channel_has, alice, "Routine greeting, for alice", every=15)
alice.channel(NS)
alice.snap("greeting in the channel")

section("9. the routine asks bob; he answers from his dashboard")
question = check("the other routine asks bob in the channel", until, 600, channel_has, bob, "Should the shop open on Sundays", every=15)
check("bob signs in to the dashboard", bob.sign_in_home)


def waiting_for_bob():
    bob.page.goto(f"{HOME}/")
    return "Waiting for you" in bob.page.content() and f"{NS}/operator asks bob" in bob.page.content()


check("it waits for him there", until, 30, waiting_for_bob)
bob.snap("waiting for you")
bob.page.locator("section.waiting a.answer").first.click()
check("its link opens the question's thread", lambda: bob.page.wait_for_url(f"{CHAT}/channels/**?thread={question['id'] if question else ''}") or True)
check("he answers in its thread", bob.reply, question["id"] if question else "", "Yes, open on Sundays.")


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
check("his dashboard no longer lists it", lambda: (bob.page.goto(f"{HOME}/"), "Waiting for you" not in bob.page.content())[1])

section("10. the commander, by its name, in shop's channel")
# The same agent in every channel, @commander; here it works in shop.
said = check("bob types @commander, picks it and asks it", bob.ask, NS, "commander",
             f"Which agent should I ask to change the page of {NS}/{APP}? Only tell me whom; do not do it yourself.")
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
      - run: echo "nightly report of {NS}/{APP}"
"""
alice.edit_file(NS, APP, ".gitea/workflows/nightly.yaml", NIGHTLY, new_branch="nightly", message="A nightly report", new=True)
nightly_pr = check("alice adds the workflow in Gitea's editor and proposes it", alice.open_pr, NS, APP, "nightly", "main", "A nightly report")
check("bob approves it", bob.approve, NS, APP, nightly_pr or 0)
check("alice merges it", alice.merge, NS, APP, nightly_pr or 0)
run_page = check("at its time, the Actions tab shows a scheduled run", until, 300, alice.scheduled_run, NS, APP, "nightly.yaml", every=15)
check("which succeeded", lambda: "Success" in (run_page or ""))
check("its log has the job's output", lambda: f"nightly report of {NS}/{APP}" in alice.run_log(f'echo "nightly report of {NS}/{APP}"'))
alice.snap("scheduled run")
check("alice disables the workflow in the Actions tab", alice.disable_workflow, NS, APP, "nightly.yaml")
ran = runs_of(NS, APP, "nightly.yaml")
check("then it runs no more", lambda: (time.sleep(150), runs_of(NS, APP, "nightly.yaml") == ran)[1])

done()
