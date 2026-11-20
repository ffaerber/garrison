# Built by Gitea Actions from the repo garrison/control-plane (control-plane/ci/build.yaml).
# Context: the repo root, which holds go.mod, schemas/, templates/ and control-plane/.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# Cache mounts: Go's module and build caches stay in the Docker daemon's
# BuildKit between builds, so a small change compiles in seconds.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY schemas ./schemas
COPY templates ./templates
COPY control-plane ./control-plane
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /garrison ./control-plane/cmd/garrison

# The Docker CLI deploys stacks; git follows the platform repo; nsenter runs
# ZFS commands on the host; sops and age encrypt the units' secrets
# (the dashboard) and decrypt them (the deployer, which alone has the keys).
# tini is PID 1 and reaps orphans: git leaves helpers behind (git-remote-http
# of a fetch cut short, detached auto-maintenance), and garrison as PID 1
# never waited for them, so the deployer gathered zombies until fork failed.
FROM docker:29-cli
RUN apk add --no-cache git util-linux-misc sops age tini
COPY --from=build /garrison /usr/local/bin/garrison
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/garrison"]
