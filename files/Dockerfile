# A namespace's file browser: Filebrowser over its media library, behind
# Garrison's gate (garrison gate), which takes only requests that came
# through the sign-in and lets only the namespace's people change files.
# Filebrowser listens on the container's loopback alone, so nothing else
# reaches it. Built with the control plane (control-plane/ci/build.yaml);
# context: the repo root.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY schemas ./schemas
COPY templates ./templates
COPY control-plane ./control-plane
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /garrison ./control-plane/cmd/garrison

FROM filebrowser/filebrowser:v2.32.0
COPY --from=build /garrison /usr/local/bin/garrison
COPY files/start.sh /start.sh
EXPOSE 8080
# The base image's check asks the port of its config file, which is not
# where Filebrowser listens here. start.sh stops the container when the
# gate stops; this asks Filebrowser itself.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s CMD curl -fsS -o /dev/null http://127.0.0.1:8081/health || exit 1
ENTRYPOINT ["/bin/sh", "/start.sh"]
