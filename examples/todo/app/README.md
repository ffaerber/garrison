# todo

A small todo app on Postgres: the example app of the namespace `todo`, in its repo `todo/app`. Everyone has a list of their own: people sign in with the home cloud's sign-in (Keycloak, through the login proxy in front of private environments), which tells the app who they are, and each sees, ticks and deletes only their own todos. A request without a signed-in person is refused; only `/healthz` answers without one. Who signed in is believed only with the environment's ingress key (`HOMECLOUD_INGRESS_KEY`, which Traefik sends as `X-Homecloud-Ingress`): another app that calls this one directly, past the sign-in, is refused.

- `main.go`, `store.go`, `page.html`: the app. It shows who added each todo, as the installation's sign-in reports it.
- `main_test.go`: its tests, run in the Dockerfile's test stage.
- `Dockerfile`: its image: tests first, then the build, with Go's caches kept between builds.
- `.gitea/workflows/pipeline.yaml`: on every push, one BuildKit build that tests, builds and pushes `git.<domain>/todo/app:<commit>`, with its layer cache kept in Gitea's registry as `:buildcache`.
How it runs is not in this repo: `todo/config/stacks/todo.yaml` (see `examples/todo/config/stacks/todo.yaml`) names this repo as its source. The deployer deploys `main` to `https://todo.<domain>` and a branch `dev` to `https://dev-todo.<domain>` once the commit's image exists. Each environment has its own database on its own ZFS dataset.

A failing test stops the pipeline, so no image is built and the app keeps running at its last good commit.
