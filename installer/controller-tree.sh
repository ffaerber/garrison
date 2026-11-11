#!/usr/bin/env bash
# controller-tree.sh OUTDIR: assembles the contents of the repo
# system/controller: its stack (compose.yaml), the controller's source, the
# Dockerfile and the build workflow.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=$1
rm -rf "$out"
mkdir -p "$out/.gitea/workflows"
cp "$ROOT/system/controller/compose.yaml" "$ROOT/go.mod" "$ROOT/go.sum" "$out/"
cp -r "$ROOT/schemas" "$ROOT/controller" "$out/"
mv "$out/controller/ci/build.yaml" "$out/.gitea/workflows/build.yaml"
rmdir "$out/controller/ci"
cat > "$out/README.md" <<'MD'
# Controller

Deploys every repo of the `system` organisation that holds a `compose.yaml`, itself included.

- `controller/`, `schemas/`, `go.mod`: the source.
- `controller/Dockerfile`: the image.
- `.gitea/workflows/build.yaml`: on every push to `main`, builds the image, pushes it to this Gitea's registry and sets its tag in `compose.yaml`; the running controller then deploys it.
- `compose.yaml`: the stack.
MD
