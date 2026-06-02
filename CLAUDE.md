## Testing

Integration tests are behind the `//go:build integration` tag and need Docker
(they spin up real Postgres via testcontainers). A plain `go test ./...` skips
them entirely, so they can silently go red without anyone noticing.

Before changing notification/dispatch wiring (`internal/coalesce`,
`internal/lifecycle`, `internal/scheduler`), run them explicitly:

    go test -tags integration ./internal/lifecycle/... -count=1

The lifecycle integration tests are the only ones that exercise the real
`RunServe` → `coalesce.New` → dispatcher → Slack path end to end. The
2026-06-02 alert blackout (dispatcher built without its individual `Sink`,
silently dropping all sub-threshold alerts) was caught only by these — and it
shipped because they weren't being run. Treat them as required, not optional.
