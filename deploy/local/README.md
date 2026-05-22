# Local development stack

A docker-compose stack that runs toggle-monitor against a real
Postgres and a deliberately mixed upstream (httpbin), so you can poke
at the alert state machine without setting up Kubernetes.

## Quickstart

```bash
cp deploy/local/.env.example deploy/local/.env  # optional; fill in
                                                 # SLACK_BOT_TOKEN if
                                                 # you want real Slack
make dev-up                                     # build + start
open http://localhost:8080                      # the UI
```

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
make dev-down       # stop + remove containers
make dev-clean      # also drop the postgres volume
```
