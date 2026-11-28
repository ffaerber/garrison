# Garrison

**Your own organisation, run by people and AI agents together, on your own machines.**

A household, a club, a small company: each runs on a handful of apps, a few websites, some shared files, and a lot of small chores around them. Updates, backups, "why is it down?", "can you change this?". Today that work lands on whoever knows how, or on cloud services that own your data.

Garrison is a different deal. You bring a machine. Garrison turns it into the home base of your organisation, with a team of AI agents who do the work alongside your people, and with every decision staying in human hands.

## The idea

**You talk; the work gets done; you decide.**
You ask in a chat, the way you would ask a colleague: *"put up a page for our summer fair"*, *"why is the todo app slow?"*, *"make the background green"*. An agent looks into it, does the work, and comes back with a proposal. Nothing happens until a person says yes. Agents never get more power than the person who asked them, unless your people have agreed to it beforehand.

**An organisation, not a pile of servers.**
Garrison is organised the way people are: in **units**, like divisions of a company or groups of a club. Each unit has its people, its apps, its chat and its own say over what happens inside it. A unit for the family's photos, one for the shop, one for the website. Each works on its own. Yet they belong to one whole: a unit can let others follow what it does, so the website can announce the shop's new release without anyone copying it over.

**A team that knows where it is.**
The agents are your staff, each with a craft. The **commander** listens first and finds who can help. The **operator** runs the place. The **coder** builds and fixes. The **sentry** keeps watch. The **scout** looks things up in the world. The **designer** makes pictures, videos and sound. They always know the current state of each unit: who is in it, what runs, what changed. So you ask once, and they don't send you elsewhere.

**Purpose over instructions.**
Garrison borrows an old idea from how good teams are led: say *why*, not only *what*. Where it is heading, each unit says what it is for, and each task carries its intent and its limits. Then agents can act sensibly on their own, in one direction, without asking about every step, and still stop to ask when it truly matters. Today, agents already hand each other tasks with clear limits and a picture of what done looks like.

**Nothing happens in the dark.**
Every change is written down before it happens, as a proposal anyone in the unit can read, with who asked, who approved and what it did. A feed shows the whole story as it unfolds. And everything can be undone: your data is snapshotted every hour and before every change, and bringing it back is one more proposal.

**Yours, at home.**
Your apps, your data, your conversations stay on your machines. You choose which AI models think for you: one running in your own house, or one you rent, and you can change at any time. When one machine is not enough, add another; apps and their data move between them on request.

## What it feels like

- Alice writes *"@commander I want a dark mode for our todo app."* The commander hands her to the coder in the todo unit's chat. The coder asks two questions, makes the change, and sends a link. Carol, an admin of the unit, approves it. A minute later the app is dark.
- Bob asks *"Is everything running?"* and gets an answer for every unit he is in, from what each one looks like right now.
- At night, the sentry checks for software updates, the scout reads what changed in each new version, and the coder prepares the updates. In the morning they wait, one click each, for a person to approve.
- The shop publishes a new release. The website unit's agent writes the news item from its release notes and proposes it to the website's people.
- Someone deletes a week of entries by mistake. An admin picks yesterday's snapshot, a colleague approves, and the data is back.

## Where it is going

Garrison is young and built in the open. It works today on one machine or a few, with apps, chat, agents, routines, snapshots, moving apps between machines, and monitoring. What comes next:
- units that share more with each other, such as one corporate design for every unit's websites;
- a clear picture of the whole organisation, so a new unit finds its place and its purpose from the start;
- backups to a second disk and off-site;
- keeping repositories in step with GitHub.

## Try it

On a fresh Ubuntu machine (26.04), as root:

```
sudo ./install.sh --answers my.env
```

The installer ends with a link to finish the setup in the browser. There you choose the first admin, the AI providers the agents think with, and the model they use. Then invite your people.

## Learn more

- [docs/walkthrough](docs/walkthrough/README.md): how it is used, step by step, with screenshots.
- [docs/spec.md](docs/spec.md): how it works, and the design. [docs/status.md](docs/status.md): what is built.
- [docs/agents.md](docs/agents.md): the agents, what they may do, their routines and chains.
- [docs/ontology.md](docs/ontology.md): Garrison as a model of the organisation: units, purposes, links, mission command.
- [docs/storage.md](docs/storage.md): disks, copies, backups and moving data.
- [docs/scenarios.md](docs/scenarios.md): what people get done by talking to the agents, as the tests play it.
- [AGENTS.md](AGENTS.md): a guide to this repository for people and coding agents working on it.
