# Garrison as an ontology of the organisation (design)

A proposal; its first part, `unit.md`, is built (see [status.md](status.md)). [spec.md](spec.md) is the design of what is built.

## The idea

Garrison already models an organisation: units, people and their roles, repos, apps and their environments, agents with their rights and routines, chains of command, secrets, models, snapshots. Each is a file in Git, changed by a reviewed pull request, carried out by the deployer, and recorded in the feed. That is an ontology: typed objects, links between them, governed actions that change them, and a history, which people and AI agents work on together.

What is missing is to make it explicit, and to let units work together:

1. **Units are divisions, and today islands.** People, repos, apps, agent rights and routines all stop at a unit's border; only the agents and the platform are shared. A corporate design used by every unit, or a website unit that announces another unit's releases, cannot be expressed.
2. **Agents read the organisation one tool at a time** (`list_units`, `list_stacks`, ...). They do not know where they are: asked "which apps run in my unit?" in a private chat, the commander had to ask back which unit, though the person is in one.

## unit.md: each unit, described live

Every unit has a `unit.md`: a Markdown description generated at the moment it is read, from Git and the cluster, never written by hand and never stale. As `AGENTS.md` describes a repository for coding agents, `unit.md` describes a unit for agents, people and other units.

```markdown
# todo: Our todo list

People: alice, carol (admins); bob (member).

## Apps
- todo: https://todo.example.org/, from todo/app at 4f2c9e1 (v1.4.0, released 2 days ago), healthy
  - dev: https://dev-todo.example.org/, at 9a01b7c

## Recent
- v1.4.0 of todo/app: due dates for todos (#12), dark mode (#9)
- carol merged #14 into dev, 3 hours ago

## Agents here
- coder: may change todo/app on dev, for members; routine nightly-tests
- designer: no rights here

## Uses
- the corporate design garrison/brand

## Publishes
- releases of todo/app, to the unit website
```

- **Agents get it with every turn** in the unit they work in, in their system prompt: they know its apps, versions, health and people without asking.
- **People see it** on the dashboard, a page per unit (`https://<domain>/u/<unit>`), also as Markdown (`/u/<unit>.md`).
- **Other units read what is published to them:** the website's agent reads the parts of todo's `unit.md` that todo publishes to it (a tool `read_unit`), nothing else.
- **What each reader sees** follows the rights there are: a person sees what they may read in Gitea and the feed; an agent what its unit's people may.

## Links between units

Units link to each other in Git, each side in its own config, each reviewed by that unit's admins. A link works only when both sides declare it.

| Link | Declared by | Example |
| --- | --- | --- |
| **uses** | the user of something shared | every unit uses the corporate design |
| **publishes** | the sender, in `publishes.yaml` | todo publishes releases of `app` to `website` |
| **subscribes** | the receiver, a routine with `on:` another unit's event | website runs `product-news` on a release of `todo/app` |
| **reads** | given by the sender with publishes | website's agents read todo's release notes, nothing else of todo |

### Shared: a corporate design for every unit

- The design lives once, in `garrison/brand` (or a unit of its own that owns it): design tokens (colours, fonts, spacing) as `tokens.yaml`, the logo as SVG, and a short guide to tone and style.
- A stack serves it at `https://brand.<domain>/` (`tokens.css`, the logo), so every unit's site uses the same files, not copies.
- `@designer` and `@coder` load the guide for anything visual, in every unit; the app templates start from `tokens.css`.
- When it changes, a chain `brand-update` proposes the change to each unit that uses it, a pull request for its admins, as `software-updates` does for images.

### Events: todo releases, the website announces it

1. A release: a tag or release of a unit's repo in Gitea, recorded in the feed (a new kind, `release`), with its notes and the pull requests merged since the last one.
2. `todo/config/publishes.yaml`: `- {what: releases, repo: app, to: website}`. todo's admins decide what leaves their unit.
3. `website/config/routines.yaml`: `- {agent: coder, name: product-news, on: {release: todo/app}, chain: product-news}`.
4. The chain `product-news`: the coder reads the release through `read_unit` (only what todo publishes), writes "What's new in Todo 1.4" in the brand's voice into the website's repo, and proposes it; a website admin reviews it.

## A world view: the whole organisation, and where a unit fits

unit.md describes one unit. Agents also need the whole: what the organisation is for, what each unit does, and how they relate; and a new unit must find its place in it, not just be made.

- **Each unit says what it is for**, in its own words, in `config/unit.yaml`, written by its people and changed by pull request; the approval check refuses a unit without a purpose:

  ```yaml
  purpose: The todo app our customers use to plan their week.
  serves: customers            # who it is for: customers, the team, other units
  offers: [releases of app]    # what others may use of it
  needs: [corporate design]    # what it uses of others
  related: [website, support]  # units it works with
  ```

- **The organisation says its mission once**, in `garrison/config/organisation.md`, kept by root admins: what it does, its divisions, its principles (for example "one unit per product; everything public uses the corporate design").
- **garrison.md, generated live** like unit.md, by code, never by an agent: the mission; every unit with its purpose, what it offers and needs; the links between units; what is shared (the brand, the models). Agents get a short form of it with their turns; people see it on the dashboard, the organisation at a glance.
- **A new unit is placed, not only made.** The operator's new-unit conversation asks what it is for and for whom; compares that with the other units' purposes in garrison.md and suggests the links that fit ("the website unit announces releases; shall the new app publish its releases there?"), which is reasoning over facts, where the model is good; asks whether it uses the corporate design; then makes the unit with its `unit.yaml`, and proposes each link to the other unit as a pull request its admins approve. `create_unit` takes a purpose, so no unit comes without one.
- **The map stays coherent**: a monthly routine of the operator reads garrison.md and reports in garrison's channel units without a purpose, or whose purpose no longer matches what they run; units that overlap; links that are likely missing ("todo releases often; no unit announces it"). It proposes; people decide.

## Mission command: doctrine as the backbone

Garrison's names and its command structure ([Hierarchical-AI-Agent-Command-Structure.md](Hierarchical-AI-Agent-Command-Structure.md)) come from the military already. Of military ontologies, the formal data models (NATO's JC3IEDM and MIP Information Model, the C2SIM ontology) are built for armies exchanging battlefield data, and too heavy for an organisation of software units; they are not used. Doctrine is used: *mission command* solves what the agents keep running into, independent units acting on their own without asking back, yet in one direction. Its concepts map onto Garrison:

| Doctrine | In Garrison |
| --- | --- |
| **Commander's intent**: the purpose, so that units decide rightly without asking | a unit's `purpose` (`unit.yaml`), the organisation's mission (`organisation.md`) |
| **Order** (OPORD: situation, mission, execution, constraints, command and signal) | `ask_agent`: the situation from unit.md, `task`, a new `intent` (why), `limits`, `done_when`, `mode` |
| **Situation report** (SITREP) | unit.md and garrison.md, generated as they are read |
| **Rules of engagement** | grants, and what no grant can lift |
| **Task organisation** | chains of command, grants |
| **Commander's critical information requirements** (CCIR) | per unit, what agents bring to a person at once (`config/unit.yaml`: `tell_at_once`, e.g. a failed deploy of main, a day's cost over a sum) |
| **After action review** (AAR) | after each chain run: what was meant, what happened, why, what to keep and change; it feeds the lessons agents record already |

Language models know these forms well: an order with intent, situation, mission, limits and an end state is followed more reliably than a paragraph, and most mistakes the question bank found (`test/e2e/questions.tsv`) were missing situation or missing intent. What people read stays plain ("purpose", not "commander's intent"); the structure underneath is doctrine.

## Later: the ontology made explicit

- **A schema of the object types**, one place that defines Unit, Person, Repo, App, Environment, Agent, Grant, Routine, Chain, Release with their fields and links, from which the files' checks, `unit.md` and the tools come.
- **A query over the objects** for agents ("which apps of the units bob is in failed a deploy this week?"), instead of one tool per kind.
- **Object types of a unit's own**, defined in Git (animalfacts' `Story` and `Short`, a shop's `Customer` and `Order`), with typed tools for its agents, and changes still reviewed.
- **Typed actions**: "invite a person", "restore an app", "publish a short" as defined actions with fields, who approves them, and their effect, carried out as pull requests underneath.

## Order

1. `unit.md`, generated, in every agent's turn and on the dashboard: it pays off at once (agents that know where they are). **Built**: people, apps (addresses, commit, running or not), repos with their last change, the agents' rights and routines; "Recent", "Uses" and "Publishes" come with the links between units.
2. Releases as events, `publishes.yaml`, routines on another unit's event, `read_unit`, the chain `product-news`: two units working together, end to end.
3. The corporate design: `garrison/brand`, its stack, the agents' guide, the chain `brand-update`.
4. The world view: `unit.yaml` with each unit's purpose, `organisation.md`, garrison.md generated and given to the agents; then the operator's new-unit conversation, links proposed to both units, and the coherence routine.
5. Mission command: `intent` in `ask_agent` and the chains' steps, `tell_at_once` per unit, an after action review after each chain run.
6. The schema, the query, a unit's own object types, typed actions.
