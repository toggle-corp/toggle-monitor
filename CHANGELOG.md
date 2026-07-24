# Changelog

## [0.3.0](https://github.com/toggle-corp/toggle-monitor/compare/0.2.0..0.3.0) - 2026-07-24
### Changes:

#### 🚀  Features

- *(selfhealth)* Detect monitor-blind outage, suppress false storm (ADR-0008) - ([86e6459](https://github.com/toggle-corp/toggle-monitor/commit/86e64595bc704aafaf9f6649a5f40ed0c9eada9e))

#### ⚙️ Miscellaneous Tasks

- Use tagged version for fugit - ([efdd485](https://github.com/toggle-corp/toggle-monitor/commit/efdd485be78f95ce9a2d206f52f45013cd732e1a))


## [0.2.0] - 2026-06-11
### Changes:

#### 🚀  Features

- *(alertmanager)* /alerts listing and /alert/{id} detail page (ADR-0005) - ([8ed425c](https://github.com/toggle-corp/toggle-monitor/commit/8ed425c0554328df33c85bee07f324295d2760f6))
- *(alertmanager)* Lifecycle wiring + retention sweeper (ADR-0005) - ([639075f](https://github.com/toggle-corp/toggle-monitor/commit/639075fdde33d5e10a06e4e4648c8b5454e3905e))
- *(alertmanager)* Webhook HTTP handler (ADR-0005) - ([8e12b2f](https://github.com/toggle-corp/toggle-monitor/commit/8e12b2f9318706bb8ebe7f820e13c730f413cad3))
- *(alertmanager)* Per-channel sliding-window rate limiter (ADR-0005) - ([6a380b9](https://github.com/toggle-corp/toggle-monitor/commit/6a380b98dc293bcb99f696c8f0d4a3fc1ed0895c))
- *(alertmanager)* Slack block-kit renderer for AM events (ADR-0005) - ([5141626](https://github.com/toggle-corp/toggle-monitor/commit/51416263bf4dec55f61f2c3a59b65a8e4c44268b))
- *(alertmanager)* Cascade evaluator for match rules (ADR-0005) - ([4f9731f](https://github.com/toggle-corp/toggle-monitor/commit/4f9731f12e684eb9d3a0874d3aea042895a39058))
- *(alertmanager)* Payload types, migration, and store methods (ADR-0005) - ([36a292f](https://github.com/toggle-corp/toggle-monitor/commit/36a292f42c9ddfe86d54c2cc4e7ccbc12a915ebd))
- *(alertmanager)* Config types and validator (ADR-0005) - ([2193282](https://github.com/toggle-corp/toggle-monitor/commit/21932829260689d967fafb797faab2fe9ae05ce4))
- *(cli)* Explain subcommand for kube.match cascade - ([e56ce37](https://github.com/toggle-corp/toggle-monitor/commit/e56ce37ab2c85537221891fc8140f034d9cdb347))
- *(cli)* --kubeconfig flag + docker-compose host-kubeconfig mount - ([8d29343](https://github.com/toggle-corp/toggle-monitor/commit/8d29343818fb9533e3065f04f4482f860b742fdc))
- *(cli)* --notify flag on slack test (uptime + ssl) - ([02bda29](https://github.com/toggle-corp/toggle-monitor/commit/02bda29f314ac3fd1d6aeb3cf215871c6650d7ed))
- *(cli)* Wire validate and config show - ([0f1c141](https://github.com/toggle-corp/toggle-monitor/commit/0f1c141dc47bb38d83c7824ef5cd1efce4383b57))
- *(coalesce)* Inject @channel/@here broadcast on group open + reminder - ([c5ab818](https://github.com/toggle-corp/toggle-monitor/commit/c5ab818d6fa19ee9b91276ae7f815b42e374fcbe))
- *(coalesce)* Three-state per-channel dispatcher (ADR-0004) - ([f9da056](https://github.com/toggle-corp/toggle-monitor/commit/f9da0564a99dbbd0e0610ee2da7495ee3d2d9ec2))
- *(coalesce)* Wire per-channel digest into scheduler + lifecycle - ([d51f3f8](https://github.com/toggle-corp/toggle-monitor/commit/d51f3f8b140add1ccf15a329c1b7c4fdeaaf3940))
- *(coalesce+lifecycle)* On-demand parent probe at pendingWait expiry (ADR-0004) - ([33cd1fc](https://github.com/toggle-corp/toggle-monitor/commit/33cd1fc89b16d1789110cd1b02bbf518edf5e2e1))
- *(config)* PendingWait alias + burst-dispatcher knobs (ADR-0004 prep) - ([acafb93](https://github.com/toggle-corp/toggle-monitor/commit/acafb93bf9e6b37011a3162d82b71bf042ae46be))
- *(config)* Critical flag, coalesce timings, parent-interval warning - ([4f171ae](https://github.com/toggle-corp/toggle-monitor/commit/4f171ae005de1ffa4ab7f8c737f99f8d9d188c7b))
- *(config)* Reject unknown keys at every level - ([022af8e](https://github.com/toggle-corp/toggle-monitor/commit/022af8ee7949631f94355e0a524414fb7651df17))
- *(config)* Allow slash-namespaced tags (e.g. acme/app-a) - ([ddb38b1](https://github.com/toggle-corp/toggle-monitor/commit/ddb38b151fe7687117570a18509bbadb971bd058))
- *(config)* ADR-0003 — StatusPage replaces Group; tag-driven membership - ([0f54412](https://github.com/toggle-corp/toggle-monitor/commit/0f54412efcb804ccb84a84220f8bec944870056b))
- *(config)* Validator for cascading kube.match tree - ([6d51108](https://github.com/toggle-corp/toggle-monitor/commit/6d51108482003e12e7893d4c6eaf77f578814c89))
- *(config)* Redesign kube.match as cascading rule tree - ([edb8c0a](https://github.com/toggle-corp/toggle-monitor/commit/edb8c0acf7366ab728e28cfeb07c3aef1f433557))
- *(config)* Validate defined fields on kube presets - ([236f052](https://github.com/toggle-corp/toggle-monitor/commit/236f052c614a481ec3a6102359bc88d854830549))
- *(config)* Outbound SOCKS5 proxies + monitor proxy: slug - ([185929d](https://github.com/toggle-corp/toggle-monitor/commit/185929de3ecfd22f4b0bd3367b8a0d0fb5e66516))
- *(config)* TlsInsecureSkipVerify for self-signed HTTPS endpoints - ([7459cec](https://github.com/toggle-corp/toggle-monitor/commit/7459cec512328ba4e16e98f603e3117e6ae9adee))
- *(config)* Anchors, x-* ignore, env interp, multi-error reporting - ([e349e75](https://github.com/toggle-corp/toggle-monitor/commit/e349e756a6a5d536d6c1fe24bec9d7695d067dbd))
- *(core)* Slug, alert, httpcheck, config modules with TDD coverage - ([62b76e0](https://github.com/toggle-corp/toggle-monitor/commit/62b76e0edaec2ac793fac8c69acc03d3f74a4d5b))
- *(db,store,scheduler)* Persistence layer + worker loop with PG-backed tests - ([f4070f4](https://github.com/toggle-corp/toggle-monitor/commit/f4070f4a0921319ae5c167ae47508ad1b37efbbc))
- *(depindex)* Reverse-dependsOn index for push-propagation (ADR-0004) - ([9a8cc52](https://github.com/toggle-corp/toggle-monitor/commit/9a8cc523970fac9fe776b228afd3697104d7a7b5))
- *(dev)* Colorize just validate-config output - ([6d46607](https://github.com/toggle-corp/toggle-monitor/commit/6d46607ab71085d9778a1415d7d78c6b7ed13082))
- *(dev)* Personal local config.yaml + just validate-config - ([d344408](https://github.com/toggle-corp/toggle-monitor/commit/d344408f37b8be0e8ab421acde4ca6df79efb663))
- *(group)* Alert-coalescing state machine for per-channel digests - ([688e7bd](https://github.com/toggle-corp/toggle-monitor/commit/688e7bda9eefcb7a2154137fad2724243072c537))
- *(heartbeat)* Deadman heartbeat + stalled-worker detection - ([04492a9](https://github.com/toggle-corp/toggle-monitor/commit/04492a9df1a50bf6caaf9762537413cb7418954d))
- *(helm)* Ship Grafana dashboard ConfigMap for /metrics - ([d4fc61d](https://github.com/toggle-corp/toggle-monitor/commit/d4fc61d833700d4d2bf0d7c8ae00a0793fb1b0ce))
- *(helm)* Rename chart name to toggle-monitor-helm - ([3cfc1a3](https://github.com/toggle-corp/toggle-monitor/commit/3cfc1a31f2184e6d43448efab22f376d37c5afde))
- *(helm)* Rework values shape, secrets, restart-on-change, examples - ([0c99066](https://github.com/toggle-corp/toggle-monitor/commit/0c990660988ac4c2a94707d815d71cfd1de29da2))
- *(issues)* Surface dependsOn-parent lookup failures on /issues - ([0d4498a](https://github.com/toggle-corp/toggle-monitor/commit/0d4498a77666dda7776a76582b1acb84908b06a0))
- *(kube)* Reserve `kube-` prefix for auto-discovered monitor slugs - ([9ca0fa5](https://github.com/toggle-corp/toggle-monitor/commit/9ca0fa5bc37d2cdf76f5c5ffae51f6ef359fbdf0))
- *(kube)* Ignore: true on match rules + /discovery filter - ([8c58bab](https://github.com/toggle-corp/toggle-monitor/commit/8c58babc16e5bc0f35c07d03425395d9f0fa7358))
- *(kube)* Configurable auto-generated friendly name styles - ([6c095a5](https://github.com/toggle-corp/toggle-monitor/commit/6c095a5124c8f95de13162e0150e95cdc94fa2ce))
- *(kube)* Conditional preset resolution + defaultPreset fallback - ([f477e2f](https://github.com/toggle-corp/toggle-monitor/commit/f477e2faf487ac3915fa8c420e98deae05db8ece))
- *(kube)* Log reconcile pass with ingress count - ([751de54](https://github.com/toggle-corp/toggle-monitor/commit/751de54b0730206455fe3c264b9f0dda968066ef))
- *(kube)* Informer, materializer, pause, discovery UI - ([44b48e8](https://github.com/toggle-corp/toggle-monitor/commit/44b48e830958883121c61fe8dbba9851f2f03d9f))
- *(lifecycle)* Soft-delete monitors removed from config - ([e47d3cd](https://github.com/toggle-corp/toggle-monitor/commit/e47d3cd85869afacc962fb9a425ac3ecf3c2c032))
- *(lifecycle)* Graceful SIGTERM ordering - ([274c76c](https://github.com/toggle-corp/toggle-monitor/commit/274c76c12704d454562a22730f194a886bc4a24b))
- *(migrate)* Per-step progress + pending-list in `migrate` output - ([30996a9](https://github.com/toggle-corp/toggle-monitor/commit/30996a95f98fbc4f91797017ddd9ece3b8941534))
- *(observability)* Prometheus /metrics + scheduler instrumentation - ([6275719](https://github.com/toggle-corp/toggle-monitor/commit/6275719207a595b2c762feedf80d2fc7a88f85d7))
- *(release)* Accept pre-release tags; mark hyphenated tags as prerelease - ([90d1cc7](https://github.com/toggle-corp/toggle-monitor/commit/90d1cc763dd1f1fbd8078bd2c23e273f74dbe645))
- *(release)* GHCR publish pipeline (docker + helm + GH release) - ([96680df](https://github.com/toggle-corp/toggle-monitor/commit/96680dfd0b822cae12ba19fb0a5bef843cd22300))
- *(release)* Vendor fugit as submodule pinned to feat/gitea-support - ([559ac8c](https://github.com/toggle-corp/toggle-monitor/commit/559ac8ca2427a5ecd4ebf9a3edb3aafaaab09a63))
- *(release)* Add release.sh wrapper + RELEASE_CUSTOM_HOOK with tests - ([80f9939](https://github.com/toggle-corp/toggle-monitor/commit/80f99399e58b36e5810cd2a731e2b09e70e575ea))
- *(removal)* Full Slack closeout + warning on monitor removal - ([1448587](https://github.com/toggle-corp/toggle-monitor/commit/1448587534662283f5d3a9afe4280a3f7d375d79))
- *(scheduler)* Dynamic refresh — kube monitors actively probed - ([c01e06b](https://github.com/toggle-corp/toggle-monitor/commit/c01e06ba2b9d867bb0b02f37bcb83ed0d77f2f15))
- *(scheduler)* DependsOn gating + temporary-paused status - ([65a8fcc](https://github.com/toggle-corp/toggle-monitor/commit/65a8fcc5896bc16faa77ec415362d0a35f565f0a))
- *(scheduler+lifecycle)* Wire dispatcher.Route + real push-propagation (ADR-0004) - ([adf4d87](https://github.com/toggle-corp/toggle-monitor/commit/adf4d872a58ac2563644a5837c12644214675dbc))
- *(sentry)* Forward panics + operator-actionable ERRORRs to Sentry - ([e9b5cfc](https://github.com/toggle-corp/toggle-monitor/commit/e9b5cfc77f9bbdf6e99f3015ecfb8d6a63960203))
- *(slack)* Fenced error bodies, two-block reminder (ADR-0007) - ([abdf480](https://github.com/toggle-corp/toggle-monitor/commit/abdf480224387769d021d2aaa6e9cfe1f872488f))
- *(slack)* Blocks-only message rendering (ADR-0006) - ([0c8421e](https://github.com/toggle-corp/toggle-monitor/commit/0c8421eb935cb2da16b4b139572deb6208377107))
- *(slack)* Digest message builders for coalesced incidents - ([024a7bd](https://github.com/toggle-corp/toggle-monitor/commit/024a7bd5b006eb39ea9a70e276aeb7dbe30a09c8))
- *(slack)* Classify + retry + fresh-parent recovery for transient failures - ([9ad25ef](https://github.com/toggle-corp/toggle-monitor/commit/9ad25ef4104a328c36feb23ae72eeec1457090c6))
- *(slack)* Cap dependents note + label mentions with *CC:* - ([84c3ceb](https://github.com/toggle-corp/toggle-monitor/commit/84c3ceb0571a9a9243756d782b56f5d08ed1d96c))
- *(slack)* Cascading-effect note for monitors with dependents - ([68b9614](https://github.com/toggle-corp/toggle-monitor/commit/68b9614affa4e5741f43e45afea24a6b9ec9052b))
- *(slack)* Adopt uptime robot message format - ([bcc1659](https://github.com/toggle-corp/toggle-monitor/commit/bcc16597045e11eaa8b8b30f40f91a76cdcb2661))
- *(slack)* Compact bold-label layout + color stripe; add slack test cli - ([5b93f3c](https://github.com/toggle-corp/toggle-monitor/commit/5b93f3cecc32a550ed6c8719e492de98c9cb262b))
- *(slack)* 24h userMapping revalidation + UI panel - ([d466348](https://github.com/toggle-corp/toggle-monitor/commit/d46634831bd00e1d2ce9cb0dfcf5ef2344e9bcae))
- *(slack)* UserMapping + group.notify + mention resolution - ([d0a7652](https://github.com/toggle-corp/toggle-monitor/commit/d0a7652e20c98a0618f34dbdcfe2e05fa990cc28))
- *(slack)* Uptime alert lifecycle — parent, reminder, resolve - ([a36c874](https://github.com/toggle-corp/toggle-monitor/commit/a36c87435c19b83b4514cdb37fc6db081dbe5f06))
- *(smtp)* Add SMTP service monitoring (depth-c probe) - ([d1bce43](https://github.com/toggle-corp/toggle-monitor/commit/d1bce4300544ff410a5672bd4989e45eccfe87fd))
- *(ssl)* SSL inspection + independent SSL alert lifecycle - ([a64783a](https://github.com/toggle-corp/toggle-monitor/commit/a64783aef4105f01466db6bf568b0a589b9e1817))
- *(statusPage)* Support multiple slugged status pages - ([fd584ad](https://github.com/toggle-corp/toggle-monitor/commit/fd584ad412ae202b7adc45a9b65a7d66244b2f16))
- *(statusPage)* GroupRegex selector for pattern-matched groups - ([01132cf](https://github.com/toggle-corp/toggle-monitor/commit/01132cfde1816aa5d2511aebeec5a4fa1c9dee49))
- *(store)* Persist incident groups for restart reattach - ([cb7d25e](https://github.com/toggle-corp/toggle-monitor/commit/cb7d25ece110b7b7a5792b0fc802d8cb32e41bd3))
- *(store)* Tags column on monitors + plumbing through config + merger - ([23e3a66](https://github.com/toggle-corp/toggle-monitor/commit/23e3a661fee724ef2fb9c41ca709b469ceb34ef5))
- *(ui)* Issues count badge + Status nav tab + table-style /status - ([40180fa](https://github.com/toggle-corp/toggle-monitor/commit/40180faec5db363bbfc298d2a14511f56a189878))
- *(ui)* Bare-minimum /issues page surfacing existing problem signals - ([64b4006](https://github.com/toggle-corp/toggle-monitor/commit/64b40068bc9e437d6a465703c9f01acef5f27e49))
- *(ui)* /groups index + group-scoped stats + clickable group slug - ([ce73f0e](https://github.com/toggle-corp/toggle-monitor/commit/ce73f0ea0a3d0a3b3aa097a044310fa73f145390))
- *(ui)* Clickable Depends-on / Gated-by entries on the detail page - ([8012a11](https://github.com/toggle-corp/toggle-monitor/commit/8012a114e375522740df73f6d1cf23f62cecc495))
- *(ui)* Latest-alerts shows the monitor friendly name, not the slug - ([241872d](https://github.com/toggle-corp/toggle-monitor/commit/241872d04d48055c5a6d211b3b64b83bce105339))
- *(ui)* Render mentions as "slug U…" in the config dialog - ([294426a](https://github.com/toggle-corp/toggle-monitor/commit/294426ae55689267dfb35dbd5f492984128451b4))
- *(ui)* Move config behind a popup, surface kube preset inline - ([c8a5d52](https://github.com/toggle-corp/toggle-monitor/commit/c8a5d52425280232e323afe3fe30e0f1c7ea2e99))
- *(ui)* Monitor detail renders effective config above current state - ([a6d34ff](https://github.com/toggle-corp/toggle-monitor/commit/a6d34ffef8caaad4cf7a89e37f4ca7acd77bb9ec))
- *(ui)* /discovery empty state distinguishes kube disabled vs idle - ([b2efb6c](https://github.com/toggle-corp/toggle-monitor/commit/b2efb6ce00d5853fecc1c336499af59d407ed0a1))
- *(ui)* 4 new columns + sortable headers on /monitors - ([20e7ffe](https://github.com/toggle-corp/toggle-monitor/commit/20e7ffe661cccecbc8868977d282b9dfa1c3fe42))
- *(ui)* Clickable URLs in listings + per-row last error - ([9606f3d](https://github.com/toggle-corp/toggle-monitor/commit/9606f3d5fc04cbabe6326fcaca3cce8a671f67ef))
- *(ui)* Badges, human time diffs, clickable overview, ssl filter - ([a43cd26](https://github.com/toggle-corp/toggle-monitor/commit/a43cd26caa6c0c27176b9f16f2acd8a129a217cc))
- *(ui)* System/light/dark theme toggle - ([9c0dcb5](https://github.com/toggle-corp/toggle-monitor/commit/9c0dcb591b02c24a5f1a1b5d126182c5e99fc079))
- *(web)* Popover tag picker + removable chip preview on /monitors - ([8100bd0](https://github.com/toggle-corp/toggle-monitor/commit/8100bd0804445d6a7b29a76452267fa4335494ce))
- *(web)* Chip-style tag filter on /monitors - ([117e527](https://github.com/toggle-corp/toggle-monitor/commit/117e52784c97fb658d9ac0aefac49ef3b8b64a4c))
- *(web)* Tag filter on /monitors + aligned status-page rows - ([60dcfe6](https://github.com/toggle-corp/toggle-monitor/commit/60dcfe6d3a960f263d020c87bcce2f61d98d7e30))
- *(web)* Live cascade trace on the discovery detail page - ([119f184](https://github.com/toggle-corp/toggle-monitor/commit/119f1842eae8a18cced940894c4811c4e995a1e7))
- *(web)* /status index honors config order, not alphabetical - ([172f332](https://github.com/toggle-corp/toggle-monitor/commit/172f332ab8cfd76c3b3cd2bd6004e37a459261c3))
- *(web)* Richer SSL column + expired state across /status and /monitors - ([240a8a0](https://github.com/toggle-corp/toggle-monitor/commit/240a8a0267a3f1d9f6842d2fc781b90acad5d1aa))
- *(web)* Surface archived monitors on detail page + /monitors filter - ([4d72bd5](https://github.com/toggle-corp/toggle-monitor/commit/4d72bd5bc1d87f8c96ba51f559201e38169198e0))
- *(web)* Revamp /status index — cards with branding, hero badge, problem chips - ([c6b37ba](https://github.com/toggle-corp/toggle-monitor/commit/c6b37baa696f297836f23367444bbdec8384f8d4))
- *(web)* Move per-status-page stats off / and into /status + /status/<slug> - ([6362682](https://github.com/toggle-corp/toggle-monitor/commit/63626829190fc75b48098863598bbc8dcff66924))
- *(web)* HTMX filters, pagination, /monitors and /group pages - ([a2582ef](https://github.com/toggle-corp/toggle-monitor/commit/a2582ef90e4a5f8d00ff905d137bd2499ad81ac4))
- *(web,lifecycle)* UI, probes, and serve wiring close the tracer bullet - ([1bd45e6](https://github.com/toggle-corp/toggle-monitor/commit/1bd45e6c939b3259c2ac91fc39833a846eb51b5e))
- Public /status page with host/group/tags match DSL - ([45565a0](https://github.com/toggle-corp/toggle-monitor/commit/45565a0ac5c7593634a09e6d584fdb846064a6f4))
- Bootstrap Go module, CLI, and CI scaffolding - ([62ebb3b](https://github.com/toggle-corp/toggle-monitor/commit/62ebb3b563957f299636a04c9accd6a0dcfa3381))

#### 🐛 Bug Fixes

- *(ci)* Repair release notes https://github.com/toggle-corp/toggle-monitor substitution + module-cache conflict - ([fed9477](https://github.com/toggle-corp/toggle-monitor/commit/fed94778e0689c6fcf3cf56d336a13ecccecbe4d))
- *(ci)* Scope GITHUB_TOKEN explicitly on docker_build + helm_validate - ([44cf78a](https://github.com/toggle-corp/toggle-monitor/commit/44cf78a5a72364f8957c98e04eb1c4cc91b1d9e9))
- *(ci)* Pin golangci-lint v2.12.2 + clear accumulated lint debt - ([4f6a833](https://github.com/toggle-corp/toggle-monitor/commit/4f6a833589d49d6162c14c60747d635ff8d5d4ad))
- *(coalesce+lifecycle)* Wire individual sink into dispatcher (ADR-0004) - ([68ea5fc](https://github.com/toggle-corp/toggle-monitor/commit/68ea5fc13d31b5fb29bda6dd022d990e22f55493))
- *(config)* Expand YAML merge keys when populating KubeConfig.setFields - ([7b8f1ef](https://github.com/toggle-corp/toggle-monitor/commit/7b8f1ef1dfd1ff0601720159cafa2289cd4e2b70))
- *(config)* Duration.MarshalYAML emits 'd' suffix for whole-day values - ([1f19afe](https://github.com/toggle-corp/toggle-monitor/commit/1f19afe59566dba4b52802940baf61ca834cf1cf))
- *(config)* Validate kube.presets[].group against declared groups - ([28122db](https://github.com/toggle-corp/toggle-monitor/commit/28122dbd9b7b0a5b61cabd64bb0b2268f012ef30))
- *(helm)* Order ArgoCD sync so migrate Job finds its ServiceAccount - ([880e2f0](https://github.com/toggle-corp/toggle-monitor/commit/880e2f0d309a17d11c09a8e5972d1f72648d79fa))
- *(helm)* Hoist terminationGracePeriodSeconds out of the container - ([08572ac](https://github.com/toggle-corp/toggle-monitor/commit/08572ac55e37b6ae9e40557ef34fbd67fac9ffd6))
- *(helm)* Kube section for cascading match tree - ([8ac7406](https://github.com/toggle-corp/toggle-monitor/commit/8ac740649ba5e73712c31f5ee15dcbc85e11b20b))
- *(kube)* Treat wildcard ingress hosts as kube-invalid + tear down on flip - ([184a722](https://github.com/toggle-corp/toggle-monitor/commit/184a722ce974b6a757f229969c143c160c965635))
- *(kube)* Observed-set prune so NTP drift can't archive live ingresses - ([9d5617a](https://github.com/toggle-corp/toggle-monitor/commit/9d5617aa8031cd49aad72e2ea73ff50ecd3da2e7))
- *(kube)* Informer never started — empty cache produced silent zero ingresses - ([e83f07a](https://github.com/toggle-corp/toggle-monitor/commit/e83f07a329c5e8a7b3c4d575aaadc676a7e331eb))
- *(release)* Install typos in CI so cliff preprocessor doesn't drop commits - ([3558825](https://github.com/toggle-corp/toggle-monitor/commit/3558825e8591d61537744714a56fa385979db1a1))
- *(release)* Stage Chart.yaml in hook so fugit's commit carries the bump - ([a5c8b3a](https://github.com/toggle-corp/toggle-monitor/commit/a5c8b3a00e2ae798dcbb5c53576a5d0eec3fd993))
- *(release)* Re-lint chart inside hook after version bump - ([b4fd259](https://github.com/toggle-corp/toggle-monitor/commit/b4fd2597652c2dd2da045fbe34b6c9f03b32b5cc))
- *(scheduler)* Decouple plan-refresh from kube resync - ([45ed537](https://github.com/toggle-corp/toggle-monitor/commit/45ed5373971dee871e4207035215cc141e3b3a4e))
- *(scheduler)* No duplicate Open when child was already down before pause - ([352523d](https://github.com/toggle-corp/toggle-monitor/commit/352523d6c0b1d038a11a9010590077742bab7155))
- *(web)* Split status URL cell into app-root + health-check links - ([b60c783](https://github.com/toggle-corp/toggle-monitor/commit/b60c783e2b232d747e66000fdfed2860fdf54e64))
- *(web)* Align Resolved card with the Show config dialog - ([3814cf3](https://github.com/toggle-corp/toggle-monitor/commit/3814cf34e797728564329dc74599e1d7fc2033b3))
- *(web)* Kv-shaped cascade rows, wider config dialog, tailwind rebuild - ([7e03b6d](https://github.com/toggle-corp/toggle-monitor/commit/7e03b6dc725b4d4649c18c3d5ac597c8c5006571))
- *(web)* Treat ssl-skipped as Operational, not Degraded - ([74c4bca](https://github.com/toggle-corp/toggle-monitor/commit/74c4bca3bd6107a46d26a922172cd284f570b7ee))
- *(web)* Spacing polish on /status index — header rule, tile rhythm - ([e944bf1](https://github.com/toggle-corp/toggle-monitor/commit/e944bf13f754bcfc507253fcaa5471e60bfd42a3))

#### 🚜 Refactor

- *(config)* KubeMatchRule.Ignore as *bool for tri-state cascade - ([7cacdd9](https://github.com/toggle-corp/toggle-monitor/commit/7cacdd972a174bc5e1f74077ee707ef384bfaeb3))
- *(config)* Kube types for cascading match tree - ([397eb51](https://github.com/toggle-corp/toggle-monitor/commit/397eb5139ff2772764be6ede845776045f6ba5bd))
- *(config)* Rename status block to statusPage - ([ed334b4](https://github.com/toggle-corp/toggle-monitor/commit/ed334b480ed7aa9a584c4e54adb9ab5ed94cea90))
- *(config)* Single source of truth for kube.friendlyName values - ([935c82e](https://github.com/toggle-corp/toggle-monitor/commit/935c82eba11c5cb318b9bb3a5fa49c97c8073efd))
- *(kube)* Drop defaultPreset; fallback via no-when match rule - ([d041e7c](https://github.com/toggle-corp/toggle-monitor/commit/d041e7ce67a6b4f43d8ebffdd7eeaaace5b14812))
- *(merger)* Cascading kube.match tree walker - ([6a7e14c](https://github.com/toggle-corp/toggle-monitor/commit/6a7e14cf6c60b0a72ed021775d518edfa83d3213))
- *(web)* Merge status-page Name and URL columns - ([2320f93](https://github.com/toggle-corp/toggle-monitor/commit/2320f93b2abead2f36742b7b5d49179a24088247))

#### 📚 Documentation

- *(adr)* Accept ADR-0006; cross-reference from 0005 - ([c723ca0](https://github.com/toggle-corp/toggle-monitor/commit/c723ca0648abd0d482521386c416d31c2ecd67bc))
- *(adr)* Align kube.match docs after code-review feedback - ([cbe9c25](https://github.com/toggle-corp/toggle-monitor/commit/cbe9c251697966ed6e420854ab4e99c54ffc21a7))
- *(alertmanager)* Show how to generate the webhook bearer token - ([8d09921](https://github.com/toggle-corp/toggle-monitor/commit/8d099217d2c7be0309cd6ca06c093a97ef08d3fa))
- *(config)* Add commented kube.match cascade example to local sample - ([ffbdb6c](https://github.com/toggle-corp/toggle-monitor/commit/ffbdb6c18c34ff18ca1b4150eb18617c346a70e5))
- *(config)* Expand local sample with full predicate-tree examples - ([2be4cbc](https://github.com/toggle-corp/toggle-monitor/commit/2be4cbc7dc62e7091ac2001583d1d70ff570ca0b))
- *(readme)* Public-launch polish - ([4bde872](https://github.com/toggle-corp/toggle-monitor/commit/4bde87263a69ae541baae489f6d302d44e095cf2))
- *(sample)* Status block + tags in deploy/local/config.sample.yaml - ([37ff606](https://github.com/toggle-corp/toggle-monitor/commit/37ff606529a6bdc2646ab56a33c227eeaac332e9))
- GHCR install snippet + tick "Push to Github" TODOs - ([fd09bf1](https://github.com/toggle-corp/toggle-monitor/commit/fd09bf113fe40959a494261ac64757cc995017e0))
- Drop incident date marker from CLAUDE.md and regression-guard comments - ([9f4316c](https://github.com/toggle-corp/toggle-monitor/commit/9f4316c35261f24642d951f3dcd971f4312f922e))
- AM-side integration guide for ADR-0005 webhook receiver - ([8b75494](https://github.com/toggle-corp/toggle-monitor/commit/8b754945b82e8b25535bcddac72764a01acd8261))
- ADR-0005 + config-schema/example for alertmanager webhook receiver - ([b03fc96](https://github.com/toggle-corp/toggle-monitor/commit/b03fc967476f4729ee261a850d126e5c9f0a507a))
- Add CLAUDE.md note on running integration tests - ([2028f52](https://github.com/toggle-corp/toggle-monitor/commit/2028f52753174b1e14b602985ec701dec8e94f6b))
- ADR-0004 + config-schema/example update for burst dispatcher - ([23af11d](https://github.com/toggle-corp/toggle-monitor/commit/23af11d196728edac22ec5d5c863075065a6131f))
- Add todo - ([a90608e](https://github.com/toggle-corp/toggle-monitor/commit/a90608e8172fb3fa012856fcd88fa34eb458f837))
- Top-level README + operations + architecture guides - ([382af3e](https://github.com/toggle-corp/toggle-monitor/commit/382af3e10c7482b7ddfbb3e34b62949ff195add5))
- Break v1 PRD into tracer-bullet issues - ([3daed33](https://github.com/toggle-corp/toggle-monitor/commit/3daed33fa9f20eebd35ecabca33eb2d29ab9af18))
- Add v1 PRD - ([2e5762f](https://github.com/toggle-corp/toggle-monitor/commit/2e5762f2082e74cc323baf0678f18b2443b8c09a))
- Grill-me documentation - ([5f0d460](https://github.com/toggle-corp/toggle-monitor/commit/5f0d460089fceb899f0be0a8a0e718326adcaf2b))
- Initial requirements - ([dd26b54](https://github.com/toggle-corp/toggle-monitor/commit/dd26b542cdd3d3ae486cfd7854a8e89de0401300))

#### 🎨 Styling

- *(slack)* Header out of color stripe; mentions inline; --cleanup flag - ([b4f4caf](https://github.com/toggle-corp/toggle-monitor/commit/b4f4caf078bb3008ac9eb979aabcc306807b329f))
- *(slack)* Render parent body as context block, not section - ([6f148ce](https://github.com/toggle-corp/toggle-monitor/commit/6f148cefda97683a22c68ff423f298295a3af54f))
- *(ui)* Issue count chip in the /issues page heading - ([58f0221](https://github.com/toggle-corp/toggle-monitor/commit/58f02210896ea514e0a2ee5f906c1ad8994a499d))
- *(ui)* Unify the kube-namespace chip across every monitor display - ([d1e8871](https://github.com/toggle-corp/toggle-monitor/commit/d1e8871d5067bb90e6cc8beaa961a3efe580c1a1))
- *(ui)* Widen latest-alerts name column, mono'd namespace prefix - ([0acc253](https://github.com/toggle-corp/toggle-monitor/commit/0acc2535300953d99d0f0ebe439aaca643ba145c))
- *(ui)* Human-readable durations in the config dialog - ([05bc63a](https://github.com/toggle-corp/toggle-monitor/commit/05bc63a9ab7eae7cad0cc80731565b0a024ebde4))
- *(ui)* Color-chip accepted status codes in config dialog - ([025b185](https://github.com/toggle-corp/toggle-monitor/commit/025b185ac5380e21ac7e18daa97c1b40cf8e7002))
- *(ui)* Pad + elevate the monitor-config dialog - ([6c58807](https://github.com/toggle-corp/toggle-monitor/commit/6c58807b96073f8520c71a7a267f7bd336a40074))
- *(ui)* Widen main container to fit the 8-column monitor table - ([f123700](https://github.com/toggle-corp/toggle-monitor/commit/f123700208397010041dd6ffd9c72e673025bd82))

#### 🧪 Testing

- *(helm)* Match the renamed chart in the schema-drift extractor - ([59d641d](https://github.com/toggle-corp/toggle-monitor/commit/59d641d28cc1ba0328a242cd5f4ae54931949cfe))
- *(merger)* Regex selector + nested final halt - ([501fdbe](https://github.com/toggle-corp/toggle-monitor/commit/501fdbeff2c71ab8994721cab43c0af992646d91))
- *(web)* Align status-page asserts with the current UI strings - ([6ebe742](https://github.com/toggle-corp/toggle-monitor/commit/6ebe742de0e4d55b9d6ce2c2e3c1e661178968cb))

#### ⚙️ Miscellaneous Tasks

- *(deploy)* Docker-compose autoreload via air - ([ffdb269](https://github.com/toggle-corp/toggle-monitor/commit/ffdb2691495f956fe26506b9dcc757c898ce6a3c))
- *(deploy)* Helm chart for k8s production - ([0350bfe](https://github.com/toggle-corp/toggle-monitor/commit/0350bfefb294e391620ae77ac879911715b4fa16))
- *(deploy)* Dockerfile + docker-compose local-dev stack - ([6ba309c](https://github.com/toggle-corp/toggle-monitor/commit/6ba309c58b320564d515ee39652c78cb6326634a))
- *(docs)* Quarantine internal working notes under docs/internal/ - ([df84b87](https://github.com/toggle-corp/toggle-monitor/commit/df84b871599ecb3da868a7d738a550918aefa4fd))
- *(fugit)* Bump pin to pick up CLAUDE.md hook-contract clarification - ([ae09f89](https://github.com/toggle-corp/toggle-monitor/commit/ae09f897b73d3b794e086d6de6bfd6356a2875fb))
- *(helm)* Bump chart version - ([21fc12c](https://github.com/toggle-corp/toggle-monitor/commit/21fc12c63651cf7434464f3788252e93828239eb))
- Gitignore local tmp/ scratch dir - ([e6f8a0b](https://github.com/toggle-corp/toggle-monitor/commit/e6f8a0b47fe6d421cd695407d05d77131aaf1cad))
- Drop docs/superpowers/ spec directory - ([4a913e0](https://github.com/toggle-corp/toggle-monitor/commit/4a913e0db66a779c126a3078ebe788f7480af7e5))
- Ignore .env and secrets/ - ([eeb7661](https://github.com/toggle-corp/toggle-monitor/commit/eeb7661c70bf42f3e02dc30098662b8c81bb6b2d))
- Clear pre-existing golangci-lint debt - ([171b103](https://github.com/toggle-corp/toggle-monitor/commit/171b1034888455370fb4fb7971b6b433fff5769a))
- Add dependabot.yml for gomod + actions + docker - ([eb0ad5c](https://github.com/toggle-corp/toggle-monitor/commit/eb0ad5cbd6ecec779fc00fc5ba10b5b4c910ff0c))
- Declare least-privilege permissions block - ([d2faca3](https://github.com/toggle-corp/toggle-monitor/commit/d2faca375be7877a75aafcedcefcbfdbd4122792))
- Add LICENSE (MIT) - ([13c142e](https://github.com/toggle-corp/toggle-monitor/commit/13c142e661c266709433386ba51c5a420da00d1b))
- Post-Task-4 cleanup (PresetSlug drop, sort import, dead code, 3-layer override test) - ([24fa8cf](https://github.com/toggle-corp/toggle-monitor/commit/24fa8cfed1feda7be2d1b11cc8c2dfd5b0949ba7))
- Golangci-lint v2 setup + clean tree - ([44b0eb8](https://github.com/toggle-corp/toggle-monitor/commit/44b0eb899a91117189fbc06bbbff9fc8c7ced08a))
- Migrate from Make to just - ([7f74295](https://github.com/toggle-corp/toggle-monitor/commit/7f7429587234f65487df6588e8ed4cf6489919b1))

#### Build

- *(docker)* Allowlist build context + explicit COPYs - ([173e3fa](https://github.com/toggle-corp/toggle-monitor/commit/173e3fa8ef84a9a9379af5a1a95414c49ea1d319))
- *(just)* Migrate + migrate-check recipes for the local stack - ([9552cfb](https://github.com/toggle-corp/toggle-monitor/commit/9552cfb16eecb717bfbb4754327b68b35611f1c9))

#### Todo

- List update - ([fd41acc](https://github.com/toggle-corp/toggle-monitor/commit/fd41acc6d1eb6ecf10e619296144ddf6415da3ab))

### 🍻 Pull Requests (1)
- (#3) [Feat/release](https://github.com/toggle-corp/toggle-monitor/pull/3)


<!-- generated by git-cliff -->
