# TODO

- [x] Setup grafana dashboard
- [x] Handle slack API downtime
- [x] Push to Github
    - [x] Setup fugit for release
    - [x] Setup CI to push docker, helm
        - Modeled on banjo-action's generate-release; implemented in `release.yml` directly (not via `workflow_call` to banjo).
- [ ] Simple admin panel (With credential, using secrets)
    - [ ] Test slack notification
    - [ ] View parsed config (masked sensitive information)
- [ ] Add support for wildcard probe testing??
