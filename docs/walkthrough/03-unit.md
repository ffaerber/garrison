# 3. A unit

A unit is a group of apps, with its own people, its own chat channel
and its own settings. In Gitea it is an organisation. alice makes one
called `todo`, for the team's todo app.

## Making it

alice can make it in Gitea's New Organisation form, or ask in the chat
(`@operator make a unit todo`). Here she uses the form:

![New Organisation in Gitea](img/unit-form.png)

Within seconds, Garrison sets it up:

- the repo `todo/config`, which holds everything about the unit: who
  belongs to it (`members.yaml`), its apps (`stacks/`), what the agents may
  do (`grants.yaml`) and do by themselves (`routines.yaml`), and its
  secrets, encrypted;
- alice as its first admin;
- a channel `todo` in the chat, which only todo's people see.

![todo in Gitea, with its config](img/unit-created.png)

## Its channel

In the new channel the operator welcomes alice. It gives examples of what to
ask, and says where its proposals wait for her:

![The operator's welcome in todo's channel](img/unit-welcome.png)

Her dashboard says the unit is set up:

![The dashboard's feed](img/unit-feed.png)

## A secret

Apps often need a password or a token. alice saves `api-token` on the
dashboard's Secrets page. The page lists its name and never shows its value
again. In `todo/config` it is stored encrypted, so it can be in Git; only
the machine can read it.

![The Secrets page](img/secrets.png)

Next: [4. An app and its stack](04-app-and-stack.md)
