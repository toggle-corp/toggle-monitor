BIN_DIR        := bin
BINARY         := $(BIN_DIR)/toggle-monitor
TAILWIND       := $(BIN_DIR)/tailwindcss
TAILWIND_INPUT := internal/web/tailwind/input.css
TAILWIND_CFG   := internal/web/tailwind/tailwind.config.js
TAILWIND_OUT   := internal/web/static/css/app.css
GOLANGCI_LINT  := $(BIN_DIR)/golangci-lint
TEMPL          := $(BIN_DIR)/templ

GO             ?= go

.PHONY: help build test lint templ tailwind tools clean

help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/{printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Compile the binary into bin/
	$(GO) build -o $(BINARY) ./cmd/toggle-monitor

test: ## Run unit tests (fast, no Docker required)
	$(GO) test ./...

test-integration: ## Run integration tests (requires Docker; spins up Postgres via testcontainers)
	$(GO) test -tags=integration ./...

lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

templ: $(TEMPL) ## Generate Go from .templ sources
	$(TEMPL) generate

tailwind: $(TAILWIND) ## Compile Tailwind CSS into the embedded asset path
	$(TAILWIND) -c $(TAILWIND_CFG) -i $(TAILWIND_INPUT) -o $(TAILWIND_OUT) --minify

tools: $(GOLANGCI_LINT) $(TEMPL) $(TAILWIND) ## Install local toolchain into bin/

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

$(TEMPL):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install github.com/a-h/templ/cmd/templ@latest

$(TAILWIND):
	scripts/tailwindcss.sh

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

# --- Local development (docker-compose) -------------------------------

COMPOSE       := docker compose -f deploy/local/docker-compose.yaml
COMPOSE_WATCH := docker compose -f deploy/local/docker-compose.yaml -f deploy/local/docker-compose.dev.yaml

.PHONY: dev-up dev-down dev-clean dev-logs dev-restart-app
.PHONY: dev-watch-up dev-watch-down dev-watch-logs

dev-up: ## Bring up the production-like local stack (built image, no autoreload)
	$(COMPOSE) up --build -d
	@echo
	@echo "UI:        http://localhost:8080"
	@echo "Metrics:   http://localhost:8080/metrics"
	@echo "Postgres:  localhost:5432 (user toggle_monitor)"

dev-down: ## Stop the local-dev stack (keeps the postgres volume)
	$(COMPOSE) down

dev-clean: ## Stop, drop the postgres volume, drop go module/build caches
	$(COMPOSE_WATCH) down -v

dev-logs: ## Tail app logs (production-like stack)
	$(COMPOSE) logs -f app

dev-restart-app: ## Rebuild + restart only the app after a config edit
	$(COMPOSE) up --build -d app

dev-watch-up: ## Bring up the dev stack with autoreload (air watches .go + .templ)
	$(COMPOSE_WATCH) up --build -d
	@echo
	@echo "UI:        http://localhost:8080   (autoreloads on source change)"
	@echo "Metrics:   http://localhost:8080/metrics"
	@echo "Postgres:  localhost:5432 (user toggle_monitor)"
	@echo
	@echo "  make dev-watch-logs   # follow air + binary output"

dev-watch-down: ## Stop the dev (autoreload) stack
	$(COMPOSE_WATCH) down

dev-watch-logs: ## Tail air output + binary logs from the dev stack
	$(COMPOSE_WATCH) logs -f app
