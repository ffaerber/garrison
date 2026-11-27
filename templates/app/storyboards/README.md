# {{repo}}

The stories of the unit {{unit}}'s shorts: one file per short, written before it is made. Agents write them with the chain of command `storyboards`, a person reviews and merges them, and `@designer` makes a short of every story on `main` that has none yet.

1. the coder reads this README and the stories already here;
2. the scout finds new, true facts on the theme on the web, each with its sources;
3. the coder writes one story per fact and proposes them as one pull request;
4. you review it: check each fact against its sources, change what you like, and merge;
5. `@designer` speaks the narration, draws the scenes in the project's styles and voice, and cuts the short into the media library; the channel's public page shows it.

## The channel

Change these lines for yours; the agents read them.

- **Theme:** surprising, true facts about animals
- **Audience:** children and families: friendly, never scary or gory
- **Media project:** {{unit}}/shorts (its styles and voice)
- **New stories per run:** 3

## A story

`stories/<subject>-<a-few-words>.md`, e.g. `stories/octopus-three-hearts.md`:

```markdown
---
title: An octopus has three hearts
subject: octopus
project: {{unit}}/shorts
sources:
  - https://www.example.org/octopus
---

## Narration

Exactly the words spoken: 75 to 90 words, about 30 seconds (the narrator speaks some 2.7 words a second).

## Scenes

1. clip: what moves in the first clip, about 5 seconds
2. clip: what moves in the second
3. clip: ...
```

The scenes are in the order spoken, 5 to 7 of them, each a `clip:` of about 5 seconds: the shorts are made of video clips only. A changed story is not made again: to remake a short, delete it from the library.

## Keeping it going

Ask the operator, in this unit's channel: "Write three new stories every Monday, and make the shorts of merged stories". It proposes in `{{unit}}/config`'s `routines.yaml`:

```yaml
- chain: storyboards                  # new stories, for you to review
  name: new-stories
  schedule: "0 7 * * mon"
  owner: <you>
  task: New stories for {{unit}}/{{repo}}.
- agent: designer                        # a short for each merged story
  name: shorts-from-stories
  on_push: { repo: {{repo}}, branch: main, paths: [stories/] }
  owner: <you>
  task: Make the short of the next story of {{unit}}/{{repo}} that has none yet.
- agent: designer                        # the rest, one per run: a short takes minutes
  name: shorts-catch-up
  every: 30m
  owner: <you>
  task: Make the short of the next story of {{unit}}/{{repo}} that has none yet.
```

A run makes one short, as a turn may take ten minutes; the catch-up routine makes the rest, one every half hour, and stays quiet once every story has its short.
