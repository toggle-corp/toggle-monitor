#!/usr/bin/env just --justfile
# Task runner for toggle-monitor. Install just:
#   https://github.com/casey/just#installation
#
# Run `just` to list every recipe.

# --- Settings ---------------------------------------------------------

set shell := ["bash", "-cu"]
set positional-arguments

bin_dir        := "bin"
binary         := bin_dir / "toggle-monitor"
tailwind_bin   := bin_dir / "tailwindcss"
tailwind_in    := "internal/web/tailwind/input.css"
tailwind_cfg   := "internal/web/tailwind/tailwind.config.js"
tailwind_out   := "internal/web/static/css/app.css"
golangci_lint  := bin_dir / "golangci-lint"
# Pinned to the version .github/workflows/ci.yml installs, so a local
# `just lint` and CI agree on which checks run.
golangci_ver   := "v2.12.2"
templ_bin      := bin_dir / "templ"

go             := env_var_or_default("GO", "go")

compose        := "docker compose -f deploy/local/docker-compose.yaml"
compose_watch  := "docker compose -f deploy/local/docker-compose.yaml -f deploy/local/docker-compose.dev.yaml"

local_config        := "deploy/local/config.yaml"
local_config_sample := "deploy/local/config.sample.yaml"

# Default recipe: list all recipes.
default:
    @just --list

# --- Build / test / lint ---------------------------------------------

# Compile the binary into bin/. Stamps the git-derived version into
# main.version (used as the Sentry release tag on every event).
build:
    @VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"; \
        {{go}} build -ldflags "-X main.version=$VERSION" -o {{binary}} ./cmd/toggle-monitor

# Run unit tests (fast, no Docker required).
test: test-release-hook
    {{go}} test ./...

# Verify release.sh's RELEASE_CUSTOM_HOOK bumps Chart.yaml under the
# '# managed by release.sh' marker contract. Cheap (~1s) — keeps the
# release path from drifting between release runs.
test-release-hook:
    bash scripts/test-release-hook.sh

# Run integration tests (requires Docker; spins up Postgres via testcontainers).
test-integration:
    {{go}} test -tags=integration ./...

# Run golangci-lint over the whole tree.
#
# staticcheck analyses the stdlib source of whichever Go toolchain is
# active, so a patched toolchain can change the findings. Custom builds
# (e.g. `go1.26.5-X:nodwarf5`) lose the `t.Fatal` -> `FailNow` ->
# `runtime.Goexit` termination fact and report SA5011 "possible nil
# pointer dereference" on every `if x == nil { t.Fatal(...) }` guard in
# the test suite. Those are false positives; CI runs an official
# toolchain and sees none. Reproduce CI locally with
# `GOTOOLCHAIN=go1.26.5 just lint`.
lint: install-golangci-lint
    {{golangci_lint}} run ./...

# Generate Go from .templ sources.
templ: install-templ
    {{templ_bin}} generate

# Compile Tailwind CSS into the embedded asset path.
tailwind: install-tailwind
    {{tailwind_bin}} -c {{tailwind_cfg}} -i {{tailwind_in}} -o {{tailwind_out}} --minify

# Re-vendor the self-hosted woff2 faces. The files are committed, so this
# only needs running to pick up an upstream font release.
fonts:
    scripts/fonts.sh --force

# Install every local tool into bin/.
tools: install-golangci-lint install-templ install-tailwind

# Remove build artifacts.
clean:
    rm -rf {{bin_dir}}

# --- Tool installers (run on demand by the recipes above) -------------

install-golangci-lint:
    @if [ ! -x "{{golangci_lint}}" ] || ! {{golangci_lint}} --version | grep -q "{{ replace(golangci_ver, 'v', '') }}"; then \
        mkdir -p {{bin_dir}}; \
        GOBIN="$PWD/{{bin_dir}}" {{go}} install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{golangci_ver}}; \
    fi

install-templ:
    @if [ ! -x "{{templ_bin}}" ]; then \
        mkdir -p {{bin_dir}}; \
        GOBIN="$PWD/{{bin_dir}}" {{go}} install github.com/a-h/templ/cmd/templ@latest; \
    fi

install-tailwind:
    @if [ ! -x "{{tailwind_bin}}" ]; then \
        scripts/tailwindcss.sh; \
    fi

# --- Local development (docker-compose) ------------------------------

# Bootstrap deploy/local/config.yaml from the gitignored sample if it
# doesn't exist yet. Idempotent — only copies the first time.
_local-config:
    @if [ ! -f {{local_config}} ]; then \
        cp {{local_config_sample}} {{local_config}}; \
        echo "Bootstrapped {{local_config}} from {{local_config_sample}}. Edit it freely; it's gitignored."; \
    fi

# Validate the local config (or a path passed as the first argument).
# Wraps the binary's `validate` subcommand so YAML errors surface with
# line numbers before you spin the stack up. Output is colorized when
# stdout is a TTY and NO_COLOR is unset.
validate-config *args: build _local-config
    @path="${1:-{{local_config}}}"; \
    if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then \
        B=$'\e[1m'; DIM=$'\e[2m'; GREEN=$'\e[32m'; RED=$'\e[31m'; YELLOW=$'\e[33m'; RESET=$'\e[0m'; \
    else B=""; DIM=""; GREEN=""; RED=""; YELLOW=""; RESET=""; fi; \
    printf "%svalidating%s %s%s%s\n" "$B" "$RESET" "$DIM" "$path" "$RESET"; \
    if out=$({{binary}} validate "$path" 2>&1); then \
        printf "%s✓ ok%s — config is valid\n" "$GREEN$B" "$RESET"; \
        [ -n "$out" ] && printf "%s%s%s\n" "$DIM" "$out" "$RESET"; \
        exit 0; \
    else \
        rc=$?; \
        printf "%s✗ invalid%s\n" "$RED$B" "$RESET"; \
        printf "%s%s%s\n" "$YELLOW" "$out" "$RESET"; \
        exit $rc; \
    fi

# Bring up the production-like local stack (built image, no autoreload).
dev-up: _local-config
    {{compose}} up --build -d
    @echo
    @echo "UI:        http://localhost:8080"
    @echo "Metrics:   http://localhost:8080/metrics"
    @echo "Postgres:  localhost:5432 (user toggle_monitor)"

# Stop the local-dev stack (keeps the postgres volume).
dev-down:
    {{compose}} down

# Stop, drop the postgres volume, drop go module/build caches.
dev-clean:
    {{compose_watch}} down -v

# Tail app logs (production-like stack).
dev-logs:
    {{compose}} logs -f app

# Apply pending schema migrations against the local-stack Postgres.
# Runs the one-shot migrate service from docker-compose with --build
# so the image picks up the latest source. Use after pulling a change
# that bumped the schema and the app refuses to start.
migrate: _local-config
    {{compose}} up --build --exit-code-from migrate migrate

# Check the schema is at the latest version without applying anything.
migrate-check: _local-config
    {{compose}} run --rm --build migrate migrate --config /etc/toggle-monitor/config.yaml --check

# Rebuild + restart only the app after a config edit.
dev-restart-app:
    {{compose}} up --build -d app

# Bring up the dev stack with autoreload (air watches .go + .templ).
dev-watch-up: _local-config
    {{compose_watch}} up --build -d
    @echo
    @echo "UI:        http://localhost:8080   (autoreloads on source change)"
    @echo "Metrics:   http://localhost:8080/metrics"
    @echo "Postgres:  localhost:5432 (user toggle_monitor)"
    @echo
    @echo "  just dev-watch-logs   # follow air + binary output"

# Stop the dev (autoreload) stack.
dev-watch-down:
    {{compose_watch}} down

# Tail air output + binary logs from the dev stack.
dev-watch-logs:
    {{compose_watch}} logs -f app
