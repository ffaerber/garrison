# {{repo}}

A static web page of the namespace {{namespace}}: `index.html`, served by nginx (`Dockerfile`). Every push builds its image (`.gitea/workflows/pipeline.yaml`), `{{registry}}/{{namespace}}/{{repo}}:<commit>`.

It runs once the namespace's config names it, in `stacks/{{repo}}.yaml` of the repo `{{namespace}}/config`:

```yaml
source: {{repo}}
entrypoint: web
services:
  web:
    image: "{{registry}}/{{namespace}}/{{repo}}:{{commit}}"
    port: 8080
    resources: { memory: 64Mi }
environments:
  main: {}     # https://{{repo}}.<domain>
  dev: {}      # https://dev-{{repo}}.<domain>, from the branch dev
```

Add `private: true` to an environment to put it behind the sign-in.
