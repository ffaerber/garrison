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
narration.yaml       the project's voice for generate_speech, e.g. voice: af_heart, speed: 1.0
```

## Narration

`narration.yaml` gives every narration of the project one voice: `voice` (a voice of the text-to-speech service: af_heart, af_bella, am_adam, am_michael, bf_emma, bm_george, ...) and optionally `speed` (0.5 to 2, default 1). generate_speech uses it whatever voice is asked for, and writes `narration-<name>-<time>.mp3` with its `.json`. Propose it with the project when the person wants shorts with a voice.

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

A workflow names model files (e.g. `unet_name`, `clip_name`, `vae_name`, `ckpt_name`). They must be ones ComfyUI has: `media_models` lists them, says which model families they make up and which nodes and settings each needs. A workflow for a model that is not there is refused when it runs.

The workflow must save its results (`SaveImage`, or a node that saves video), or the run keeps nothing.

## Styles for shorts

A project for shorts has two styles in one look: a picture style for key pictures (Z-Image, portrait 704x1280) and a video style that starts from a picture (Wan 2.2 image to video; its `nodes` place `image`). Copy both workflows from `media_models`. Neither style's prompt nor negative may ask for slow or little movement.

## Setting one up

1. Ask the person what the project is and which looks they want. If they have no workflow, write one: call `media_models`, pick the family that fits (pictures or video) and copy its example workflow as it is, with the style `nodes` it gives; change only prompt defaults, size, length or steps. A workflow written from memory is usually refused (a missing input, a wrong one).
2. Propose the repo's files as one pull request: README.md, the workflow, one style per look, and narration.yaml if it is to speak.
3. Once merged, make one test picture per style with `generate_media` and show the person.
4. Change a style by pull request; keep the old file if pictures were made with it, as their `.json` names it.

## From a storyboard

A repo made from the template `storyboards` holds one story per short, `stories/<name>.md`: front matter (`title`, `subject`, `project`, `sources`), `## Narration` (the words to speak) and `## Scenes` (numbered, each `clip:` and what moves in it; `picture:` only for a still the story wants). People review and merge them; you make a short of each story on `main`.

1. Find the next story: `list_files` of `stories/` on main, by name; the first whose `/data/media/<project>/shorts/<name>.mp4` does not exist yet. None left: you are done (in a routine, answer only `NOTHING TO DO`).
2. `generate_speech` with the narration exactly as written, `name` the story's name. Its length is the short's.
3. One key picture of the story's subject with the project's picture style (704x1280), then each scene with `generate_media` in the project's image-to-video style, `image` the key picture and the scene's words as the prompt, saying what the subject does: a clip of 5 seconds each, the same character in all of them. A `picture:` scene only if the story asks for a still.
4. Cut them in the scenes' order to the narration's length, as for any short (crossfades, loudness -14 LUFS, 48 kHz stereo), into `/data/media/<project>/shorts/<name>.mp4`, with its `.json`, `<name>.mp4.json`: `title` (the story's), `narration` (its text), `story` (`<unit>/<repo>/stories/<name>.md`), `files` and `captions`.
5. Say which story you made, and where to watch it.
