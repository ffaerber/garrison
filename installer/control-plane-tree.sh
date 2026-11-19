#!/usr/bin/env bash
# control-plane-tree.sh OUTDIR: assembles the contents of the repo
# garrison/control-plane: its stack (compose.yaml), the source, the
# Dockerfiles (the control plane's, the agents' sandbox's and the
# namespaces' file browser's) and the build
# workflow.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=$1
rm -rf "$out"
mkdir -p "$out/.gitea/workflows"
cp "$ROOT"/garrison/control-plane/* "$ROOT/go.mod" "$ROOT/go.sum" "$out/"
cp -r "$ROOT/schemas" "$ROOT/templates" "$ROOT/control-plane" "$ROOT/sandbox" "$ROOT/files" "$out/"
mv "$out/control-plane/ci/build.yaml" "$out/.gitea/workflows/build.yaml"
rmdir "$out/control-plane/ci"
cat > "$out/README.md" <<'MD'
# Control plane

Deploys every repo of the `garrison` organisation that holds a `compose.yaml`, itself included.

- `control-plane/`, `schemas/`, `templates/`, `go.mod`: the source.
- `control-plane/Dockerfile`: the image.
- `sandbox/`: the agents' sandbox image: Pi, the agent harness, and what agents work with.
- `files/`: the namespaces' file browser image, `garrison/files`: Filebrowser behind Garrison's gate.
- `.gitea/workflows/build.yaml`: on every push to `main`, builds the images, pushes them to this Gitea's registry and sets their tag in `compose.yaml`; the running deployer then deploys them.
- `compose.yaml`: the stack.
MD
