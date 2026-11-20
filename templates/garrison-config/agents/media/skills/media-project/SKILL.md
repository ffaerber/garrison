---
name: media-project
description: How a media project is laid out (styles, ComfyUI workflows, the media library) and how to set one up. Load it before making a project or changing a style.
---

# A media project

A project is a repo of the unit, e.g. `media/demo`, and a folder of its media library, `/data/media/demo/`, where `generate_media` puts what it makes, each file with a `.json` beside it saying how it was made (prompt, style, seed, workflow).

```
README.md            what the project is: subject, audience, tone; read it before generating
styles/<style>.yaml  one file per style
workflows/<name>.json ComfyUI workflows in API format (in ComfyUI: Workflow → Export (API))
```

## A style

```yaml
workflow: workflows/comic.json            # in this repo
prompt: "{prompt}, comic style, bold ink lines, flat colours"   # {prompt}: what the agent asks for
negative: photo, blurry, text, watermark
width: 1024
height: 1024
frames: 0           # for a video workflow: its length in frames
seed: 1234          # optional: always this seed; without it each run gets a random one
nodes:              # where each setting goes: the node's id in the workflow and its input
  prompt:   { node: "6", input: text }
  negative: { node: "7", input: text }
  seed:     { node: "3", input: seed }
  width:    { node: "5", input: width }
  height:   { node: "5", input: height }
  frames:   { node: "12", input: length }
```

Only `workflow` and `nodes.prompt` are needed. A setting without a node stays as the workflow has it.

To find the node ids: read the workflow's JSON; each top-level key is a node id, with its `class_type` (e.g. `CLIPTextEncode` for a prompt, `KSampler` for the seed, `EmptyLatentImage` for the size) and its `inputs`. The positive and negative prompts are both `CLIPTextEncode`: the positive one feeds the sampler's `positive` input.

The workflow must save its results (`SaveImage`, or a node that saves video), or the run keeps nothing.

## Setting one up

1. Ask the person what the project is and which looks they want; ask for the workflow JSON if there is none in the project yet (people export it from ComfyUI).
2. Propose the repo's files as one pull request: README.md, the workflow, one style per look.
3. Once merged, make one test picture per style with `generate_media` and show the person.
4. Change a style by pull request; keep the old file if pictures were made with it, as their `.json` names it.
