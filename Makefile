# Build and test targets for the Virtual Desktop project.
#
# `make build-all` is the release build: it cross-compiles every binary for
# every supported OS/arch pair and names each artifact with the correct
# platform extension (.exe on Windows, none on Linux/macOS) via
# scripts/build.sh. `make build` builds for the host platform only.

BINARIES := vdhost vdclient e2e captest rt
DIST_DIR := dist

.PHONY: build build-all checksums test vet clean

build: ## Build all binaries for the host OS/arch (no cross-compilation)
	@mkdir -p $(DIST_DIR)
	@ext=""; \
	if [ "$$(go env GOOS)" = "windows" ]; then ext=".exe"; fi; \
	for bin in $(BINARIES); do \
		echo "building $$bin$$ext"; \
		go build -o $(DIST_DIR)/$$bin$$ext ./cmd/$$bin/ || exit 1; \
	done

build-all: ## Cross-compile all binaries for every supported OS/arch (release build)
	./scripts/build.sh

checksums: ## Regenerate SHA256SUMS for dist/ artifacts
	cd $(DIST_DIR) && sha256sum -- * > SHA256SUMS

test: ## Run the full test suite
	go test ./...

vet: ## Run go vet
	go vet ./...

clean: ## Remove build artifacts
	rm -rf $(DIST_DIR)
