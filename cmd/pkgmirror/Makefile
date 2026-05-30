# cmd/pkgmirror/Makefile
#
# Focused commands for working inside cmd/pkgmirror/. The repo-root
# Makefile has the full set (cross-compile matrix, blackbox suites,
# CSS toolchain, etc); this file mirrors only the day-to-day verbs
# you reach for while iterating on the server binary itself.
#
# Defaults:
#   - the binary builds to ./pkgmirror (already in .gitignore)
#   - runtime data goes to ./data (also gitignored)
#   - `make run` boots with a fixed dev admin token so you don't have
#     to copy it from the log on every restart
#   - `make dev` adds PKGMIRROR_CONSOLE_DEV_DIR so template + CSS
#     edits hot-reload without rebuilding

BINARY      ?= pkgmirror
DATA_DIR    ?= ./data
ADDR        ?= 127.0.0.1:8080
ADMIN_TOKEN ?= pkm_dev_admin_token_change_me_for_real_use

# Path to the console source tree, used by `make dev` for the embed-FS
# override that hot-reloads templates + static assets without a rebuild.
# Resolved at make-time so the env var ends up absolute regardless of cwd.
REPO_ROOT     := $(realpath ../..)
CONSOLE_DEV   := $(REPO_ROOT)/internal/console

.DEFAULT_GOAL := help

.PHONY: help build run dev clean gen-keys

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "; printf "Usage: make <target>\n\nTargets:\n"} \
	      /^[a-zA-Z0-9_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' \
	      $(MAKEFILE_LIST)

build: ## Build ./pkgmirror for the host OS/arch.
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) .

run: build ## Build + run with dev defaults (ephemeral session/CSRF keys, fixed admin token).
	@printf '\n==> Booting pkgmirror on http://%s\n' "$(ADDR)"
	@printf '    Admin token: %s\n' "$(ADMIN_TOKEN)"
	@printf '    Web console: http://%s/console/\n' "$(ADDR)"
	@printf '    Data dir:    %s\n\n' "$(DATA_DIR)"
	PKGMIRROR_ADDR=$(ADDR) \
	PKGMIRROR_DATA_DIR=$(DATA_DIR) \
	PKGMIRROR_ADMIN_TOKEN=$(ADMIN_TOKEN) \
	./$(BINARY)

dev: build ## Like `run` but also enables template + static-asset hot reload.
	@printf '\n==> Booting pkgmirror (dev mode, hot-reload) on http://%s\n' "$(ADDR)"
	@printf '    Admin token: %s\n' "$(ADMIN_TOKEN)"
	@printf '    Web console: http://%s/console/  (edit-and-refresh on .tmpl + static/*)\n' "$(ADDR)"
	@printf '    Data dir:    %s\n\n' "$(DATA_DIR)"
	PKGMIRROR_ADDR=$(ADDR) \
	PKGMIRROR_DATA_DIR=$(DATA_DIR) \
	PKGMIRROR_ADMIN_TOKEN=$(ADMIN_TOKEN) \
	PKGMIRROR_CONSOLE_DEV_DIR=$(CONSOLE_DEV) \
	./$(BINARY)

clean: ## Remove the built binary and the local data dir.
	rm -f $(BINARY)
	rm -rf $(DATA_DIR)

gen-keys: ## Print fresh hex-encoded session + CSRF keys (also in root Makefile).
	@printf 'PKGMIRROR_SESSION_AUTH_KEY=%s\n' "$$(openssl rand -hex 64)"
	@printf 'PKGMIRROR_SESSION_ENC_KEY=%s\n'  "$$(openssl rand -hex 32)"
	@printf 'PKGMIRROR_CSRF_KEY=%s\n'         "$$(openssl rand -hex 32)"
