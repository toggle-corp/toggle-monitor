# Sentry integration

Design captured during brainstorming on 2026-05-25. Implementation
shipped in the same session.

## Goals

Forward unhandled panics and *actionable* `ERROR`-level slog records
to Sentry so operators learn about unexpected failures without
parsing JSON log streams. The tool's primary signal (failing
monitored endpoints) stays in Prometheus + Slack as today — Sentry
gets only the things that mean "operator should look at toggle-monitor
itself."

## Non-goals (v1)

- Performance tracing / spans (`tracesSampleRate` is wired through
  but we add no `StartSpan` calls).
- User context / `sentry.SetUser` (no auth in v1).
- Manual breadcrumbs — the captured slog record IS the breadcrumb.
- Custom fingerprinting — defaults until evidence says otherwise.
- Pod / namespace tags via the downward API.
- Replacing Prometheus or the Slack incident pipeline.

## Architecture

A new package `internal/sentry` owns the SDK lifecycle and the
slog→Sentry bridge.

```
                  ┌──────────────────────────────────┐
                  │ lifecycle.RunServe               │
                  │  1. config.Load                  │
                  │  2. sentry.Init(cfg.Sentry)      │
                  │  3. build slog handler chain     │
                  │  4. … rest of startup            │
                  │  5. on shutdown: sentry.Flush(2s)│
                  └──────────────────────────────────┘
                              │
            ┌─────────────────┼────────────────────┐
            │                 │                    │
            ▼                 ▼                    ▼
   slog handler chain   HTTP middleware     panic recoverers
   stdout JSON ─┐                                  │
                ├─ slog.MultiHandler               │
   sentry hook ─┘  (level >= ERROR)                │
                                                   │
                       ▼ (panic events)            │
                ┌────────────────┐                 │
                │ sentry-go SDK  │◄────────────────┘
                └────────────────┘
```

Package surface:

| Symbol | Purpose |
|---|---|
| `Init(cfg config.Sentry, release string) (flush func(), err error)` | Bootstraps the SDK; no-op when disabled. Returns a flush closure called from the lifecycle shutdown path. |
| `Handler() slog.Handler` | The level-gated slog bridge. Always returned (returns a no-op handler when disabled) so the lifecycle wiring is unconditional. |
| `HTTPMiddleware(next http.Handler) http.Handler` | Wraps the chi mux. Recovers panics, repanics so Go's default error response still fires. |
| `RecoverGoroutine(log *slog.Logger, where string)` | Deferred helper for the scheduler / kube workers. |

## Configuration

New top-level `sentry:` YAML block (optional). Defaults to disabled.

```yaml
sentry:
  enabled: true               # required when block is present
  dsnEnv: SENTRY_DSN          # env var holding the DSN
  environment: production     # tag every event; defaults to "production"
  sampleRate: 1.0             # [0.0..1.0]; defaults to 1.0
  tracesSampleRate: 0.0       # [0.0..1.0]; defaults to 0.0
  serverName: ""              # defaults to os.Hostname()
```

Resolution rule:

| `enabled` | DSN env value | Behavior |
|---|---|---|
| `false` or block absent | — | disabled; no-op handler; no log |
| `true` | non-empty | initialized |
| `true` | empty / unset | **startup fails** with `sentry enabled but <dsnEnv> not set` |

Validation lives in `internal/config/config.go` alongside the other
top-level blocks. The env-var check runs in `cli/serve.go` next to
the existing `database.passwordEnv` check.

## Slog bridge semantics

`internal/sentry/handler.go` implements `slog.Handler`:

- `Enabled(_, lvl) → lvl >= slog.LevelError` (WARN and below dropped).
- `Handle(ctx, r)`:
  - Message ← `r.Message`.
  - Level ← `sentry.LevelError`.
  - Tags ← any record attr named `monitor` becomes `monitor=<slug>`.
  - The attr named `error` (if it's a Go `error`) drives
    `sentry.NewException` → stacktrace + grouping by exception. All
    other attrs go to `Extra`.
  - Release ← global, set at `Init` time from build ldflag
    `main.version` (passed through `RunServe`).
- Uses `sentry.CurrentHub().Clone()` per call so concurrent log
  handlers don't share scope.

## Panic recovery

Three capture sites:

| Site | Repanic | Reason |
|---|---|---|
| `HTTPMiddleware` (chi mux) | yes | Preserve Go's default handler-panic behavior so the client sees a 500. |
| `scheduler.runMonitor` per-tick defer | no | One panicking monitor must not kill the whole scheduler. |
| `kube.Watcher.Run` reconcile defer | no | Transient k8s API bugs must not crash the binary. |

Helper:

```go
func RecoverPanic(log *slog.Logger, where string) {
    if r := recover(); r != nil {
        err := fmt.Errorf("panic in %s: %v", where, r)
        sentry.CurrentHub().Recover(err) // captures with stack
        log.Error("recovered panic", "where", where, "error", err)
        sentry.Flush(2 * time.Second)
    }
}
```

The `log.Error` call re-forwards via the slog bridge — duplicate
event accepted (Sentry fingerprinting merges; cheap; far less
mechanism than de-dup).

## Log-level downgrades

Three pre-existing ERROR sites are routine business signals; without
this they would flood Sentry. Reclassified to WARN. Behavior
unchanged otherwise.

| File:line | Before | After |
|---|---|---|
| `scheduler.go:267` | `log.Error("tick error", …)` | `log.Warn` |
| `scheduler.go:417` | `log.Error("apply ssl check", …)` | `log.Warn` |
| `kube.go:147, 157` | `log.Error("kube reconcile failed", …)` | `log.Warn` |

Sites that stay at ERROR (and therefore reach Sentry):

- `scheduler.go:386` — Slack event-sink failure
- `scheduler.go:432` — Slack SSL-sink failure
- `lifecycle.go:292` — HTTP listener crash
- The new `recovered panic` site

`docs/operations.md` Logs table updated to match.

## Helm chart

No new chart Secret. The chart's existing `envFromSecrets` pattern
already projects arbitrary secrets into env vars; operators add a
`SENTRY_DSN` entry there:

```yaml
envFromSecrets:
  - name: SENTRY_DSN
    secret:
      name: toggle-monitor-sentry
      key: dsn
```

The `values.yaml` gains a commented-out example and a new
`sentry:` block under `config.inline` mirroring the YAML schema
above. Defaults keep Sentry disabled.

## Build wiring

`Dockerfile` build stage already trims paths and strips symbols. Add
`-X main.version=$(VERSION)` to the existing `-ldflags`. The
`justfile` `build` recipe gets the same treatment for local builds
(`git describe --tags --always` falling back to `dev`).

`main.version` is a new package-level `var version = "dev"` in
`cmd/toggle-monitor/main.go`, threaded into the CLI and then to
`lifecycle.RunServe` via a new `ServeOptions.Release` field.

## Testing

| Layer | What |
|---|---|
| Unit (`internal/sentry/handler_test.go`) | record with `error` attr → Exception present; without → message-only; `monitor` attr → Sentry tag; WARN dropped (`Enabled`). |
| Unit (`internal/sentry/recover_test.go`) | `RecoverPanic` captures + doesn't repanic. |
| Unit (`internal/sentry/http_test.go`) | handler panic → middleware captures + repanics so net/http's default applies. |
| Unit (`internal/sentry/init_test.go`) | enabled=false → noop; enabled=true + DSN → real client; enabled=true + DSN missing → error. |
| Config validation | `sentry:` block round-trip + validation errors. |
| Integration | (deferred — covered by unit tests above; lifecycle integration would require a Sentry transport mock not provided by sentry-go's public API.) |

All Sentry SDK interaction in tests routes through
`sentry.ClientOptions{Transport: …}` with a capturing fake transport
defined in `internal/sentry/testtransport.go`.
