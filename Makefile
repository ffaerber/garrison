# Go runs in a container so the host needs only Docker. Set GO=go to use a local toolchain.
GO_IMAGE ?= golang:1.26-alpine
GO ?= docker run --rm -u $(shell id -u):$(shell id -g) -v $(CURDIR):/src -w /src \
	-v garrison-gocache:/cache -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 $(GO_IMAGE) go

# Docker makes a new volume root's: hand it to this user once, as the
# container runs as them. Skipped with a local toolchain.
GO_CACHE_OWNER = docker run --rm -v garrison-gocache:/cache $(GO_IMAGE) chown $(shell id -u):$(shell id -g) /cache

.PHONY: test build tidy golden cache
cache:
ifneq ($(GO),go)
	@$(GO_CACHE_OWNER)
endif

test: cache
	$(GO) vet ./...
	$(GO) test ./...

build: cache
	$(GO) build -o bin/garrison ./control-plane/cmd/garrison

tidy: cache
	$(GO) mod tidy

golden: cache
	$(GO) test ./control-plane/internal/render -update
