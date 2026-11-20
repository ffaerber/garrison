# 7. A second person

Who belongs to a unit is in `members.yaml` of its config: its admins
and its members. A change to it is a pull request like any other. alice asks
the operator to add bob.

## Adding bob

> @operator Add bob as a member of the unit todo; alice stays its admin.

![alice asks the operator to add bob](img/members-ask.png)

alice is still todo's only person, so her own change needs nobody else's
approval; here a root admin approves it too. Then alice merges it, and
within a minute bob sees todo's channel.

![The admin approves](img/members-approved.png)

## Now main takes only approved changes

While alice was todo's only person, she could change main freely. With two
people, main of todo's repos takes only pull requests that someone other
than their author approved.

bob changes the heading in `page.html` straight on main in Gitea's editor, and Gitea refuses
it:

![bob's edit onto main is refused](img/members-refused.png)

He commits it to a new branch instead and opens a pull request. Its page
offers him no merge until alice approves it:

![bob's pull request](img/members-proposed.png)

![alice approves it](img/members-approve.png)

Then bob merges it, and todo shows his heading.

![bob merges it](img/members-merged.png)

Next: [8. Routines: agents on a schedule](08-routines.md)
