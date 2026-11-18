# 6. The agents at work

The agents are the same in every channel; in a namespace's channel they
work in that namespace. The channel's pinned message lists them: the
operator runs namespaces and apps, the coder changes code, the photographer
takes screenshots, the reviewer reviews pull requests, the monitor watches
apps, and so on. An agent acts with the rights of the person asking. What
it changes is a pull request, which only people allowed to merge can apply.

## A screenshot for the repo

alice asks the operator for a screenshot of her page, for the repo's docs:

> @operator Take a screenshot of https://web.garrison.test/ and propose it
> as docs/web.png in the repo shop/web.

![alice asks for a screenshot](img/agents-ask-screenshot.png)

The agent opens the page in a browser in its sandbox and proposes the image
as a pull request. Gitea shows it in the files changed:

![The screenshot in the pull request](img/agents-screenshot.png)

## Not sure whom to ask?

`@commander` finds the agent for what you want. It hands the work over, or
tells you whom to ask. bob asks it who changes the page of web:

![bob asks the commander](img/agents-commander.png)

Next: [7. A second person](07-members.md)
