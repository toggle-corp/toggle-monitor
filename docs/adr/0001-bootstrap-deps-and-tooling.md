# ADR 0001 — Bootstrap dependencies and tooling

**Status:** Accepted (Issue 1 of [`issues-v1.md`](../issues-v1.md))
**Date:** 2026-05-21

## Context

toggle-monitor v1 ships as a single static Go binary that runs as a
single replica inside a Kubernetes cluster (see
[`prd-v1.md`](../prd-v1.md) and
[`design-decisions.md`](../design-decisions.md)). Issue 1 is the
foundation slice — its job is to lock the module path, dependency set,
tooling choices, and CI surface so every later vertical slice builds on
a stable substrate.

This ADR records the choices made during bootstrap and the rationale.
Decisions made elsewhere (Postgres via CNPG, templ + HTMX + Tailwind UI,
golang-migrate as a PreSync hook, slog JSON logging, Prometheus
metrics, the auto-discovery model, the alert state machine) are not
duplicated here — they live in the design docs. This ADR is about *how
the binary is built*, not *what the binary does*.

## Decision

### Module path: `github.com/toggle-corp/toggle-monitor`

The repo currently lives on local gitea but will move to GitHub later.
Go module paths are local string identifiers for internal imports —
they only need to resolve over the network when an external consumer
runs `go get`. For a single-application binary with no library
consumers, using the eventual GitHub URL now costs nothing on gitea and
saves a rename later.

### Go toolchain: 1.24 (latest stable)

Latest stable as of bootstrap. Pinned via `go 1.24` in `go.mod`; the
local environment may be newer (1.26.3 at bootstrap time) — Go's
forward-compatible toolchain handles that. CI is locked to 1.24 to
guarantee reproducibility.

### CLI library: `spf13/cobra`

Considered alternatives: stdlib `flag`.

Rationale:
- Multi-subcommand binary surface (`serve`, `validate`, `config show`,
  `migrate`, `migrate --check`) — cobra is the idiomatic choice for
  this shape in Go (kubectl, helm, hugo, gh all use it).
- Built-in `--help`, autocompletion, nested subcommand support
  (`config show` is a subcommand under `config`).
- Cost: one direct dep plus a small transitive tree (`pflag`,
  `mousetrap`). Acceptable for the leverage it provides.

If we later regret it, switching to stdlib `flag` is a self-contained
refactor in `cmd/toggle-monitor/internal/cli/`.

### Postgres driver: `jackc/pgx/v5`

Considered alternatives: `lib/pq` via `database/sql`.

Rationale:
- pgx is the modern Postgres driver for Go. Better performance, richer
  protocol support, native PostgreSQL types, first-class pgx-specific
  API for advanced needs (notifications, batch, COPY).
- Plays well with `golang-migrate/v4/database/pgx/v5` for the migration
  runner.
- The `database/sql` standard interface is still available if needed,
  but pgx's native API is what we'll typically use.

### Schema migrations: `golang-migrate/migrate/v4` (library form)

Driven from the `toggle-monitor migrate` subcommand over an embedded
SQL set (`//go:embed internal/migrate/migrations/*`). This matches the
design decision to run migrations as an ArgoCD PreSync hook job so
operators can view migration logs in the ArgoCD UI before the new app
pod rolls out.

### Kubernetes client: `k8s.io/client-go`

The canonical client. Used by the ingress informer (Issue 8) over the
`networking.k8s.io/v1` API only.

### UI stack: `a-h/templ` + HTMX + precompiled Tailwind

Locked decision per [`prd-v1.md`](../prd-v1.md). This ADR records the
build-time toolchain:

- **templ** for type-safe Go-native templates with codegen via
  `go generate`. `templ generate` is invoked by `make templ`. The
  generated `_templ.go` files are checked in.
- **HTMX** loaded as a small static asset; no SPA build.
- **Tailwind via the standalone CLI binary** — `scripts/tailwindcss.sh`
  downloads the platform-specific binary into `bin/tailwindcss`. No
  Node, no npm, no `package.json`. The Makefile's `tailwind` target
  compiles `internal/web/tailwind/input.css` into
  `internal/web/static/css/app.css`, which is then embedded via
  `embed.FS`.

The Tailwind output CSS is checked in so consumers can build the
project without running the Tailwind step.

### Observability: `prometheus/client_golang` + stdlib `slog`

Direct deps for the Prometheus metrics endpoint and Go runtime
metrics; `slog` is stdlib so no external dep needed. Specific series
land in Issue 14.

### Config parser: `gopkg.in/yaml.v3`

`yaml.v3` for line-numbered errors (via node positions) and modern
YAML 1.2 features. Required for the multi-error reporting behavior in
Issue 5.

### Linter: `golangci-lint` with a curated rule set

Enabled: `errcheck`, `govet`, `staticcheck`, `revive`, `gosec`,
`gocritic`, `misspell`, `unused`, `unparam`, `ineffassign`. Formatters:
`gofmt`, `goimports`. Test files relax the `gosec`, `gocritic`,
`revive` rules.

Considered alternative: bare `go vet` + `staticcheck`. Rejected because
the gap (catching dead code, ineffective assignments, security
patterns, error-handling slips) is real and `golangci-lint` adds it
for one tool install.

### Build/CI: GNU Make + GitHub Actions

- **Make** for the local task interface (`build`, `test`, `lint`,
  `templ`, `tailwind`, `tools`, `clean`). Considered alternative
  Taskfile; rejected because Make is universally present and the
  Makefile is small.
- **GitHub Actions** for CI. Workflow runs build + test + lint on PRs
  and pushes to `main`. Cache is keyed on `go.sum`.

### Repository layout

```
cmd/toggle-monitor/                # main package + cobra CLI
  main.go
  internal/cli/                    # subcommand definitions
internal/                          # per-module packages
  config/ slug/ secret/ db/ migrate/ store/
  kube/ merger/ scheduler/ httpcheck/ sslinspect/ alert/
  slack/ heartbeat/ web/ lifecycle/ observability/
internal/migrate/migrations/       # //go:embed source
internal/web/static/css/           # //go:embed source (Tailwind output)
internal/web/tailwind/              # Tailwind config + input.css
internal/web/templates/            # .templ sources + generated _templ.go
scripts/tailwindcss.sh             # downloads platform-specific CLI
docs/                              # PRD, design decisions, schema, ADRs
```

The 17 module directories from `prd-v1.md` are all present from day
one (some empty); later issues fill them in.

## Consequences

- New PRs against the repo will be expected to honor the
  `golangci-lint` rule set; failures block CI.
- Adding a new dependency requires updating either an existing stub or
  introducing the dep in the first issue that actually uses it. Blank
  imports were used during bootstrap to lock deps in `go.mod` ahead of
  real use; later issues remove the blank import as they wire the
  package for real.
- The `bin/` directory holds local tools (`tailwindcss`,
  `golangci-lint`, `templ`) and the compiled binary. It is
  `.gitignore`d.
- The Tailwind compiled CSS (`internal/web/static/css/app.css`) IS
  checked in so a fresh clone can build without first running
  `make tailwind`. This is reconciled by the `make tailwind` step
  before any UI work.
- Switching CLI library, linter, or build tool later is each a
  self-contained refactor — none of them are cross-cutting concerns.

## References

- [`docs/issues-v1.md`](../issues-v1.md) — Issue 1 (bootstrap) and
  Issue 2 (tracer bullet) acceptance criteria.
- [`docs/prd-v1.md`](../prd-v1.md) — v1 PRD.
- [`docs/design-decisions.md`](../design-decisions.md) — locked
  design decisions.
