# Go runs in a container so the host needs only Docker. Set GO=go to use a local toolchain.
GO_IMAGE ?= golang:1.26-alpine
GO ?= docker run --rm -u $(shell id -u):$(shell id -g) -v $(CURDIR):/src -w /src \
	-v homecloud-gocache:/cache -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 $(GO_IMAGE) go

.PHONY: test build tidy golden
test:
	$(GO) vet ./...
	$(GO) test ./...

build:
	$(GO) build -o bin/homecloud ./control-plane/cmd/homecloud

tidy:
	$(GO) mod tidy

golden:
	$(GO) test ./control-plane/internal/render -update
