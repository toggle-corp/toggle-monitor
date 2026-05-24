# toggle-monitor

[![CI](https://github.com/toggle-corp/toggle-monitor/actions/workflows/ci.yml/badge.svg)](https://github.com/toggle-corp/toggle-monitor/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A single-binary uptime + SSL monitor that runs inside Kubernetes,
auto-discovers Ingress resources, and posts Block Kit alerts to Slack.

- **YAML-driven config in a ConfigMap.** Static endpoints declared
  by hand; in-cluster Ingresses opt in via a single annotation.
- **Slack lifecycle that doesn't spam.** One parent message per
  incident, a thread reply every `reminderInterval`, and a parent
  edit + thread reply on resolve. SSL is its own incident class on
  the same channel.
- **Operationally boring.** Single replica, Postgres via CNPG,
  Prometheus `/metrics`, slog JSON logs, `stakater/reloader`-driven
  restart on config change, outbound deadman heartbeat (any
  healthchecks.io-compatible URL works).
- **Read-only HTMX UI** with stats, paginated listings, per-monitor
  detail with provenance, and an auto-discovery view that explains
  why every Ingress did or did not materialize into a monitor.

> Status: **v1 feature-complete** as of `main`. Historical context — the
> v1 PRD, design log, and tracer-bullet issues — lives under
> [`docs/internal/`](docs/internal/); the day-to-day references are
> [`docs/architecture.md`](docs/architecture.md),
> [`docs/operations.md`](docs/operations.md),
> [`docs/config-schema.md`](docs/config-schema.md), and the ADRs under
> [`docs/adr/`](docs/adr/).

## Quick start

### Local development

```bash
cp deploy/local/.env.example deploy/local/.env   # optional; for Slack
just dev-watch-up                                # autoreload via air;
                                                  # bootstraps a personal
                                                  # config.yaml from the
                                                  # checked-in sample
open http://localhost:8080
just validate-config                             # sanity-check the YAML
```

This brings up Postgres + httpbin (as a probe target) + the
toggle-monitor binary with live reload. Detailed walkthrough +
teardown commands: **[deploy/local/README.md](deploy/local/README.md)**.

### Deploy to Kubernetes

```bash
# Render to review:
helm template my-toggle deploy/helm/toggle-monitor \
  --set postgres.passwordSecret.name=tm-db \
  --set slack.tokenSecret.name=tm-slack

# Install:
helm upgrade --install toggle-monitor deploy/helm/toggle-monitor \
  --namespace monitoring --create-namespace \
  -f my-values.yaml
```

Chart structure + production checklist:
**[deploy/helm/toggle-monitor/README.md](deploy/helm/toggle-monitor/README.md)**.

## Documentation map

| Doc | Audience | What it covers |
|---|---|---|
| [docs/architecture.md](docs/architecture.md) | New contributors | Module map, data-flow diagrams, state machines, single-replica rationale. |
| [docs/operations.md](docs/operations.md) | SREs running it in prod | Endpoints, metrics, log format, schema migrations, troubleshooting. |
| [docs/config-schema.md](docs/config-schema.md) | Operators writing the YAML | Per-field schema reference. Field types, validation rules, examples. |
| [docs/config-example.yaml](docs/config-example.yaml) | Operators | Hand-written example exercising the full schema. |
| [docs/adr/](docs/adr/) | Anyone curious why | Architecture Decision Records for the consequential choices (tooling, deps, layout). |
| [docs/internal/](docs/internal/) | Archaeology | The original PRD, design-decisions log, tracer-bullet issues, and initial brief — preserved for historical context; superseded in detail by the docs above and the ADRs. |
| [deploy/local/README.md](deploy/local/README.md) | Developers | docker-compose dev stack with autoreload. |
| [deploy/helm/toggle-monitor/README.md](deploy/helm/toggle-monitor/README.md) | Operators | Helm chart, values reference, production checklist. |

## CLI surface

The single `toggle-monitor` binary ships these subcommands:

| Command | What it does |
|---|---|
| `serve` (default) | Run the monitor service. |
| `validate <path>` | Pre-push CI check — exits non-zero with line-numbered errors if the YAML is invalid. |
| `config show [--monitor <slug>]` | Print the fully merged final config for every monitor (or one). |
| `migrate [--check]` | Apply pending schema migrations. `--check` verifies without applying. Designed to run as an ArgoCD `PreSync` hook. |

```bash
just build                       # produces ./bin/toggle-monitor
./bin/toggle-monitor --help
```

## Repository layout

```
cmd/toggle-monitor/         # main + cobra CLI wiring
internal/                   # 17 packages, one per concern
  config/   slug/   secret/ db/    migrate/ store/
  kube/     merger/ scheduler/ httpcheck/ sslinspect/ alert/
  slack/    heartbeat/ web/   lifecycle/ observability/
deploy/local/               # docker-compose dev stack
deploy/helm/                # production Helm chart
docs/                       # PRD, schema, design notes, ADRs, this doc map
scripts/                    # tailwindcss.sh and friends
```

A deeper module-by-module tour lives in
**[docs/architecture.md](docs/architecture.md)**.

## Development workflow

```bash
just                       # list every recipe
just build                 # static binary
just test                  # unit tests (fast, no Docker)
just test-integration      # integration tests (needs Docker; postgres via testcontainers)
just lint                  # golangci-lint v2 with the curated rule set
just templ                 # regenerate _templ.go from .templ sources
just tailwind              # recompile CSS into the embedded asset path
just tools                 # install every local tool into bin/
```

The task runner is **[just](https://github.com/casey/just)**.
Install once (`brew install just` / `cargo install just` / one-line
installer at https://just.systems) and reuse.

### What gets tested where

- **Unit tests** (`just test`) exercise pure-logic modules — slug
  rules, the alert state machine, config validation,
  `httpcheck.Check` against `httptest`, Block Kit builders, Slack
  client against a fake server, mention resolution, secret
  masking, etc.
- **Integration tests** (`just test-integration`) spin a real
  Postgres via testcontainers and cover the DB layer, migrations,
  the store repository, the scheduler's full Tick path, the kube
  watcher, the materializer, the web server, and the end-to-end
  lifecycle (`RunServe` against a fake Slack + httptest upstream).

`_test.go` files use the standard `testing` package; integration
tests are gated by the `integration` build tag so the default
`go test ./...` stays fast and reproducible anywhere.

### Codegen

Two checked-in artifacts are generated:

- `internal/web/templates/*_templ.go` — produced by `just templ`
  from `.templ` source files. `air` runs this automatically in the
  dev container.
- `internal/web/static/css/app.css` — produced by `just tailwind`
  from `internal/web/tailwind/input.css`. Re-run manually after
  changing utility classes in `.templ` files.

## Production deployment summary

A complete production install needs:

1. **Postgres.** Bring your own (CNPG recommended). The chart
   doesn't manage the DB.
2. **Secrets.** A Secret with the DB password (key: `password`) and
   a Secret with the Slack bot token (key: `bot-token`). Reference
   them from `values.yaml` via `postgres.passwordSecret.name` and
   `slack.tokenSecret.name`.
3. **`stakater/reloader`** (or equivalent) so config edits trigger
   a rolling restart.
4. **An Ingress** restricted to your internal network — there is
   **no auth in v1**.
5. **(Optional) kube-prometheus-stack** to scrape `/metrics` via the
   chart's `ServiceMonitor` (`serviceMonitor.enabled=true`).
6. **(Optional) ArgoCD** to drive the install. The chart includes a
   PreSync hook Job that runs `toggle-monitor migrate` before each
   sync.

Full checklist:
**[deploy/helm/toggle-monitor/README.md](deploy/helm/toggle-monitor/README.md#production-checklist)**.

## Contributing

Issues and PRs are welcome. Follow the development workflow above and
open a PR against `main`.

PRs are expected to:

- Pass `just lint`, `just test`, and (for non-trivial changes)
  `just test-integration`.
- Include focused tests when the change touches the alert state
  machine, the config validator, the kube materializer, or the
  store.
- Update `docs/` if a design decision changes (don't let docs and
  code drift). Subsystem-scale changes that supersede earlier
  decisions land as a new ADR under [`docs/adr/`](docs/adr/).

## License

Released under the [MIT License](LICENSE). © 2026 Toggle Corp.
