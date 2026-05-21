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
