#!/usr/bin/env bash
# control-plane-tree.sh OUTDIR: assembles the contents of the repo
# system/control-plane: its stack (compose.yaml), the source, the
# Dockerfile and the build workflow.
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=$1
rm -rf "$out"
mkdir -p "$out/.gitea/workflows"
cp "$ROOT"/system/control-plane/* "$ROOT/go.mod" "$ROOT/go.sum" "$out/"
cp -r "$ROOT/schemas" "$ROOT/control-plane" "$out/"
mv "$out/control-plane/ci/build.yaml" "$out/.gitea/workflows/build.yaml"
rmdir "$out/control-plane/ci"
cat > "$out/README.md" <<'MD'
# Control plane

Deploys every repo of the `system` organisation that holds a `compose.yaml`, itself included.

- `control-plane/`, `schemas/`, `go.mod`: the source.
- `control-plane/Dockerfile`: the image.
- `.gitea/workflows/build.yaml`: on every push to `main`, builds the image, pushes it to this Gitea's registry and sets its tag in `compose.yaml`; the running deploller then deploys it.
- `compose.yaml`: the stack.
MD
