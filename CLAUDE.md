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

### Outage simulation

`internal/lifecycle/outage_simulation_test.go` runs `RunServe` against a
simulated network: `fakedns_test.go` installs a fake UDP DNS zone as the
process resolver, so flipping one gate makes every probe *and* Slack fail
name resolution with a real `*net.DNSError`. That is how notification-storm
bugs get reproduced rather than reasoned about — count the messages a channel
receives for one outage.

Two things the rig has to do, and any new scenario must keep doing:

- Close the servers' live connections when the gate flips. Probes ride pooled
  keep-alive connections and would otherwise never re-resolve, so a loopback
  outage looks like no outage at all.
- Give monitors an `interval` several times the `pendingWait`. Equal-ish
  values hide the trickle behaviour that ADR-0015 is about.
