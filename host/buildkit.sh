#!/usr/bin/env bash
# Runs BuildKit for the user namespaces' pipelines: started by
# homecloud-buildkit.service, as a container outside Swarm because it must be
# privileged, which Swarm services cannot be. It is reachable only on the
# builds network, as homecloud-buildkit:1234, where the user runner's jobs
# send it Dockerfiles. Each build step runs in BuildKit's own sandbox, without
# privileges; BuildKit grants no insecure entitlements. Images go straight to
# Gitea's registry, never into the host's Docker.
set -euo pipefail
. /etc/homecloud/install.env
exec docker run --rm --name homecloud-buildkit --privileged \
  --network homecloud-builds \
  --add-host "git.$DOMAIN:host-gateway" \
  -v homecloud-buildkit:/var/lib/buildkit \
  -v /etc/homecloud/buildkitd.toml:/etc/buildkit/buildkitd.toml:ro \
  -v /etc/homecloud/tls.crt:/etc/homecloud/ca.crt:ro \
  moby/buildkit:v0.34.1 --addr tcp://0.0.0.0:1234
