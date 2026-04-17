BINARY := maquinista
BUILD_DIR := ./cmd/maquinista
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

# Dashboard (Next.js) project. See plans/active/dashboard.md.
DASHBOARD_WEB_DIR  := internal/dashboard/web
DASHBOARD_NODE_MODULES := $(DASHBOARD_WEB_DIR)/node_modules
DASHBOARD_STANDALONE_TGZ := internal/dashboard/standalone.tgz

# Every web source file the standalone bundle depends on. If any of
# these is newer than the tarball, `make build` re-packages; otherwise
# the tarball is left alone and the Go binary just re-links.
DASHBOARD_WEB_SRC := \
  $(shell find $(DASHBOARD_WEB_DIR)/src -type f 2>/dev/null) \
  $(shell find $(DASHBOARD_WEB_DIR)/public -type f 2>/dev/null) \
  $(DASHBOARD_WEB_DIR)/package.json \
  $(DASHBOARD_WEB_DIR)/package-lock.json \
  $(DASHBOARD_WEB_DIR)/next.config.ts \
  $(DASHBOARD_WEB_DIR)/tsconfig.json \
  $(DASHBOARD_WEB_DIR)/postcss.config.mjs \
  $(DASHBOARD_WEB_DIR)/components.json

# Every Go source file the binary depends on. This keeps `make build`
# a true no-op when nothing changed and avoids false relinks when
# only tests under testdata/ touched.
GO_SRC := $(shell find . -type f -name '*.go' -not -path './$(DASHBOARD_WEB_DIR)/*' 2>/dev/null)

# Skip the Next.js pipeline entirely (operator opt-out — the committed
# placeholder tarball at $(DASHBOARD_STANDALONE_TGZ) is used instead,
# and `dashboard start` falls back to the Node healthcheck stub).
#   make build SKIP_DASHBOARD=1
SKIP_DASHBOARD ?=

.PHONY: build build-go test vet clean \
        dashboard-test dashboard-web-dev dashboard-web-test \
        dashboard-e2e dashboard-e2e-install

# Default target: one-shot fresh build. Re-runs npm install only when
# package.json changed; re-packages the standalone tarball only when
# any web source changed; re-links the Go binary only when any .go
# changed. Zero wasted work on a no-op rebuild.
build: $(BINARY)

$(BINARY): $(GO_SRC) $(DASHBOARD_STANDALONE_TGZ)
	go build $(LDFLAGS) -o $(BINARY) $(BUILD_DIR)

# build-go skips the web pipeline (alias for SKIP_DASHBOARD=1).
# Useful for quick Go-only iteration; produces a binary with the
# currently-committed tarball (real or placeholder).
build-go:
	go build $(LDFLAGS) -o $(BINARY) $(BUILD_DIR)

# Tarball rule. Depends on every web source; any edit triggers a
# rebuild + repackage on the next `make build`.
$(DASHBOARD_STANDALONE_TGZ): $(DASHBOARD_NODE_MODULES) $(DASHBOARD_WEB_SRC)
ifeq ($(SKIP_DASHBOARD),)
	@echo ">>> building Next.js standalone bundle"
	@cd $(DASHBOARD_WEB_DIR) && npm run build --silent
	@if [ ! -d $(DASHBOARD_WEB_DIR)/.next/standalone ]; then \
		echo "error: $(DASHBOARD_WEB_DIR)/.next/standalone missing after build"; \
		exit 1; \
	fi
	@# Next standalone mode doesn't copy these — per next docs.
	@if [ -d $(DASHBOARD_WEB_DIR)/public ]; then \
		rm -rf $(DASHBOARD_WEB_DIR)/.next/standalone/public && \
		cp -r $(DASHBOARD_WEB_DIR)/public $(DASHBOARD_WEB_DIR)/.next/standalone/public; \
	fi
	@mkdir -p $(DASHBOARD_WEB_DIR)/.next/standalone/.next
	@rm -rf $(DASHBOARD_WEB_DIR)/.next/standalone/.next/static
	@cp -r $(DASHBOARD_WEB_DIR)/.next/static $(DASHBOARD_WEB_DIR)/.next/standalone/.next/static
	@tar -czf $@ -C $(DASHBOARD_WEB_DIR)/.next/standalone .
	@echo ">>> packaged $@ ($$(du -h $@ | cut -f1))"
else
	@echo ">>> SKIP_DASHBOARD set — leaving $(DASHBOARD_STANDALONE_TGZ) untouched"
	@touch $@
endif

# npm install guard. A sentinel file under node_modules tracks whether
# we've already run `npm install` for the current package-lock.json.
$(DASHBOARD_NODE_MODULES): $(DASHBOARD_WEB_DIR)/package-lock.json $(DASHBOARD_WEB_DIR)/package.json
ifeq ($(SKIP_DASHBOARD),)
	@if ! command -v npm >/dev/null 2>&1; then \
		echo "error: npm not on PATH. Install Node 22+ or rerun with SKIP_DASHBOARD=1."; \
		exit 1; \
	fi
	@echo ">>> npm install in $(DASHBOARD_WEB_DIR)"
	@cd $(DASHBOARD_WEB_DIR) && npm install --silent
	@touch $@
else
	@echo ">>> SKIP_DASHBOARD set — skipping npm install"
	@mkdir -p $@ && touch $@
endif

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
	rm -rf $(DASHBOARD_WEB_DIR)/.next $(DASHBOARD_NODE_MODULES) \
	       $(DASHBOARD_WEB_DIR)/playwright-report \
	       $(DASHBOARD_WEB_DIR)/test-results

# --- dashboard targets -----------------------------------------------------

# Go-side dashboard tests (supervisor, config, CLI, integration).
dashboard-test:
	go test -race ./cmd/maquinista/ ./internal/dashboard/ ./internal/config/ -timeout 120s

# Run the Next.js dev server (HMR). Not part of `make build`.
dashboard-web-dev: $(DASHBOARD_NODE_MODULES)
	cd $(DASHBOARD_WEB_DIR) && npm run dev

# Vitest unit / component tests.
dashboard-web-test: $(DASHBOARD_NODE_MODULES)
	cd $(DASHBOARD_WEB_DIR) && npm test

# Playwright end-to-end. Boots Postgres + maquinista + browser, drives
# real user journeys. Requires Playwright browser binaries; install
# once with `make dashboard-e2e-install` (sudo).
dashboard-e2e: $(DASHBOARD_NODE_MODULES) $(DASHBOARD_STANDALONE_TGZ)
	cd $(DASHBOARD_WEB_DIR) && npx playwright test

# Install Playwright's browser binaries + OS libs (requires sudo).
dashboard-e2e-install:
	cd $(DASHBOARD_WEB_DIR) && npx playwright install --with-deps
