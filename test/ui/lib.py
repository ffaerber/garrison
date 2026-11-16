"""What the UI scenarios share: a browser per person, signed in through
Keycloak as they would, the pages of Gitea, Open WebUI and the dashboard,
and checks reported as the other steps report them (ok / FAIL).

Selectors of Gitea's and Open WebUI's pages live here, one helper each, so
an upgrade of either means changing this file only.
"""

import base64
import json
import os
import re
import ssl
import sys
import time
import traceback
import urllib.error
import urllib.request

from playwright.sync_api import Page, sync_playwright

DOMAIN = os.environ["DOMAIN"]
PASSWORD = os.environ.get("PASSWORD", "12345678")  # the test people's
ADMIN_USER = os.environ["ADMIN_USER"]
ADMIN_PASSWORD = os.environ["ADMIN_PASSWORD"]
OUT = os.environ.get("OUT", "out")
GIT = f"https://git.{DOMAIN}"
CHAT = f"https://chat.{DOMAIN}"
HOME = f"https://{DOMAIN}"

os.makedirs(OUT, exist_ok=True)
failed = 0
shot = 0


def url(name: str) -> str:
    """The page of an app: https://<name>.<domain>/."""
    return f"https://{name}.{DOMAIN}/"


# ---------- checks ----------

def check(name: str, fn, *args, **kwargs):
    """Runs fn; ok if it returns something true, FAIL with why otherwise.
    Returns what fn returned, None on failure."""
    global failed
    t0 = time.time()
    try:
        out = fn(*args, **kwargs)
        if out is False or out is None:
            raise AssertionError("not so")
        print(f"ok    {name}", flush=True)
        return out
    except Exception as e:  # noqa: BLE001 - a check reports every failure
        failed += 1
        why = str(e).strip().splitlines()[0][:300] if str(e).strip() else type(e).__name__
        print(f"FAIL  {name}\n      {why}", flush=True)
        traceback.print_exc(limit=2, file=sys.stderr)
        for p in Person.all:
            p.snap("fail")
        return None
    finally:
        took = time.time() - t0
        if took >= 10:
            print(f"slow  {int(took):4d}s  {name}", flush=True)


def until(seconds: float, fn, *args, every: float = 2, **kwargs):
    """Calls fn until it returns something true, for up to seconds."""
    deadline = time.time() + seconds
    last = None
    while True:
        try:
            out = fn(*args, **kwargs)
            if out:
                return out
        except Exception as e:  # noqa: BLE001
            last = e
        if time.time() >= deadline:
            raise AssertionError(f"not within {seconds:.0f}s" + (f": {last}" if last else ""))
        time.sleep(every)


def section(title: str):
    print(f"# {title}", flush=True)


def done():
    print()
    print("all checks passed" if failed == 0 else f"{failed} check(s) failed")
    sys.exit(1 if failed else 0)


# ---------- reading Gitea, to check what the pages did ----------

_ctx = ssl.create_default_context()
_ctx.check_hostname = False
_ctx.verify_mode = ssl.CERT_NONE


def gitea(path: str, method: str = "GET", body=None):
    """Gitea's API as the platform's admin: for checks, never for actions."""
    req = urllib.request.Request(f"{GIT}/api/v1{path}", method=method,
                                 data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Authorization", "Basic " + base64.b64encode(f"{ADMIN_USER}:{ADMIN_PASSWORD}".encode()).decode())
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, context=_ctx, timeout=20) as r:
            data = r.read()
            return json.loads(data) if data[:1] in (b"{", b"[") else data.decode()
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return None
        raise


def raw(org: str, repo: str, path: str, ref: str = "main"):
    return gitea(f"/repos/{org}/{repo}/raw/{path}?ref={ref}")


def pulls(org: str, repo: str, state: str = "open"):
    return gitea(f"/repos/{org}/{repo}/pulls?state={state}&limit=50") or []


def pr_files(org: str, repo: str, n: int):
    return [f["filename"] for f in gitea(f"/repos/{org}/{repo}/pulls/{n}/files") or []]


def newest_pr(org: str, repo: str, author: str, base: str = "main", file: str | None = None, after: int = 0):
    """The newest open pull request of org/repo by author into base, changing file."""
    for p in sorted(pulls(org, repo), key=lambda p: -p["number"]):
        if p["number"] <= after or p["user"]["login"] != author or p["base"]["ref"] != base:
            continue
        if file is None or file in pr_files(org, repo, p["number"]):
            return p["number"]
    return None


def approval(org: str, n: int) -> str:
    """The garrison/approval status of a config pull request: success, pending, failure or ""."""
    sha = gitea(f"/repos/{org}/config/pulls/{n}")["head"]["sha"]
    for s in (gitea(f"/repos/{org}/config/commits/{sha}/status") or {}).get("statuses") or []:
        if s["context"] == "garrison/approval":
            return s["status"]
    return ""


# ---------- people ----------

class Person:
    """One person's browser, signed in to Keycloak once for every app."""

    all: list["Person"] = []

    def __init__(self, browser, name: str, password: str = PASSWORD):
        self.name, self.password = name, password
        self.ctx = browser.new_context(ignore_https_errors=True, viewport={"width": 1400, "height": 900})
        self.ctx.set_default_timeout(30_000)
        self.page: Page = self.ctx.new_page()
        Person.all.append(self)

    def snap(self, what: str):
        """A screenshot of what the person sees now, into out/."""
        global shot
        shot += 1
        try:
            self.page.screenshot(path=f"{OUT}/{shot:03d}-{self.name}-{re.sub(r'[^a-z0-9]+', '-', what.lower())[:50]}.png")
        except Exception:  # noqa: BLE001 - a screenshot is no reason to fail
            pass

    # Keycloak's sign-in form, wherever an app sends the person to it.
    def keycloak(self):
        p = self.page
        p.wait_for_selector("#username")
        p.fill("#username", self.name)
        p.fill("#password", self.password)
        p.click("#kc-login")

    def sign_in_gitea(self):
        p = self.page
        p.goto(f"{GIT}/user/oauth2/keycloak")
        if p.locator("#username").count():
            self.keycloak()
        p.wait_for_url(f"{GIT}/**")
        p.goto(f"{GIT}/")
        return p.locator(f"img.avatar[alt='{self.name}'], .user-menu, [aria-label='Profile and Settings…']").count() > 0 or \
            self.name in p.content()

    def sign_in_chat(self):
        p = self.page
        p.goto(f"{CHAT}/auth")
        p.get_by_text("Continue with Keycloak").click()
        if p.locator("#username").count():
            self.keycloak()
        p.wait_for_url(re.compile(rf"^{re.escape(CHAT)}/(?!auth)"))
        self.dismiss()
        return True

    def sign_in_home(self):
        p = self.page
        p.goto(f"{HOME}/")
        if p.locator("#username").count():
            self.keycloak()
        p.wait_for_selector(".who")
        return self.name in p.locator(".who").inner_text()

    # ---------- Open WebUI ----------

    def dismiss(self):
        """Closes Open WebUI's release notes, shown at a first sign-in."""
        p = self.page
        p.wait_for_timeout(1500)
        ok = p.get_by_text("Okay, Let's Go!")
        if ok.count():
            ok.click()

    def channel(self, name: str):
        """Opens a channel from the sidebar."""
        p = self.page
        p.goto(f"{CHAT}/")
        self.dismiss()
        if p.locator("button[aria-label='Open Sidebar']").first.is_visible():
            p.locator("button[aria-label='Open Sidebar']").first.click()
        link = p.locator("a[href*='/channels/']").filter(has_text=re.compile(rf"^\s*{re.escape(name)}\s*$"))
        # The sidebar renders after the page; its Channels section may be
        # closed, and opens on a click (a click on an open one closes it).
        try:
            link.first.wait_for(timeout=5_000)
        except Exception:  # noqa: BLE001 - closed
            p.get_by_text("Channels", exact=True).click()
        link.first.click()
        p.wait_for_url(f"{CHAT}/channels/**")
        p.wait_for_selector("#chat-input")
        return p.url.rsplit("/", 1)[1]

    def has_channel(self, name: str) -> bool:
        return any(c["name"] == name for c in self.owui("/api/v1/channels/"))

    def owui(self, path: str):
        """Open WebUI's API as the page itself calls it, with the person's
        session, which lives in Open WebUI's pages only."""
        if not self.page.url.startswith(CHAT):
            self.page.goto(f"{CHAT}/")
            self.page.wait_for_function("() => !!localStorage.token")
        return self.page.evaluate(
            "path => fetch(path, {headers: {Authorization: 'Bearer ' + localStorage.token}}).then(r => r.json())", path)

    def say(self, agent: str, text: str) -> str:
        """In the open channel: types @agent, picks it from the list, types
        text and sends it. Returns the message's id."""
        p = self.page
        cid = p.url.rsplit("/", 1)[1]
        before = {m["id"] for m in self.owui(f"/api/v1/channels/{cid}/messages?skip=0&limit=50")}
        option = p.locator("button.selected-command-option-button, button[data-selected='true']").filter(has_text=agent)
        # The page knows the agents there were when it loaded; one of a
        # namespace made since appears after a reload, as for a person.
        for attempt in range(3):
            ed = p.locator("#chat-input")
            ed.click()
            p.keyboard.press("Control+A")
            p.keyboard.press("Delete")
            p.keyboard.type("@" + agent)
            try:
                option.first.wait_for(timeout=10_000)
                break
            except Exception:  # noqa: BLE001 - not offered (yet)
                if attempt == 2:
                    raise
                p.reload()
                p.locator("#chat-input").wait_for()
        option.first.click()
        p.keyboard.type(" " + text)
        p.keyboard.press("Enter")
        mine = until(30, lambda: [m for m in self.owui(f"/api/v1/channels/{cid}/messages?skip=0&limit=50")
                                  if m["id"] not in before and agent in (m.get("content") or "")])
        self.snap(f"asked {agent}")
        return mine[0]["id"]

    def answer(self, message: str, agent: str, seconds: float = 600) -> str:
        """The agent's answer in the thread of message, once finished; opens
        the thread on the page."""
        p = self.page
        cid = p.url.rsplit("/", 1)[1]

        def finished():
            for m in self.owui(f"/api/v1/channels/{cid}/messages/{message}/thread?limit=50"):
                meta = m.get("meta") or {}
                if meta.get("model_id") == agent and meta.get("done"):
                    return m.get("content") or " "
            return None
        text = until(seconds, finished, every=5)
        p.goto(f"{CHAT}/channels/{cid}/messages/{message}")
        p.wait_for_timeout(2000)
        self.snap(f"answer of {agent}")
        return text

    def ask(self, channel: str, agent: str, text: str, seconds: float = 600) -> str:
        self.channel(channel)
        return self.answer(self.say(agent, text), agent, seconds)

    def reply(self, message: str, text: str):
        """Replies in the thread of message of the open channel."""
        p = self.page
        cid = p.url.split("/channels/", 1)[1].split("/", 1)[0]
        p.goto(f"{CHAT}/channels/{cid}/messages/{message}")
        box = p.locator("#chat-input").last
        box.wait_for()
        box.click()
        p.keyboard.type(text)
        p.keyboard.press("Enter")
        until(20, lambda: any(text in (m.get("content") or "")
                              for m in self.owui(f"/api/v1/channels/{cid}/messages/{message}/thread?limit=50")))
        self.snap("replied")
        return True

    # ---------- Gitea ----------

    def gitea_form(self, path: str):
        self.page.goto(f"{GIT}{path}")

    def create_org(self, name: str):
        p = self.page
        p.goto(f"{GIT}/org/create")
        p.fill("#org_name", name)
        p.locator("input[name='visibility'][value='1']").check(force=True)  # limited: everyone signed in
        p.get_by_role("button", name="Create Organization").click()
        # Gitea goes to the organisation's page or its dashboard.
        p.wait_for_url(re.compile(rf"^{re.escape(GIT)}/(org/)?{re.escape(name)}(/|$)"))
        self.snap(f"created {name}")
        return True

    def create_branch(self, org: str, repo: str, branch: str, base: str = "main"):
        """Gitea's branch list: Create branch, from base."""
        p = self.page
        p.goto(f"{GIT}/{org}/{repo}/branches")
        # Each branch's row has its "create a branch from here" button.
        row = p.locator("table tr").filter(has_text=re.compile(rf"^\s*{re.escape(base)}\s")).first
        (row.locator(".show-create-branch-modal") if row.count() else p.locator(".show-create-branch-modal")).first.click()
        p.locator("input#new_branch_name:visible").fill(branch)
        p.get_by_role("button", name="Confirm").click()
        until(20, lambda: gitea(f"/repos/{org}/{repo}/branches/{branch}"))
        self.snap(f"branch {branch}")
        return True

    def approve(self, org: str, repo: str, n: int):
        """Reviews a pull request in Gitea: Files changed, Review, Approve."""
        p = self.page
        p.goto(f"{GIT}/{org}/{repo}/pulls/{n}/files")
        p.locator(".js-btn-review").first.click()
        p.locator("textarea[name='content']:visible").first.fill("Looks right.")
        p.locator("button[name='type'][value='approve']:visible").click()
        until(20, lambda: any(r["state"] == "APPROVED" and r["user"]["login"] == self.name
                              for r in gitea(f"/repos/{org}/{repo}/pulls/{n}/reviews") or []))
        self.snap(f"approved {org}/{repo}#{n}")
        return True

    def can_merge(self, org: str, repo: str, n: int) -> bool:
        """Whether the pull request's page lets this person merge now. An
        owner is offered to merge "when checks succeed", Gitea's auto-merge:
        that is no merge now."""
        p = self.page
        p.goto(f"{GIT}/{org}/{repo}/pulls/{n}")
        p.wait_for_timeout(1500)
        b = p.get_by_role("button", name=re.compile(r"^\s*Create merge commit\s*$"))
        return b.count() > 0 and b.first.is_enabled() and "enough required approvals" not in p.content()

    def merge(self, org: str, repo: str, n: int):
        """Merges a pull request with its page's button, and confirms."""
        p = self.page
        p.goto(f"{GIT}/{org}/{repo}/pulls/{n}")
        p.get_by_role("button", name="Create merge commit").first.click()
        p.locator("button[name='do'][value='merge']:visible").click()
        until(60, lambda: (gitea(f"/repos/{org}/{repo}/pulls/{n}") or {}).get("merged"))
        self.snap(f"merged {org}/{repo}#{n}")
        return True

    def open_pr(self, org: str, repo: str, head: str, base: str, title: str) -> int:
        """Compares head with base in Gitea and opens the pull request."""
        p = self.page
        def opened():
            return next((x["number"] for x in pulls(org, repo) if x["head"]["ref"] == head and x["base"]["ref"] == base), None)
        for _ in range(2):  # a request the page drops halfway is tried again
            p.goto(f"{GIT}/{org}/{repo}/compare/{base}...{head}")
            p.get_by_role("button", name="New Pull Request").click()
            p.fill("form#new-issue #issue_title", title)
            p.locator("form#new-issue").get_by_role("button", name="Create Pull Request").click()
            try:
                p.wait_for_url(re.compile(rf"/{org}/{repo}/pulls/\d+"))
                break
            except Exception:  # noqa: BLE001
                if opened():
                    break
        self.snap(f"opened {title}")
        return until(10, opened)

    def edit_file(self, org: str, repo: str, path: str, content: str, branch: str = "main",
                  new_branch: str | None = None, message: str = "Change it") -> str:
        """Gitea's web editor: replaces the file's content, then commits to
        branch, or to new_branch and on to the new pull request's page.
        Returns where the page ends up."""
        p = self.page
        p.goto(f"{GIT}/{org}/{repo}/_edit/{branch}/{path}")
        editor = p.locator(".monaco-editor textarea").first
        editor.wait_for()
        editor.focus()
        p.keyboard.press("Control+A")
        p.keyboard.press("Delete")
        # Pasted, as a person would: typed, the editor would indent each
        # new line after the last.
        editor.evaluate("""(t, text) => { const d = new DataTransfer(); d.setData('text/plain', text);
            t.dispatchEvent(new ClipboardEvent('paste', {clipboardData: d, bubbles: true, cancelable: true})); }""", content)
        p.wait_for_timeout(500)
        p.fill("input[name='commit_summary']", message)
        if new_branch:
            p.locator("input[name='commit_choice'][value='commit-to-new-branch']").check(force=True)
            p.fill("input[name='new_branch_name']", new_branch)
        else:
            p.locator("input[name='commit_choice'][value='direct']").check(force=True)
        p.locator("#commit-button").click()
        p.wait_for_timeout(3000)
        self.snap(f"edited {path}")
        return p.url

    def page_shows(self, name: str, *texts: str) -> bool:
        p = self.page
        p.goto(url(name))
        html = p.content()
        return all(t in html for t in texts)

    def background(self, name: str) -> str:
        """The computed background colour of an app's page, e.g. rgb(46, 125, 50)."""
        p = self.page
        p.goto(url(name))
        return p.evaluate("() => getComputedStyle(document.body).backgroundColor")


def browser():
    """Chromium, with every name of the installation on this machine."""
    pw = sync_playwright().start()
    return pw.chromium.launch(args=[f"--host-resolver-rules=MAP *.{DOMAIN} 127.0.0.1, MAP {DOMAIN} 127.0.0.1"])
