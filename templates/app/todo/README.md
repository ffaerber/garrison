# {{repo}}

A small todo app on Postgres, of the namespace {{namespace}}. Everyone has a list of their own: people sign in with Garrison's sign-in (Keycloak, through the login proxy in front of private environments), which tells the app who they are, and each sees, ticks and deletes only their own todos. A request without a signed-in person is refused; only `/healthz` answers without one. Who signed in is believed only with the environment's ingress key (`GARRISON_INGRESS_KEY`, which Traefik sends as `X-Garrison-Ingress`): another app that calls this one directly, past the sign-in, is refused.

- `main.go`, `store.go`, `page.html`: the app. It shows who added each todo, as the installation's sign-in reports it.
- `main_test.go`: its tests, run in the Dockerfile's test stage.
- `Dockerfile`: its image: tests first, then the build, with Go's caches kept between builds.
- `.gitea/workflows/pipeline.yaml`: on every push, one BuildKit build that tests, builds and pushes `{{registry}}/{{namespace}}/{{repo}}:<commit>`, with its layer cache kept in Gitea's registry as `:buildcache`.

A failing test stops the pipeline, so no image is built and the app keeps running at its last good commit.

## Running it

How it runs is not in this repo: a stack in the repo `{{namespace}}/config` names this repo as its source. Its file name is its hostname: `stacks/todo.yaml` runs `main` at `https://todo.<domain>` and a branch `dev` at `https://dev-todo.<domain>`, once the commit's image exists. Each environment has its own database on its own ZFS dataset, and its own generated password.

```yaml
source: {{repo}}
entrypoint: web

services:
  web:
    image: "{{registry}}/{{namespace}}/{{repo}}:{{commit}}"
    port: 8080
    env:
      DATABASE_HOST: db
      DATABASE_NAME: todo
      DATABASE_USER: todo
      DATABASE_PASSWORD_FILE: /run/secrets/db-password
    secrets: [db-password]
    resources: { memory: 128Mi }

  db:
    image: postgres:17-alpine
    env:
      POSTGRES_DB: todo
      POSTGRES_USER: todo
      POSTGRES_PASSWORD_FILE: /run/secrets/db-password
    volumes:
      data: { dataset: todo-db, size: 2Gi, path: /var/lib/postgresql/data }
    secrets: [db-password]
    resources: { memory: 256Mi }

environments:
  main: { private: true }   # behind the sign-in: the app needs to know who you are
  dev: { private: true }
```
