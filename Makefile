VERSION ?= $(shell cat ./VERSION)
GITHASH ?= $(shell git describe --match nEvErMatch --always --abbrev=10 --dirty)
NAME=bin/hs-csi-plugin
RELEASE_IMAGE ?= hammerspaceinc/csi-plugin:${VERSION}
TRIVY_SEVERITY ?= HIGH,CRITICAL
RELEASE_BUILD_FLAGS ?= --pull --no-cache

.PHONY: compile clean unittest sanity sanity-compile build-dev build build-release build-release-image scan-release sbom-release sign-release verify-release

compile:
	@echo "==> Building the Hammerspace CSI Driver Version ${VERSION}"
	@env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -mod=readonly -ldflags "-X 'github.com/hammer-space/csi-plugin/pkg/common.Version=${VERSION}' -X 'github.com/hammer-space/csi-plugin/pkg/common.Githash=${GITHASH}'" -o ${NAME} ./

clean:
	@echo "==> Cleaning"
	@env go clean
	rm -rf bin go.sum

unittest:
	@echo "==> Running tests"
	@env go test -v -count 1 -skip='^TestSanity$$' ./...

sanity-compile:
	@echo "==> Compiling csi-test v5 sanity suites"
	@env go test ./test/sanity/... -run='^$$' -count=1

sanity:
	@echo "==> Running sanity functional tests"
	@env go test -timeout=2h -v ./test/sanity/...

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

sbom-release:
	@command -v syft >/dev/null 2>&1 || { echo "ERROR: syft is required"; exit 1; }
	@syft "${RELEASE_IMAGE}" -o spdx-json="hammerspace-csi-${VERSION}.spdx.json"

sign-release:
	@command -v cosign >/dev/null 2>&1 || { echo "ERROR: cosign is required"; exit 1; }
	@cosign sign --yes "${RELEASE_IMAGE}"

verify-release:
	@$(MAKE) --no-print-directory scan-release
	@$(MAKE) --no-print-directory sbom-release
