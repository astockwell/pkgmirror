# pkgmirror Makefile
#
# Run `make help` for a one-line summary of every target.
# Most targets are pure Go (no docker required); test-blackbox and the
# `test-blackbox-*` per-format targets need a working docker daemon.

# ---- Configuration -----------------------------------------------------------

# Use the project-local go.mod toolchain (Go 1.26+) for everything.
GO         ?= go
BIN_DIR    ?= ./bin
BINARY     ?= pkgmirror
PKG_MAIN   ?= ./cmd/pkgmirror
DIST_DIR   ?= ./dist

# Strip the symbol + DWARF tables to keep binaries small; -trimpath makes
# builds reproducible across machines.
GO_LDFLAGS ?= -s -w
GO_FLAGS   ?= -trimpath -ldflags='$(GO_LDFLAGS)'

# Cross-compile matrix.
CROSS_PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64

# Black-box test timeout. Big enough for image pulls on a cold cache;
# override on the command line if you're iterating: `make test-blackbox BB_TIMEOUT=2m`.
BB_TIMEOUT ?= 15m

.DEFAULT_GOAL := help
.PHONY: help

# ---- Help --------------------------------------------------------------------

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "; printf "Usage: make <target>\n\nTargets:\n"} \
	      /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' \
	      $(MAKEFILE_LIST)

# ---- Build -------------------------------------------------------------------

.PHONY: build build-all build-linux build-linux-amd64 build-linux-arm64 \
        build-darwin build-darwin-amd64 build-darwin-arm64

build: ## Build the binary for the host OS/arch into ./bin.
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GO_FLAGS) -o $(BIN_DIR)/$(BINARY) $(PKG_MAIN)

build-linux: build-linux-amd64 build-linux-arm64 ## Build linux/amd64 + linux/arm64.

build-darwin: build-darwin-amd64 build-darwin-arm64 ## Build darwin/amd64 + darwin/arm64.

build-all: $(addprefix build-,$(subst /,-,$(CROSS_PLATFORMS))) ## Build every target in the cross-compile matrix.

build-linux-amd64: ## Cross-compile linux/amd64 into ./dist.
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 $(GO) build $(GO_FLAGS) -o $(DIST_DIR)/$(BINARY)-linux-amd64 $(PKG_MAIN)

build-linux-arm64: ## Cross-compile linux/arm64 into ./dist.
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 $(GO) build $(GO_FLAGS) -o $(DIST_DIR)/$(BINARY)-linux-arm64 $(PKG_MAIN)

build-darwin-amd64: ## Cross-compile darwin/amd64 into ./dist.
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build $(GO_FLAGS) -o $(DIST_DIR)/$(BINARY)-darwin-amd64 $(PKG_MAIN)

build-darwin-arm64: ## Cross-compile darwin/arm64 into ./dist.
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build $(GO_FLAGS) -o $(DIST_DIR)/$(BINARY)-darwin-arm64 $(PKG_MAIN)

# ---- Run ---------------------------------------------------------------------

.PHONY: run run-public

run: ## Run pkgmirror from source against ./data.
	$(GO) run $(PKG_MAIN)

run-public: ## Run with the default tenant set to public (useful for quick demos).
	PKGMIRROR_DEFAULT_TENANT_VISIBILITY=public $(GO) run $(PKG_MAIN)

# ---- Console assets ----------------------------------------------------------
#
# The committed bundle at internal/console/static/css/console.css is
# what gets embedded. `make build-css` is only needed when you edit
# input.css or add a Tailwind class to a console template.
#
# `tools/tailwindcss/install.sh` pins an exact upstream version and
# verifies the binary's SHA-256 before installing. On a machine without
# the binary, `make build-css` will run the installer first.

TAILWIND_BIN ?= $(BIN_DIR)/tailwindcss
CONSOLE_CSS_IN  := internal/console/static/css/input.css
CONSOLE_CSS_OUT := internal/console/static/css/console.css
CONSOLE_TEMPLATES := $(shell find internal/console -name '*.tmpl' 2>/dev/null)

.PHONY: install-tailwind build-css watch-css gen-keys

install-tailwind: ## Install the pinned Tailwind standalone CLI into ./bin.
	./tools/tailwindcss/install.sh

$(TAILWIND_BIN):
	./tools/tailwindcss/install.sh

build-css: $(TAILWIND_BIN) $(CONSOLE_CSS_IN) $(CONSOLE_TEMPLATES) ## Build console CSS from input.css.
	$(TAILWIND_BIN) -i $(CONSOLE_CSS_IN) -o $(CONSOLE_CSS_OUT) --minify

watch-css: $(TAILWIND_BIN) ## Rebuild console CSS on template changes (dev loop).
	$(TAILWIND_BIN) -i $(CONSOLE_CSS_IN) -o $(CONSOLE_CSS_OUT) --watch

gen-keys: ## Print fresh PKGMIRROR_SESSION_AUTH_KEY / _ENC_KEY / CSRF_KEY (hex).
	@printf 'PKGMIRROR_SESSION_AUTH_KEY=%s\n' "$$(openssl rand -hex 64)"
	@printf 'PKGMIRROR_SESSION_ENC_KEY=%s\n'  "$$(openssl rand -hex 32)"
	@printf 'PKGMIRROR_CSRF_KEY=%s\n'         "$$(openssl rand -hex 32)"


# ---- Test --------------------------------------------------------------------

.PHONY: test test-race test-cover test-all \
        test-blackbox test-blackbox-go test-blackbox-pypi \
        test-blackbox-npm test-blackbox-rubygems test-blackbox-container \
        test-blackbox-generic test-blackbox-alpine test-blackbox-maven \
        test-blackbox-debian test-blackbox-rpm test-blackbox-nuget test-blackbox-cran

test: ## Fast unit + grey-box tests (no docker).
	$(GO) test ./...

test-race: ## Unit tests with the race detector enabled.
	$(GO) test -race ./...

test-cover: ## Unit tests with coverage; writes coverage.out and prints a summary.
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

test-blackbox: ## Full black-box conformance suite (needs docker).
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/...

test-blackbox-go: ## Go module proxy conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/goproxy/...

test-blackbox-pypi: ## PyPI conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/pypi/...

test-blackbox-npm: ## npm conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/npm/...

test-blackbox-rubygems: ## RubyGems conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/rubygems/...

test-blackbox-container: ## Container / OCI conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/container/...

test-blackbox-generic: ## Generic format conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/generic/...

test-blackbox-alpine: ## Alpine (apk) conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/alpine/...

test-blackbox-maven: ## Maven conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/maven/...

test-blackbox-debian: ## Debian (apt) conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/debian/...

test-blackbox-rpm: ## RPM (dnf/yum) conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/rpm/...

test-blackbox-nuget: ## NuGet (V3) conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/nuget/...

test-blackbox-cran: ## CRAN (R) conformance only.
	$(GO) test -tags=blackbox -timeout=$(BB_TIMEOUT) ./tests/blackbox/cran/...

test-all: test test-blackbox ## Run unit + grey-box + black-box.

# ---- Lint / format -----------------------------------------------------------

.PHONY: lint vet fmt fmt-check tidy verify check-package-dupes fix-package-dupes install-hooks

vet: check-package-dupes ## Run go vet across all packages.
	$(GO) vet ./...

lint: vet ## Alias for vet; reserved for richer linters later.

fmt: ## Format every Go file with gofmt -s -w.
	gofmt -s -w .

fmt-check: ## Fail if any Go file is not gofmt'd (use in CI).
	@out=$$(gofmt -s -l . 2>&1); \
	if [ -n "$$out" ]; then \
		echo "gofmt: files need formatting:"; echo "$$out"; exit 1; \
	fi

check-package-dupes: ## Detect duplicate `package X` declarations (CI-safe; exit 1 on bad).
	@$(GO) run ./tools/dedup-package .

fix-package-dupes: ## Repair duplicate `package X` declarations in place.
	@$(GO) run ./tools/dedup-package -fix .

install-hooks: ## Symlink .githooks into .git/hooks so pre-commit runs locally.
	@mkdir -p .git/hooks
	@for h in .githooks/*; do \
		name=$$(basename "$$h"); \
		ln -sf "../../$$h" ".git/hooks/$$name"; \
		echo "installed: .git/hooks/$$name -> ../../$$h"; \
	done

tidy: ## go mod tidy.
	$(GO) mod tidy

verify: ## go mod verify — checks dependency module hashes against go.sum.
	$(GO) mod verify

# ---- Dependency management ---------------------------------------------------

.PHONY: deps-list deps-update deps-update-patch deps-vendor

deps-list: ## List direct dependencies with available updates (read-only).
	$(GO) list -m -u all

deps-update: ## Update every dependency to its latest minor/patch and run go mod tidy.
	$(GO) get -u ./...
	$(GO) mod tidy

deps-update-patch: ## Update every dependency to its latest patch only (safer than deps-update).
	$(GO) get -u=patch ./...
	$(GO) mod tidy

deps-vendor: ## Vendor dependencies into ./vendor for offline builds.
	$(GO) mod vendor

# ---- Docker ------------------------------------------------------------------

.PHONY: docker-build docker-run

docker-build: ## Build the docker image as pkgmirror:dev.
	docker build -t pkgmirror:dev .

docker-run: ## Run pkgmirror:dev with ./data mounted as the data dir on :8080.
	docker run --rm -p 8080:8080 -v $(PWD)/data:/data \
		-e PKGMIRROR_DATA_DIR=/data pkgmirror:dev

# ---- House-keeping -----------------------------------------------------------

.PHONY: clean distclean

clean: ## Remove build outputs (./bin, ./dist) and coverage files.
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out

distclean: clean ## clean + remove the local data dir (DESTRUCTIVE — wipes SQLite + blobs).
	rm -rf ./data
