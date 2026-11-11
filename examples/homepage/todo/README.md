# todo

A small todo list on Postgres: the example app of the namespace `homepage`.

- `main.go`, `store.go`, `page.html`: the app. It shows who added each todo, as the installation's sign-in reports it.
- `main_test.go`: its tests, run by the pipeline.
- `Dockerfile`: its image.
- `.gitea/workflows/pipeline.yaml`: on every push, test, then build and push the image `git.<domain>/homepage/todo:<commit>`.
- `stack.yaml`: the app and its database. The controller deploys `main` to `https://todo.<domain>` and a branch `dev` to `https://dev-todo.<domain>` once the commit's image exists. Each environment has its own database on its own ZFS dataset.

A failing test stops the pipeline, so no image is built and the app keeps running at its last good commit.
