# Built by Gitea Actions from the repo system/controller (controller/ci/build.yaml).
# Context: the repo root, which holds go.mod, schemas/ and controller/.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# Cache mounts: Go's module and build caches stay in the Docker daemon's
# BuildKit between builds, so a small change compiles in seconds.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY schemas ./schemas
COPY controller ./controller
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /controller ./controller/cmd/controller

# The Docker CLI deploys stacks; git follows the platform repo; nsenter runs
# ZFS commands on the host.
FROM docker:29-cli
RUN apk add --no-cache git util-linux-misc
COPY --from=build /controller /usr/local/bin/controller
ENTRYPOINT ["/usr/local/bin/controller"]
