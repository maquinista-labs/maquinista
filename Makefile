BINARY := maquinista
BUILD_DIR := ./cmd/maquinista
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

# Dashboard (Next.js) project. See plans/active/dashboard.md.
DASHBOARD_WEB_DIR := internal/dashboard/web

.PHONY: build test vet clean \
        dashboard-test \
        dashboard-web-install dashboard-web-dev dashboard-web-build \
        dashboard-web-test dashboard-e2e

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

dashboard-web-test:
	@if [ -f $(DASHBOARD_WEB_DIR)/package.json ]; then \
		cd $(DASHBOARD_WEB_DIR) && npm test; \
	else \
		echo "skip: $(DASHBOARD_WEB_DIR) not scaffolded yet (Phase 1 Commit 1.1)"; \
	fi

# Playwright end-to-end. Boots the Go binary against dbtest.PgContainer
# and drives real browser journeys. Requires Docker + Playwright
# browsers; install with: cd $(DASHBOARD_WEB_DIR) && npx playwright
# install --with-deps chromium.
dashboard-e2e:
	@if [ -f $(DASHBOARD_WEB_DIR)/playwright.config.ts ]; then \
		cd $(DASHBOARD_WEB_DIR) && npx playwright test; \
	else \
		echo "skip: Playwright not configured yet (Phase 1 Commit 1.7)"; \
	fi
