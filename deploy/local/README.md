# Local development stack

A docker-compose stack that runs toggle-monitor against a real
Postgres and a deliberately mixed upstream (httpbin), so you can poke
at the alert state machine without setting up Kubernetes.

## Quickstart

Pick the flavor:

| Flavor | Command | When to use |
|---|---|---|
| **production-like** (built image, no reload) | `make dev-up` | shaking down behavior, demos |
| **autoreload** (air watches `.go` + `.templ`) | `make dev-watch-up` | day-to-day iteration |

Both flavors use the same `config.yaml`, postgres volume, and httpbin
upstream, so you can switch between them freely.

```bash
cp deploy/local/.env.example deploy/local/.env   # optional; fill in
                                                  # SLACK_BOT_TOKEN
                                                  # for real Slack
make dev-watch-up                                # build dev image + start
make dev-watch-logs                              # follow rebuild output
open http://localhost:8080                       # the UI
```

Touch any `.go` or `.templ` file → air rebuilds + restarts the
binary inside the container within ~1s. The Go module + build caches
are kept in named volumes so warm rebuilds stay fast.

## What's in the box

| Service     | What it does                                                                 |
|-------------|------------------------------------------------------------------------------|
| `postgres`  | Postgres 17, persisted to a named volume.                                    |
| `upstream`  | httpbin — a generic HTTP service the sample config probes.                   |
| `migrate`   | one-shot job that applies pending migrations against `postgres`.             |
| `app`       | toggle-monitor `serve`. Depends on `migrate` completing successfully.        |

## Files

- `docker-compose.yaml` — the stack definition.
- `config.yaml` — sample config mounted into both `migrate` and `app`
  at `/etc/toggle-monitor/config.yaml`. Edit freely.
- `.env.example` — secrets template. Copy to `.env` and edit. Real
  values aren't checked in.

## Tweaking the demo

- Add a monitor: append to `monitors:` in `config.yaml` and
  `docker compose restart app`.
- Make the flaky monitor recover: change its URL from
  `/status/500` to `/status/200` and restart `app` — you should see
  the resolve transition in the UI alerts feed and the
  `toggle_monitor_checks_total` counter shift.
- Hit `/metrics` directly: `curl localhost:8080/metrics | head`.
- Hit `/healthz` and `/readyz`: same idea.

## Teardown

```bash
make dev-down          # stop the production-like stack
make dev-watch-down    # stop the autoreload stack
make dev-clean         # also drop the postgres + go-cache volumes
```

## Autoreload notes

- Touches to `.go`, `.templ`, or `.sql` files trigger a rebuild.
- `templ generate` runs as an air pre-build step, so editing a
  `.templ` file regenerates the `_templ.go` automatically.
- Tailwind CSS is **not** rebuilt on the fly — run `make tailwind`
  on the host after adding new utility classes in `.templ` files
  (the checked-in `app.css` is usually fine for dev).
- `_test.go` edits are ignored to keep the watcher quiet; run
  `make test` or `make test-integration` from the host instead.
