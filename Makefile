VERSION ?= $(shell cat ./VERSION)
GITHASH ?= $(shell git describe --match nEvErMatch --always --abbrev=10 --dirty)
NAME=bin/hs-csi-plugin
RELEASE_IMAGE ?= hammerspaceinc/csi-plugin:${VERSION}
TRIVY_SEVERITY ?= HIGH,CRITICAL
RELEASE_BUILD_FLAGS ?= --pull --no-cache

.PHONY: compile clean unittest sanity build-dev build build-release build-release-image scan-release

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

build:
	@echo "==> Building Docker Image Latest"
	@docker build -t "hammerspaceinc/csi-plugin:latest" . -f Dockerfile --no-cache

build-release: build-release-image
	@$(MAKE) --no-print-directory scan-release

build-release-image:
	@echo "==> Building Docker Image ${VERSION} ${GITHASH}"
	@docker build ${RELEASE_BUILD_FLAGS} --build-arg version=${VERSION} -t "${RELEASE_IMAGE}" . -f Dockerfile

scan-release:
	@command -v trivy >/dev/null 2>&1 || { \
		echo "ERROR: Trivy is required to scan release images: https://trivy.dev/latest/getting-started/installation/"; \
		exit 1; \
	}
	@echo "==> Scanning Docker Image ${RELEASE_IMAGE} with Trivy"
	@trivy image --exit-code 1 --severity "${TRIVY_SEVERITY}" "${RELEASE_IMAGE}"
