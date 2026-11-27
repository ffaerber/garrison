# {{repo}}

The public page of the unit {{unit}}'s channel: every finished short of its media library, newest first, to watch, each with its title and what is said in it. `@designer` makes the shorts; this page shows them as soon as they are in the library, without a new deploy.

It reads the library read-only and serves only the shorts, the `.mp4` files in each project's `shorts/` folder; the pictures, clips, narrations, drafts and uploads beside them stay unseen. A short's title and text come from the `.json` beside it (`title`, `narration`, or its narration file's text, or its captions).

- `main.go`, `channel.go`, `page.html`: the page. It has no JavaScript and escapes everything it shows.
- `main_test.go`: its tests. A failing test fails the pipeline, so no image is built and the page keeps its last good version.
- `Dockerfile`, `.gitea/workflows/pipeline.yaml`: on every push, one BuildKit build that tests, builds and pushes `{{registry}}/{{unit}}/{{repo}}:<commit>`.

## Running it

How it runs is not in this repo: a stack in `{{unit}}/config` names this repo as its source. `stacks/{{repo}}.yaml` runs `main` at `https://{{repo}}.<domain>`, for everyone, without a sign-in:

```yaml
source: {{repo}}
entrypoint: web

services:
  web:
    image: "{{registry}}/{{unit}}/{{repo}}:{{commit}}"
    port: 8080
    env:
      TITLE: "{{repo}}"
      ABOUT: ""          # a line about the channel
      PROJECT: ""        # only this project's shorts; empty: every project's
    volumes:
      library: { share: media, readonly: true, path: /media }
    resources: { memory: 64Mi }

environments:
  main: {}
```

`main: { private: true }` puts it behind the installation's sign-in instead, for the unit's people to look first.
