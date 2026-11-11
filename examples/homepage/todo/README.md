# todo

A small todo list on Postgres: the example app of the namespace `homepage`.

- `main.go`, `store.go`, `page.html`: the app. It shows who added each todo, as the installation's sign-in reports it.
- `main_test.go`: its tests, run in the Dockerfile's test stage.
- `Dockerfile`: its image: tests first, then the build, with Go's caches kept between builds.
- `.gitea/workflows/pipeline.yaml`: on every push, one BuildKit build that tests, builds and pushes `git.<domain>/homepage/todo:<commit>`, with its layer cache kept in Gitea's registry as `:buildcache`.
- `stack.yaml`: the app and its database. The controller deploys `main` to `https://todo.<domain>` and a branch `dev` to `https://dev-todo.<domain>` once the commit's image exists. Each environment has its own database on its own ZFS dataset.

A failing test stops the pipeline, so no image is built and the app keeps running at its last good commit.
