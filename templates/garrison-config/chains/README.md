# Chains of command

The order in which agents do one kind of job, one file each: `<name>.yaml`. Only root admins change this repo, so only they add or change chains.

```yaml
description: what the chain does
when: which requests it is for, in words; the commander hands those to its lead
lead: sentry              # the agent that runs it; its first step is the lead's
steps:                    # in order, 2 to 12
  - agent: sentry
    does: what this agent does in this step
  - agent: scout
    does: ...
```

- **The lead runs the chain.** It does its own steps, and asks each other agent for its step with `ask_agent`, one after another, passing on what that step needs from the steps before. Each agent answers back to the lead; none asks the next one itself. The lead may ask the agents of its chains without naming them in `agents:` of its `agent.yaml`.
- **Every agent knows the order:** its system prompt lists each chain it is part of, with its own steps and who comes after it. The commander knows every chain, and hands a request that matches one's `when` to its lead as a whole.
- **A routine** can run a chain: `chain: <name>` in a namespace's `routines.yaml`, without `agent` (the lead runs it) and with `task` only for what this routine adds. `add_routine` with `chain` and `repos` also gives the chain's agents that propose (with `propose_patch` or `screenshot`) the grants on those repos that runs with nobody watching need.
- A chain naming an agent that is not there, or switched off, is left out, and the approval check says why.

`software-updates`: the sentry lists the versions Garrison runs, the scout finds the releases on the web, the coder proposes the minor and patch updates, and the sentry reports, majors included. Every Monday at 7 is a routine in garrison, owned by a root admin; ask the operator: "Check for software updates every Monday".
