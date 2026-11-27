"""Invited with a link, as each person sees it in the browser. carol, an
admin of todo, makes a link on the dashboard's People page and sends it to
frank, who has no account: he opens it, chooses his username and password,
and signs in, sent to Gitea once on his first visit. Accepting proposes him
in todo's members.yaml in carol's name: alice approves it in Gitea, carol
merges it, and frank is a member of todo. Later bob forgets his password:
alice makes him a link, and he sets a new one with it and signs in. See
docs/scenarios.md, "Invited with a link". Run by test/ui/run.sh invites.
"""

from lib import HOME, Person, browser, check, done, gitea, newest_pr, section, until

NS = "todo"
b = browser()
alice, carol = Person(b, "alice"), Person(b, "carol")
# Fresh browsers, as someone opening a link they were sent.
frank = Person(b, "frank", password="frank-pass-0123")
bob = Person(b, "bob", password="bob-new-pass-0123")


def team_members(team: str) -> list:
    t = next((t for t in gitea(f"/orgs/{NS}/teams") or [] if t["name"] == team), None)
    return [u["login"] for u in gitea(f"/teams/{t['id']}/members") or []] if t else []


section("1. carol invites frank into todo")
check("carol signs in to the dashboard", carol.sign_in_home)
link = check("she makes a link for a member of todo on the People page", carol.invite, NS, "member", "Frank")
check("it is a link to /join", lambda: link and link.startswith(f"{HOME}/join?token="))
def open_row() -> str:
    """The open link's row on carol's People page, its words as one line."""
    carol.page.goto(f"{HOME}/people")
    row = carol.page.locator("li", has_text="Frank")
    return " ".join(row.first.inner_text().split()) if row.count() else ""


check("the People page lists it as open", lambda: "member of todo" in open_row())
check("without the link itself", lambda: link and link.split("token=")[1] not in carol.page.content())

section("2. frank opens the link")
heading = check("frank makes his account with it", frank.join, link or "", "frank")
check("he is welcome", lambda: heading == "Welcome, frank")
check("the page says carol proposes him in todo", lambda: "carol proposes you as member of todo" in frank.page.content())
check("the link works once", lambda: not Person(b, "stranger").link_works(link or ""))
check("he signs in: the dashboard has him open Gitea once, then it is his", frank.first_visit)

section("3. todo's rules hold: another admin approves")
pr = check("accepting proposed frank in members.yaml, in carol's name",
           lambda: until(60, lambda: newest_pr(NS, "config", "carol", file="members.yaml")))
check("alice signs in to Gitea", alice.sign_in_gitea)
check("alice approves it", alice.approve, NS, "config", pr)
check("carol signs in to Gitea", carol.sign_in_gitea)
check("carol merges it", carol.merge, NS, "config", pr)
check("frank is a member of todo", lambda: until(90, lambda: "frank" in team_members("members")))

section("4. bob forgot his password")
check("alice signs in to the dashboard", alice.sign_in_home)
reset = check("alice makes bob a password link", alice.password_link, "bob")
heading = check("bob sets a new password with it", bob.join, reset or "")
check("it is set", lambda: heading == "Your new password is set")
check("bob signs in with the new one", bob.sign_in_home)

section("5. a link alice takes back")
guest = check("alice invites a guest without a unit", alice.invite, "", "member", "guest")
check("she revokes it", alice.revoke, "guest")
check("the revoked link opens nothing", lambda: not Person(b, "guest").link_works(guest or ""))

done()
