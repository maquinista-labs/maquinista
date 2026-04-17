BINARY := maquinista
BUILD_DIR := ./cmd/maquinista
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

# Dashboard (Next.js) project. See plans/active/dashboard.md.
DASHBOARD_WEB_DIR := internal/dashboard/web

.PHONY: build test vet clean \
        dashboard-test \
        dashboard-web-install dashboard-web-dev dashboard-web-build \
        dashboard-web-package dashboard-web-test \
        dashboard-e2e dashboard-e2e-install

build:
	go build $(LDFLAGS) -o $(BINARY) $(BUILD_DIR)

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
	rm -rf $(DASHBOARD_WEB_DIR)/.next $(DASHBOARD_WEB_DIR)/dist $(DASHBOARD_WEB_DIR)/node_modules $(DASHBOARD_WEB_DIR)/playwright-report

# --- dashboard targets -----------------------------------------------------

# Go-side dashboard tests (supervisor, config, CLI, integration).
# Scoped so iterating on the dashboard doesn't pay the full repo
# test cost.
dashboard-test:
	go test -race ./cmd/maquinista/ ./internal/dashboard/ ./internal/config/ -timeout 120s

# Install npm deps for the Next.js project. Phase 1 Commit 1.1 creates
# the project; until then these targets no-op with a clear message.
dashboard-web-install:
	@if [ -f $(DASHBOARD_WEB_DIR)/package.json ]; then \
		cd $(DASHBOARD_WEB_DIR) && npm install; \
	else \
		echo "skip: $(DASHBOARD_WEB_DIR) not scaffolded yet (Phase 1 Commit 1.1)"; \
	fi

dashboard-web-dev:
	@if [ -f $(DASHBOARD_WEB_DIR)/package.json ]; then \
		cd $(DASHBOARD_WEB_DIR) && npm run dev; \
	else \
		echo "skip: $(DASHBOARD_WEB_DIR) not scaffolded yet (Phase 1 Commit 1.1)"; \
	fi

dashboard-web-build:
	@if [ -f $(DASHBOARD_WEB_DIR)/package.json ]; then \
		cd $(DASHBOARD_WEB_DIR) && npm run build; \
	else \
		echo "skip: $(DASHBOARD_WEB_DIR) not scaffolded yet (Phase 1 Commit 1.1)"; \
	fi

# Build the Next.js standalone bundle and tar it into
# internal/dashboard/standalone.tgz, which embed.go pulls in via
# //go:embed. Run this before `make build` for a release binary.
dashboard-web-package: dashboard-web-build
	@if [ ! -d $(DASHBOARD_WEB_DIR)/.next/standalone ]; then \
		echo "error: $(DASHBOARD_WEB_DIR)/.next/standalone missing after build"; \
		exit 1; \
	fi
	@# Copy public/ + .next/static/ into the standalone tree per the
	@# Next.js docs (standalone mode does not copy them automatically).
	@if [ -d $(DASHBOARD_WEB_DIR)/public ]; then \
		cp -r $(DASHBOARD_WEB_DIR)/public $(DASHBOARD_WEB_DIR)/.next/standalone/public; \
	fi
	@mkdir -p $(DASHBOARD_WEB_DIR)/.next/standalone/.next
	@cp -r $(DASHBOARD_WEB_DIR)/.next/static $(DASHBOARD_WEB_DIR)/.next/standalone/.next/static
	tar -czf internal/dashboard/standalone.tgz -C $(DASHBOARD_WEB_DIR)/.next/standalone .
	@echo "packaged: internal/dashboard/standalone.tgz ($$(du -h internal/dashboard/standalone.tgz | cut -f1))"

dashboard-web-test:
	@if [ -f $(DASHBOARD_WEB_DIR)/package.json ]; then \
		cd $(DASHBOARD_WEB_DIR) && npm test; \
	else \
		echo "skip: $(DASHBOARD_WEB_DIR) not scaffolded yet (Phase 1 Commit 1.1)"; \
	fi

# Playwright end-to-end. The global-setup hook builds the
# maquinista binary + Next standalone bundle and spawns
# `maquinista dashboard start` on an ephemeral port; specs drive
# the running dashboard. Requires Playwright browser binaries
# (~150 MiB) and their OS deps. On a fresh box:
#   cd $(DASHBOARD_WEB_DIR) && npx playwright install --with-deps chromium
dashboard-e2e:
	cd $(DASHBOARD_WEB_DIR) && npx playwright test

# Install Playwright's Chromium binary + OS libs (requires sudo).
# CI runs this once during image build.
dashboard-e2e-install:
	cd $(DASHBOARD_WEB_DIR) && npx playwright install --with-deps chromium
