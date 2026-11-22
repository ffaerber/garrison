# Built by BuildKit for every push (.gitea/workflows/pipeline.yaml). The tests
# run here too, in the stage test, and check news.yaml: if it has a mistake,
# or a test fails, there is no image, and the page keeps its last good
# version. Go's module and build caches are cache mounts, kept by BuildKit
# between builds.
FROM golang:1.26-alpine AS source
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go page.html news.yaml topics.md ./

FROM source AS test
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./... && touch /tested

FROM source AS build
# Depends on the tests, so BuildKit runs them before it builds.
COPY --from=test /tested /tmp/tested
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /news .

FROM alpine:3.22
COPY --from=build /news /usr/local/bin/news
USER nobody
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/news"]
