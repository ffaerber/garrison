#!/usr/bin/env bash
# Runs BuildKit for the user namespaces' pipelines: started by
# garrison-buildkit.service, as a container outside Swarm because it must be
# privileged, which Swarm services cannot be. It is reachable only on the
# builds network, as garrison-buildkit:1234, where the user runner's jobs
# send it Dockerfiles. Each build step runs in BuildKit's own sandbox, without
# privileges; BuildKit grants no insecure entitlements. Images go straight to
# Gitea's registry, never into the host's Docker.
set -euo pipefail
. /etc/garrison/install.env
exec docker run --rm --name garrison-buildkit --privileged \
  --network garrison-builds \
  --add-host "git.$DOMAIN:host-gateway" \
  -v garrison-buildkit:/var/lib/buildkit \
  -v /etc/garrison/buildkitd.toml:/etc/buildkit/buildkitd.toml:ro \
  -v /etc/garrison/tls.crt:/etc/garrison/ca.crt:ro \
  moby/buildkit:v0.34.1 --addr tcp://0.0.0.0:1234
