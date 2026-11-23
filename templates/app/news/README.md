# {{repo}}

A page of what is new on your topics, of the unit {{unit}}: news, job offers, releases, whatever `topics.md` asks for. Garrison's agents keep it current by themselves, with the chain of command `news-page`:
1. the coder reads `topics.md` and the items already on the page;
2. the scout looks at the sources it names and searches the web for each topic;
3. the coder rewrites `news.yaml` with what is new, then proposes and merges it.

Each merge builds the page again and deploys it. The scout reads the web but cannot change any repo; the coder changes this repo but never opens a web page.

- `topics.md`: what the page looks for, in plain words, with the sites to look at first. Each topic is a `## ` heading. A line `Only from: example.com, jobs.example.org` under one limits its items to those sites. Edit it whenever you like; a change starts an update at once.
- `news.yaml`: what the page lists, one item per story with its title, link, source, date, topic and a one-line summary. `keep` is how many days an item stays.
- `main.go`, `news.go`, `page.html`: the page. It has no JavaScript, escapes everything it shows, and links only to the items' own `https://` addresses.
- `main_test.go`: its tests. They check `news.yaml`: a file with a mistake (an unknown field, a link that is not `https://`, the same link twice, a missing date, a topic that is not a heading of `topics.md`, a link off its topic's `Only from` sites, more than 100 items in one topic) fails the pipeline, so no image is built and the page keeps its last good version.
- `Dockerfile`, `.gitea/workflows/pipeline.yaml`: on every push, one BuildKit build that tests, builds and pushes `{{registry}}/{{unit}}/{{repo}}:<commit>`.

## Running it

How it runs is not in this repo: a stack in `{{unit}}/config` names this repo as its source. `stacks/{{repo}}.yaml` runs `main` at `https://{{repo}}.<domain>`, behind the installation's sign-in:

```yaml
source: {{repo}}
entrypoint: web

services:
  web:
    image: "{{registry}}/{{unit}}/{{repo}}:{{commit}}"
    port: 8080
    resources: { memory: 64Mi }

environments:
  main: { private: true }
```

## Keeping it current

Ask the operator: "Keep {{repo}} up to date every morning at 7". It proposes in `{{unit}}/config`:
- a routine running the chain `news-page` every morning;
- one running it when `topics.md` changes on `main` (`on_push` with `paths: [topics.md]`, so the coder's own merges of `news.yaml` start nothing);
- the grant that lets the coder merge into `main` once the pipeline passes, with nobody watching. The chain says `merge: auto`.

An admin of {{unit}} approves them once. After that, nobody approves the updates: undo one by reverting its merge.
