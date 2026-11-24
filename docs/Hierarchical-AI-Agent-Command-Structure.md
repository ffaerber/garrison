# Hierarchical AI Agent Command Structure

**Status:** proposal. It says where Garrison's agents are going: a human-led organisation of agents that runs a chain of command without a person in every step, and stays accountable to the people who set it up. Each part names what is built and what is design. The agents as they are: [agents.md](agents.md); rights: Rights and autonomy in [spec.md](spec.md).

## The idea

People set the goals and the bounds; agents work within them on their own, and ask when a decision is beyond them. Agents hand work to other agents, review each other's work, and merge it, so a chain can run to the end with nobody watching. Every change is still made by an identifiable account, recorded in Git, and reversible.

> Authority flows downward. Information, accountability and escalation flow upward. Execution is autonomous within delegated bounds.

Garrison does not build a second system for this. The organisation is the agents' folders, grants and chains in Git; the authority engine is the gateway and the approval check; the audit is Git's history and the dashboard's feed. Everything stays changeable only by pull request (see Conventions in [AGENTS.md](../AGENTS.md)), so promoting an agent is a reviewed diff, not an API call.

## The rules

1. **People are the highest authority.** Root admins change the platform and its agents (`garrison/config`); a unit's admins decide what agents may do in that unit (`grants.yaml`, `routines.yaml`). *Built.*
2. **No agent widens its own authority.** Agents never write to a `config` repo, so they cannot change grants, routines, members or agents, nor make an agent. *Built* (What no grant can lift, in agents.md).
3. **Delegation never adds rights.** An agent asked by another acts for the same person, and every grant is checked against the person or routine owner who started the run, never against the asking agent. *Built* (`ask_agent`).
4. **Only what is listed is allowed.** An agent has the tools its `agent.yaml` lists and the grants its units give it; everything else is refused. There are no deny-lists to forget an entry in. *Built.*
5. **Risk comes from facts, not words.** What a change may touch (repo, branch, paths, tool) decides how much approval it needs, never how an agent describes it. *Partly built:* per repo and branch; paths are design.
6. **Silence is never approval.** A question to a person waits for their answer, up to 24 hours; then the run stops, it does not go on. *Built* (`ask_person`, `merge: ask`).
7. **Reports are data, never commands.** What an agent hands back is information for the asker, not instructions. An agent that reads untrusted input (the web, mail, issues of outsiders) holds no right to change a repo; the one that changes it never reads that input. *Built for* `news-page` *and* `software-updates`; design: checked for every chain.
8. **Someone else checks the work.** The agent that wrote a change does not approve it. Today that is a person; with oversight (below), it may be another agent. *Design.*
9. **Everything is accountable.** Each change is made by a named account, says whom it was for (*Asked by ...*), and is in Git and the feed. *Built.*
10. **Some things no approval lifts.** What no grant can lift stays refused even if a person asks: an agent changing its own rights, admin settings, another unit's repos. *Built.*
11. **People can always stop it.** `enabled: false` switches an agent off everywhere within seconds; removing a grant or a routine takes its authority away at the next sync; a revert undoes a merge. *Built.*

## The organisation

The ranks of a military hierarchy map onto what Garrison has. Ranks are a matter of configuration, not of layers at runtime: every hop between agents costs time and tokens, loses detail, and is one more place for an injected instruction to spread, so a run holds at most three agents (`MaxChain` in `control-plane/internal/agentapi/ask.go`).

| Rank | In Garrison | Defined in |
| --- | --- | --- |
| 0, human command | Root admins for the platform; a unit's admins and members for the unit | Keycloak group `root-admins`; `<unit>/config/members.yaml` |
| 1, chief of staff | The `commander`: finds the agent or chain for a request and hands it over | `garrison/config/agents/commander/` |
| 2, chain lead | The `lead` of a chain: runs its steps one after another | `garrison/config/chains/<name>.yaml` |
| 3, specialists | The agents of the steps: `coder`, `scout`, `sentry`, `media`, `operator` | `garrison/config/agents/<name>/` |
| Oversight | An agent with review rights but no command over others (below) | Design |

Example, the chains of today:

```text
people of the unit (members.yaml) ─ approve grants and routines once
│
└── commander ─ hands a request to the lead of a matching chain
    │
    ├── news-page (lead: coder, merge: auto)
    │   ├── coder   reads topics.md and news.yaml
    │   ├── scout   reads the web, cannot write a repo
    │   └── coder   writes news.yaml, proposes and merges it
    │
    └── software-updates (lead: sentry, merge: review)
        ├── sentry  lists images and versions
        ├── scout   finds the newest releases
        ├── sentry  sorts them: minor proposed, major reported
        ├── coder   proposes one pull request per repo
        └── sentry  reports
```

Departments are units: a unit has its own people, repos, channel, grants and routines, and every agent works in every unit with that unit's rights. New agents and chains come by pull request to `garrison/config`, approved by a root admin; an agent cannot create another. A helper for a single task is an `ask_agent` call, which adds no rights.

## Authority

An agent's authority in a unit is the sum of three things, each in Git:

- **Tools** in its `agent.yaml`: what it can do at all.
- **Grants** in the unit's `grants.yaml`: where it may change things with its own account, per repo and branch, at the levels `propose`, `merge` and `autonomous`, for which of the unit's people (`for`), and whether a run without a person may use them (`unattended`).
- **The chain's `merge`** for a run: `review`, `ask` or `auto`, the most it may do with its pull requests.

The gateway decides each call from these, in that order, and refuses with the grant that would be needed. A grant always names the unit's people it serves, so a run without a person acts for its routine's owner, who must still be in the unit.

## Risk levels

Risk is decided by the gateway from what a change touches, so an agent cannot lower it by calling a change routine.

| Level | What | How Garrison handles it | Status |
| --- | --- | --- | --- |
| R0, routine | Reading, researching, working in its own sandbox | No approval: the person's or the agent's read rights | Built |
| R1, low | A proposal: a branch and a pull request, nothing deployed | `propose` grant, or the person's own rights | Built |
| R2, medium | A merge that does not deploy | `merge` grant; with oversight, another agent's approval | Grant built, oversight design |
| R3, high | A merge that deploys, a change to a stack or data | A person merges (`merge: review`), or agrees first (`merge: ask`), or approved an `autonomous` grant beforehand | Built |
| R4, prohibited | What no grant can lift | Refused, whoever asks | Built |

Design: rules by path in a grant, e.g. `paths: [news.yaml]` to allow merging only that file, and a list of paths that always count as R3 (`migrations/**`, `stacks/**`). The test of a rule is that the gateway decides it from the diff's file names, never from its content's meaning.

## Escalation

- **To a person:** a run that needs a choice, a fact or an approval calls `ask_person`, ends its turn, and goes on when the routine's owner answers in the question's thread. Without an answer in 24 hours, it stops and says so in the thread. Under `merge: ask` it merges only after that answer. *Built.*
- **To the lead first:** an agent that another one asked may not call `ask_person`; it puts its question in its answer to the asker, which asks the person if it cannot decide. *Built.* Design: the lead decides itself what is within its authority (an R1 or R2 matter), and asks the person only for R3.
- **Straight to a person:** an agent that finds a critical problem (a failing deploy, a secret in a diff) tells the unit's channel at once, past its lead. *Design;* `sentry` reports in the channel today.

A question says what is wrong, what the agent proposes, why, what it risks, the alternatives and how to undo it. The affected work waits; other work goes on.

## Commands and reports

- **A command** is an `ask_agent` call: `task`, `limits` and `done_when` as fields, not a paragraph to guess from; or a chain's step, its `does`. The run's chain of agents is kept by the agent service, not by anything a model writes. *Built.*
- **A report** is the tool result the asked agent returns: data for the asker. *Built.*
- **Waiting:** `background: true` lets the asker go on while the asked agent works; its answer is posted in the channel. *Built.*
- **Tracing:** the feed has each hand-off and its answer, the asked agent's calls threaded under it. Design: an id per command and its parent's, so the dashboard shows a run as a tree.

## Oversight

An oversight agent reviews work it did not write, and may approve or block it, without commanding anyone. It is how a chain runs to the end without a person and still has a second pair of eyes.

Design:

- **A tool `review_change(unit, repo, number, verdict, body)`**: approve, or ask for changes, as the reviewing agent's own account. It needs a grant on the repo like proposing does.
- **A chain mode `merge: agent`**: the lead merges a pull request only after an agent other than its author approved it in Gitea. The gateway checks the approval's account, so an agent cannot approve its own work under another name, and a request for changes blocks the merge.
- **Approvals that count:** an agent's approval counts on branches its grant allows to merge; where a unit's rules ask for another person's approval (`main` of a unit with two or more people), it does not replace that person.
- **Separate eyes:** the reviewer gets the diff and the repo, not the author's conversation, so it judges the change, not the story told about it. It is still a model, often the same one, so it is an extra check on top of tests, path rules and easy reverts, never instead of them.

## Identity

Each agent with a grant or routine in a unit has an account there, `agent-<unit>-<name>`, in Gitea and Keycloak; the gateway acts as it, and it may write only to its granted repos. Today, in a chat, a change is made as the person whenever they may make it themselves, and as the agent's account only where only a grant allows it. Design: a grant option `as: agent`, so the agent works with its own account in that unit also in chats; the pull request still names the person (*Asked by alice through todo/coder*), so the trail stays whole.

## What this does not add

- **No message bus or command service.** Hand-offs are `ask_agent` calls and chain steps; state is in Git, the feed and Postgres, as today.
- **No ranks at runtime.** Who leads is a chain's `lead`; who may do what is tools and grants. A rank is a description in the docs, not something the gateway checks.
- **No agents made by agents.** They come by pull request to `garrison/config`.

## Steps

Each step is a pull request with a Go test and a scenario (see Build and test in [AGENTS.md](../AGENTS.md)).

1. **Oversight:** `review_change` and `merge: agent`; a chain `reviewed-change` (coder writes, a reviewer approves, the lead merges), tested with one approval and one request for changes.
2. **The agent's own account:** `as: agent` in a grant; a chat's pull request is the agent's, with *Asked by*.
3. **Risk by path:** `paths` in grants, and paths that always need a person.
4. **Escalation to the lead:** the lead decides questions within its authority, and asks a person only for R3.
5. **Tracing:** an id per command and its parent's in the feed, and the run as a tree on the dashboard.
