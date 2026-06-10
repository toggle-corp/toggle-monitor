## Testing

Integration tests are behind the `//go:build integration` tag and need Docker
(they spin up real Postgres via testcontainers). A plain `go test ./...` skips
them entirely, so they can silently go red without anyone noticing.

Before changing notification/dispatch wiring (`internal/coalesce`,
`internal/lifecycle`, `internal/scheduler`), run them explicitly:

    go test -tags integration ./internal/lifecycle/... -count=1

The lifecycle integration tests are the only ones that exercise the real
`RunServe` → `coalesce.New` → dispatcher → Slack path end to end. Treat them
as required, not optional.
