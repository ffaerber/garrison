# 5. A dev environment

The stack named two environments, `main` and `dev`. Each is a branch of the
app's repo, and runs as its own copy of the app, with its own database:
`main` at `https://todo.garrison.test`, `dev` at
`https://dev-todo.garrison.test`.
Changes go to dev first. When they look right, dev is merged into main.

## The branch

alice makes the branch `dev` in Gitea's branch list. Nobody has to merge
anything for it: the stack names dev already. The image of that commit was
built for main, so `dev-todo` comes up within a minute.

![The branch dev in Gitea](img/dev-branch.png)

## A change on dev

She asks the operator for a green background, on dev only:

> @operator Change the background colour of the todo list of todo/app to
> #2e7d32, on its branch dev. Change nothing else.

![alice asks for a green background](img/dev-ask.png)

It proposes a pull request into `dev` that changes `page.html` only. She
merges it. The pipeline tests and builds the new image, and dev-todo turns
green, while todo, on main, stays light grey.

![Merged into dev](img/dev-merged.png)

## dev into main

In Gitea she compares dev with main, opens the pull request "Green
background" and merges it. todo turns green too.

![The pull request from dev to main](img/dev-to-main.png)

![Merged into main](img/dev-main-merged.png)

![todo, green](img/dev-green.png)

Next: [6. The agents at work](06-agents.md)
