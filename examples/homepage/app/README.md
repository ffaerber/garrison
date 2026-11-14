# homepage

A hello-world homepage: one static page, the example app of the namespace `homepage`, in its repo `homepage/app`.

- `index.html`: the page.
- `Dockerfile`: nginx, as an unprivileged user, on port 8080.
- `.gitea/workflows/pipeline.yaml`: on every push, builds and pushes `git.<domain>/homepage/app:<commit>`.

How it runs is not in this repo: `homepage/config/stacks/homepage.yaml` (see `examples/homepage/config/stacks/homepage.yaml`) names this repo as its source. The deployer deploys `main` to `https://homepage.<domain>`, open to everyone, without the sign-in.
