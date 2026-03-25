APP_VERSION ?= $(shell git describe --always --dirty --tags 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD)
BUILD_DATE ?= $(shell date -u "+%Y%m%d-%H%M")
VERSION_PKG := github.com/InjectiveLabs/web3-gateway/version
VERSION_FLAGS := -X $(VERSION_PKG).AppVersion=$(APP_VERSION) -X $(VERSION_PKG).GitCommit=$(GIT_COMMIT) -X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

DOCKERHUB_IMAGE ?= injectivelabs/web3-gateway
TAG ?= $(GIT_COMMIT)
DOCKER_PLATFORM ?= linux/amd64

install:
	go install \
		-ldflags '$(VERSION_FLAGS)' \
		./cmd/web3-gateway

build:
	mkdir -p build
	go build \
		-ldflags '$(VERSION_FLAGS)' \
		-o build/web3-gateway \
		./cmd/web3-gateway

docker:
	@echo "Building image for $(DOCKERHUB_IMAGE):$(TAG)"
	docker buildx build \
		--platform $(DOCKER_PLATFORM) \
		--build-arg VERSION_FLAGS="$(VERSION_FLAGS)" \
		-t $(DOCKERHUB_IMAGE):$(TAG) \
		-f Dockerfile \
		--load \
		.

buildx: docker

buildx-push:
	@echo "Building and pushing multi-arch image for $(DOCKERHUB_IMAGE):$(TAG)"
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION_FLAGS="$(VERSION_FLAGS)" \
		-t $(DOCKERHUB_IMAGE):$(TAG) \
		-f Dockerfile \
		--push \
		.

buildx-push-latest:
	@echo "Building and pushing multi-arch image with latest tag"
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION_FLAGS="$(VERSION_FLAGS)" \
		-t $(DOCKERHUB_IMAGE):$(TAG) \
		-t $(DOCKERHUB_IMAGE):latest \
		-f Dockerfile \
		--push \
		.

buildx-setup:
	docker buildx create --name web3-gateway-builder --use --bootstrap || docker buildx use web3-gateway-builder

buildx-clean:
	docker buildx rm web3-gateway-builder || true

.PHONY: install build buildx docker buildx-push buildx-push-latest buildx-setup buildx-clean
