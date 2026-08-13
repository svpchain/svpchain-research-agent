#!/usr/bin/make -f

VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
AGENT   := svpchain-research-agent

ldflags = -X github.com/cosmos/cosmos-sdk/version.Name=svpchain \
	-X github.com/cosmos/cosmos-sdk/version.AppName=$(AGENT) \
	-X github.com/cosmos/cosmos-sdk/version.Version=$(VERSION) \
	-X github.com/cosmos/cosmos-sdk/version.Commit=$(COMMIT)

BUILD_FLAGS := -ldflags '$(ldflags)'

# GOWORK=off everywhere: a go.work in the parent directory makes this module
# resolve svpchain-agent-core from the local checkout, which hides a missing
# version bump and would ship against a core revision that was never tagged.
GO := GOWORK=off go

.PHONY: build test vet fmt vendor docker deploy clean

build:
	$(GO) build -mod=readonly $(BUILD_FLAGS) -o build/$(AGENT) ./cmd/$(AGENT)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

# Materialize dependencies so the Docker build is self-contained: the go.mod
# replace to ../svpagent/protocol is not inside the build context.
vendor:
	$(GO) mod vendor

docker: vendor
	docker build --platform linux/amd64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t ghcr.io/svpchain/$(AGENT):$(VERSION) \
		-f cmd/$(AGENT)/Dockerfile .

deploy:
	./scripts/deploy.sh $(DEPLOY_FLAGS)

clean:
	rm -rf build/ vendor/
