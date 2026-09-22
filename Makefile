VERSION ?= $(shell cat ./VERSION)
GITHASH ?= $(shell git describe --match nEvErMatch --always --abbrev=10 --dirty)
NAME=bin/hs-csi-plugin
RUNTIME_BASE ?= runtime
IMAGE ?= hammerspaceinc/csi-plugin:latest
# Only runtime-base-rhel needs this; ordinary builds use public repositories.
RH_SUB_SECRET ?= $(HOME)/.config/redhat/build-subscription.env

.PHONY: compile clean unittest sanity build-dev runtime-base-rhel build build-release test-image

compile:
	@echo "==> Building the Hammerspace CSI Driver Version ${VERSION}"
	@env GO111MODULE=on go mod tidy
	@env GO111MODULE=on go mod download
	@env GO111MODULE=on CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -ldflags "-X 'github.com/hammer-space/csi-plugin/pkg/common.Version=${VERSION}' -X 'github.com/hammer-space/csi-plugin/pkg/common.Githash=${GITHASH}'" -o ${NAME} ./

clean:
	@echo "==> Cleaning"
	@env go clean
	rm -rf bin go.sum

unittest:
	@echo "==> Running tests"
	@env go test -v -count 1 -run="[^TestSanity]" ./...

sanity:
	@echo "==> Running sanity functional tests"
	@env GO111MODULE=on go test -timeout=0 -v ./test/sanity/...

build-dev:
	@echo "==> Building Docker Image for Dev Image"
	@docker build -t "hammerspaceinc/csi-plugin-dev:latest" . -f Dockerfile_dev --no-cache

# Maintainer-only alternative if an all-Red-Hat dependency supply is required.
runtime-base-rhel:
	@RH_SUB_SECRET="$(RH_SUB_SECRET)" hack/build_runtime_base.sh

build:
	@echo "==> Building Docker Image $(IMAGE)"
	@docker build --build-arg RUNTIME_BASE="$(RUNTIME_BASE)" --build-arg version="$(VERSION)" --build-arg release="$(GITHASH)" -t "$(IMAGE)" . -f Dockerfile

build-release:
	@echo "==> Building Docker Image ${VERSION} ${GITHASH}"
	@docker build --build-arg RUNTIME_BASE="$(RUNTIME_BASE)" --build-arg version="$(VERSION)" --build-arg release="$(GITHASH)" -t "hammerspaceinc/csi-plugin:${VERSION}" . -f Dockerfile

test-image:
	@hack/test_runtime_image.sh "$(IMAGE)"
