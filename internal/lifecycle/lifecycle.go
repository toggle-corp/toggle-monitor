// Package lifecycle orchestrates startup ordering and graceful
// shutdown on SIGTERM (stop listener → cancel checks → flush DB).
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/coalesce"
	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/group"
	"github.com/toggle-corp/toggle-monitor/internal/heartbeat"
	"github.com/toggle-corp/toggle-monitor/internal/httpcheck"
	"github.com/toggle-corp/toggle-monitor/internal/kube"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/observability"
	"github.com/toggle-corp/toggle-monitor/internal/proxypool"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/selfhealth"
	tmsentry "github.com/toggle-corp/toggle-monitor/internal/sentry"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/smtpcheck"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web"
	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// ServeOptions parameterizes serve startup. CLI parses these from
// flags + env; tests construct them directly.
type ServeOptions struct {
	Config     config.Config
	DBConfig   db.Config
	ListenAddr string              // e.g. ":8080"
	Logger     *slog.Logger        // nil → slog.Default()
	OnReady    func(addr net.Addr) // optional, called once listener is bound

	// Release is stamped onto every Sentry event. CLI threads the
	// build-time version variable here; tests pass "" or a stub.
	Release string

	// SentryDSN, when non-empty AND Config.Sentry.Enabled, overrides
	// the env-var lookup. CLI does the env lookup itself; tests
	// inject DSNs directly without setenv.
	SentryDSN string

	// SlackBaseURL lets tests point the Slack client at an httptest
	// server. Empty → slack.DefaultBaseURL.
	SlackBaseURL string

	// KubeconfigPath, when set, drives client-go via the named file
	// instead of in-cluster ServiceAccount. Tests typically inject a
	// KubeIngressLister instead and skip this entirely.
	KubeconfigPath string

	// KubeIngressLister lets tests bypass NewWithCluster (which needs
	// a real cluster) and feed a hand-built lister directly. When
	// non-nil and Config.Kube != nil, it overrides KubeconfigPath.
	KubeIngressLister kube.IngressLister
}

// RunServe wires the worker, the HTTP server, and the DB connection
// pool together. It blocks until ctx is cancelled or an unrecoverable
// startup error occurs.
//
// Order of operations:
//  1. Connect to Postgres (exp backoff up to ~60s).
//  2. Verify the schema version matches the binary.
//  3. Reconcile every monitor from the YAML into the DB.
//  4. Start the HTTP server (listener bound, MarkReady).
//  5. Start the scheduler.
//  6. Block until ctx is cancelled, then shut down in reverse order.
func RunServe(ctx context.Context, opts ServeOptions) error {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	// Sentry first: any panic in subsequent startup steps should reach
	// the SDK. When disabled, sentryFlush is a no-op and Handler()
	// returns a no-op handler.
	sentryFlush, err := tmsentry.Init(buildSentryConfig(opts), opts.Release)
	if err != nil {
		return fmt.Errorf("sentry init: %w", err)
	}
	defer sentryFlush()
	// Wrap the caller's logger so ERROR records also go to Sentry.
	// The base handler keeps emitting to stdout regardless.
	log = slog.New(tmsentry.NewMultiHandler(log.Handler(), tmsentry.Handler()))

	pool, err := db.ConnectWithBackoff(ctx, opts.DBConfig, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	// pool is closed explicitly during the shutdown sequence below so
	// the ordering matches docs/design-decisions.md §Resilience &
	// lifecycle. A defer also fires on the error-return paths above.
	defer pool.Close()

	if err := migrate.Check(opts.DBConfig.DSN()); err != nil {
		return fmt.Errorf("schema version check: %w (run `toggle-monitor migrate`)", err)
	}

	repo := store.New(pool)

	// Resolve outbound proxies once at startup. Failures here are
	// operator-actionable (unset env var, bad address) and warrant a
	// clean shutdown rather than runtime surprises.
	proxies, err := proxypool.Build(opts.Config.Proxies)
	if err != nil {
		return fmt.Errorf("build proxy pool: %w", err)
	}

	// Build the Slack client + notifier up front so the monitor
	// reconcile pass can dispatch removal warnings + closeouts via it.
	channelByMonitor, tokens, err := resolveSlackTokens(opts.Config)
	if err != nil {
		return err
	}
	// metrics built early so the slack client + notifier can emit
	// retry / post / fresh-parent counters from their first call.
	metrics := observability.New()

	slackOpts := []slack.Option{slack.WithObserver(metrics)}
	if opts.SlackBaseURL != "" {
		slackOpts = append(slackOpts, slack.WithBaseURL(opts.SlackBaseURL))
	}
	slackClient := slack.NewClient(slackOpts...)

	// Workspace check: enforce single-workspace at startup; surface
	// transient failures via the cached state. Multi-workspace is
	// fatal.
	wsWatcher := slack.NewWorkspaceWatcher(tokens, slackClient, log)
	if err := wsWatcher.VerifyOnce(ctx); err != nil {
		return fmt.Errorf("slack workspace check: %w", err)
	}

	// Shared channel-slug → ChannelInfo resolver, used by both the
	// per-monitor notifier and the coalescing digest poster.
	channelLookup := func(slug string) (slack.ChannelInfo, bool) {
		info, ok := channelByMonitor[slug]
		return info, ok
	}

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client:            slackClient,
		Store:             repo,
		Channels:          channelLookup,
		BodyMaxChars:      opts.Config.Slack.BodyMaxChars,
		DependentsNoteMax: opts.Config.Slack.DependentsNoteMax,
		PublicBase:        opts.Config.PublicBaseURL,
		Logger:            log,
		Observer:          metrics,
	})

	// userMapping validator. v1 is single-workspace so picking any of
	// the resolved tokens is fine; tokenAny grabs the first
	// alphabetically by env-var name for determinism.
	tokenAny := func() string {
		keys := make([]string, 0, len(tokens))
		for k := range tokens {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			return ""
		}
		return tokens[keys[0]].Reveal()
	}
	umValidator := slack.NewUserMappingValidator(slackClient, opts.Config.Slack.UserMapping, tokenAny, log)
	// Best-effort startup verification; failures are cached for the UI.
	umValidator.VerifyOnce(ctx)

	// Reconcile YAML-declared static monitors into the DB, then
	// soft-delete any prior static monitor that is no longer
	// declared. Soft-delete fires the in-thread closeout + the
	// non-threaded "monitor removed" warning via the notifier.
	declared := make(map[string]struct{}, len(opts.Config.Monitors)+len(opts.Config.SMTPMonitors))
	for _, m := range opts.Config.Monitors {
		declared[m.Slug] = struct{}{}
		spec := store.MonitorSpec{
			Slug:             m.Slug,
			Kind:             store.KindHTTP,
			FriendlyName:     m.FriendlyName,
			URL:              m.URL,
			Source:           store.SourceStatic,
			DependsOn:        m.DependsOn,
			SlackChannelSlug: m.Slack,
			Tags:             m.Tags,
		}
		if err := repo.ReconcileMonitor(ctx, spec); err != nil {
			return fmt.Errorf("reconcile %q: %w", m.Slug, err)
		}
	}
	for _, m := range opts.Config.SMTPMonitors {
		declared[m.Slug] = struct{}{}
		spec := store.MonitorSpec{
			Slug:             m.Slug,
			Kind:             store.KindSMTP,
			FriendlyName:     m.FriendlyName,
			URL:              m.URL(),
			Host:             m.Host,
			Port:             m.Port,
			TLSMode:          m.TLSMode(),
			Source:           store.SourceStatic,
			DependsOn:        m.DependsOn,
			SlackChannelSlug: m.Slack,
			Tags:             m.Tags,
		}
		if err := repo.ReconcileMonitor(ctx, spec); err != nil {
			return fmt.Errorf("reconcile %q: %w", m.Slug, err)
		}
	}
	priorStatic, err := repo.ListActiveBySource(ctx, store.SourceStatic)
	if err != nil {
		log.Warn("list prior static monitors", "error", err)
	}
	for _, m := range priorStatic {
		if _, kept := declared[m.Slug]; kept {
			continue
		}
		view := monitorViewFromRow(m)
		if err := repo.SoftDeleteMonitor(ctx, m.Slug, "removed from config"); err != nil {
			log.Warn("soft-delete missing monitor", "slug", m.Slug, "error", err)
			continue
		}
		log.Info("monitor removed from config (soft-deleted)", "slug", m.Slug, "was_status", m.Status)
		if m.SlackChannelSlug != "" {
			notifier.NotifyRemoved(ctx, m.SlackChannelSlug, view, "removed from config", "static config")
		}
	}

	// Heartbeat source: pulls open-incidents from the store and the
	// last-tick gauge from metrics.
	hbSource := &heartbeatSource{repo: repo, metrics: metrics}

	srv := web.New(repo, log)
	srv.SetMetricsHandler(metrics.Handler())
	srv.SetMappingReader(&mappingAdapter{v: umValidator})
	srv.SetPageSizes(web.PageSizes{
		HomepageAlerts:   opts.Config.UI.PageSize.HomepageAlerts,
		MonitorListing:   opts.Config.UI.PageSize.MonitorListing,
		MonitorHistory:   opts.Config.UI.PageSize.MonitorHistory,
		DiscoveryListing: opts.Config.UI.PageSize.DiscoveryListing,
		MaxPerPage:       opts.Config.UI.MaxPerPage,
	})
	{
		ds := templates.DiscoveryStatus{KubeEnabled: opts.Config.Kube != nil}
		if ds.KubeEnabled {
			ds.ResyncInterval = opts.Config.Kube.ResyncInterval.AsDuration()
		}
		srv.SetDiscoveryStatus(ds)
	}
	if len(opts.Config.StatusPages) > 0 {
		configs := make([]*templates.StatusConfig, 0, len(opts.Config.StatusPages))
		for _, sc := range opts.Config.StatusPages {
			tc := &templates.StatusConfig{
				Slug:         sc.Slug,
				FriendlyName: sc.FriendlyName,
				Description:  sc.Description,
				LogoURL:      sc.LogoURL,
				Color:        sc.Color,
			}
			for _, sec := range sc.Sections {
				tc.Sections = append(tc.Sections, templates.StatusConfigSection{
					Title: sec.Title,
					Match: compileSectionMatch(sec.Match),
				})
			}
			configs = append(configs, tc)
		}
		srv.SetStatusConfigs(configs)
	}
	// Alertmanager webhook receiver (ADR-0005). Constructed and
	// registered before Routes() is invoked so the listener actually
	// serves /webhooks/<slug> when cfg.Alertmanager is set. Absent
	// block → no handler, no route, no sweeper (the goroutine spawn
	// below is also gated on a nil sweeper).
	amHandler, err := buildAMHandler(
		opts.Config.Alertmanager,
		opts.Config.Slack.UserMapping,
		repo,
		slackClient,
		channelLookup,
		metrics,
		opts.Config.PublicBaseURL,
		log,
	)
	if err != nil {
		return fmt.Errorf("alertmanager handler init: %w", err)
	}
	if amHandler != nil {
		srv.RegisterRoute("POST "+opts.Config.Alertmanager.Endpoint.Path, amHandler)
	}
	amSweeper := buildAMSweeper(opts.Config.Alertmanager, repo, log)

	listener, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("bind listen address %q: %w", opts.ListenAddr, err)
	}
	httpServer := &http.Server{
		Handler:           tmsentry.HTTPMiddleware(srv.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if opts.OnReady != nil {
		opts.OnReady(listener.Addr())
	}

	// Kube materializer (nil when Config.Kube isn't set) is built
	// here so its CurrentPlans() is reachable from the scheduler's
	// dynamic plan source below.
	var materializer *merger.Materializer
	if opts.Config.Kube != nil {
		materializer = merger.New(repo, opts.Config, proxies)
		materializer.SetLogger(log)
	}

	// Alert coalescing: non-critical uptime opens/resolves route into a
	// living per-channel digest instead of one Slack message per
	// monitor. The manager owns the in-memory groups + persistence; a
	// central evaluator goroutine (below) drives them on wall-clock
	// time. Reattach any open groups from a prior process so deltas edit
	// the existing digest instead of re-storming.
	// Effective* accessors collapse the deprecated groupWait alias into
	// pendingWait and fill defaults at the config layer. PendingWait
	// is the dispatcher's wait window (ADR-0004); the legacy
	// group.Config.GroupWait is set to 0 because the dispatcher's
	// pre-warmed promotion bypasses the group's own wait via Group.Open.
	groupMgr := coalesce.New(coalesce.Options{
		Store:  repo,
		Poster: &digestPoster{client: slackClient, channels: channelLookup},
		// Sink is the individual-notification path: every sub-threshold
		// non-critical failure (the 90% case) flushes through it. It is
		// the same notifier closure the scheduler's critical EventSink
		// uses — EventSink and coalesce.Sink share an identical
		// signature. Omitting it silently discards all routine alerts
		// (a past regression); the SinkWired guard below makes that
		// omission fatal at boot rather than silent in production.
		Sink: coalesce.Sink(buildSink(notifier)),
		Config: group.Config{
			GroupInterval:  opts.Config.Slack.Coalesce.EffectiveGroupInterval(),
			RepeatInterval: opts.Config.Slack.Coalesce.EffectiveRepeatInterval(),
		},
		PendingWait:    opts.Config.Slack.Coalesce.EffectivePendingWait(),
		BurstThreshold: opts.Config.Slack.Coalesce.EffectiveBurstThreshold(),
		GroupMention:   opts.Config.Slack.Coalesce.EffectiveGroupMention(),
		Logger:         log,
	})
	// Fail fast: the daemon must never run with a nil individual sink —
	// it would silently swallow every sub-threshold non-critical alert.
	if !groupMgr.SinkWired() {
		return fmt.Errorf("coalesce dispatcher built without an individual notification sink: every sub-threshold non-critical alert would be silently dropped (refusing to start)")
	}
	if w := config.DependsOnIntervalWarnings(opts.Config); len(w) > 0 {
		for _, line := range w {
			log.Warn("dependsOn parent slower than child", "detail", line)
		}
	}
	if err := groupMgr.Reattach(ctx); err != nil {
		log.Warn("reattach incident groups", "error", err)
	}

	// planSource is constructed empty here so the push-propagation
	// closure below can capture the pointer; the static / materializer
	// fields are filled in once the plans are built. This ordering lets
	// us pass scheduler.WithPushPropagation at scheduler construction
	// without a later setter.
	planSource := &combinedPlanSource{}
	pushPropagation := makePushPropagation(repo, groupMgr, planSource, log)
	// The on-demand probe captures groupMgr too (so it can Route the
	// parent's failure back through the dispatcher), creating the
	// usual chicken-and-egg around constructor injection. Set
	// post-construction; safe before the evaluator goroutine starts.
	groupMgr.SetOnDemandParentProbe(makeOnDemandParentProbe(
		repo, groupMgr, planSource, pushPropagation,
		opts.Config.Slack.Coalesce.EffectiveOnDemandProbeTimeout(),
		time.Now,
		log,
	))

	// Self-health degraded mode (ADR-0008). When the selfHealth block is
	// present, a shared detector aggregates every tick's outcome; a
	// FailKindDNS tick is held provisional by the scheduler (no
	// alert.Apply / dispatch) while the monitor may be blind. A dedicated
	// evaluator loop (started below) decides once per window whether to
	// commit isolated failures or suppress a real blindness storm behind
	// one self-health notice. nil detector → feature disabled (DNS
	// failures commit immediately, as before).
	var selfHealthDet *selfhealth.Detector
	var selfHealthN *selfHealthNotifier
	if sh := opts.Config.SelfHealth; sh != nil {
		selfHealthDet = selfhealth.New(selfhealth.Config{
			Window:      sh.EffectiveWindow(),
			MinMonitors: sh.EffectiveMinMonitors(),
		})
		commit := makeSelfHealthCommit(repo, groupMgr, planSource, pushPropagation, notifier, time.Now, log)
		selfHealthN = newSelfHealthNotifier(
			selfHealthDet,
			&digestPoster{client: slackClient, channels: channelLookup},
			sh.Channel,
			sh.Mention,
			metrics,
			commit,
			log,
		)
	}

	schedOpts := []scheduler.Option{
		scheduler.WithLogger(log),
		scheduler.WithEventSink(buildSink(notifier)),
		scheduler.WithSSLSink(buildSSLSink(notifier)),
		scheduler.WithGroupSink(groupSinkAdapter{m: groupMgr}),
		scheduler.WithPushPropagation(pushPropagation),
		scheduler.WithMetrics(metrics),
	}
	if selfHealthDet != nil {
		schedOpts = append(schedOpts, scheduler.WithSelfHealth(selfHealthDet))
	}
	sched := scheduler.New(repo, schedOpts...)
	srv.SetMissingParentReader(&missingParentAdapter{s: sched})
	if materializer != nil {
		srv.SetAnnotationIssueReader(&annotationIssueAdapter{m: materializer})
	}
	staticPlans := buildPlans(opts.Config, proxies)
	idToSlug := make(map[string]string, len(opts.Config.Slack.UserMapping))
	for slug, id := range opts.Config.Slack.UserMapping {
		// First slug wins on collision — userMapping is operator-curated
		// so duplicates are rare; keeping it deterministic via "first
		// seen" is enough for a display-only reverse lookup.
		if _, dup := idToSlug[id]; !dup {
			idToSlug[id] = slug
		}
	}
	planSource.static = staticPlans
	planSource.materializer = materializer
	planSource.idToSlug = idToSlug
	srv.SetConfigLookup(planSource)

	var wg sync.WaitGroup
	// HTTP server.
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("http server listening", "addr", listener.Addr().String())
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
		}
	}()

	srv.MarkReady()

	// Scheduler. RunDynamic re-evaluates the plan set every
	// refreshInterval so newly-materialized kube monitors are picked
	// up without restarting the worker. The refresh is a cheap
	// in-memory plan diff (slug-keyed map lookup, then reflect.DeepEqual
	// per kept slug) — independent of the watcher's k8s-API resync,
	// which is bounded by API cost and may be tens of minutes. A
	// previous version of this code yoked the two together, which made
	// kube monitors invisible to the scheduler for up to ResyncInterval
	// after they were materialized.
	refresh := 30 * time.Second
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.RunDynamic(ctx, planSource, refresh)
	}()

	// Central coalescing evaluator: advances every live group on
	// wall-clock time (independent of per-monitor ticks) and dispatches
	// the resulting digest posts/edits/replies. A short cadence keeps
	// group_wait (30s) and recovery rendering responsive without busy
	// work — each tick is an in-memory pass plus one DB upsert per
	// active group.
	wg.Add(1)
	go func() {
		defer wg.Done()
		groupMgr.RunEvaluator(ctx, 5*time.Second)
	}()

	// Self-health evaluator (ADR-0008): decides enter/exit once per
	// window and drives the single self-health notice. Only started when
	// the selfHealth block is configured.
	if selfHealthN != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selfHealthN.run(ctx, opts.Config.SelfHealth.EffectiveWindow())
		}()
	}

	// Hourly workspace re-check.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wsWatcher.Run(ctx, time.Hour)
	}()

	// 24h userMapping re-validation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		umValidator.Run(ctx, 24*time.Hour)
	}()

	// AM retention sweeper. Hardcoded 24h cadence (see ADR-0005
	// §"Persistence"). When the alertmanager block is absent the
	// sweeper is nil and the goroutine isn't started; when present
	// but RetentionDays == 0 the sweeper's Run logs disabled and
	// exits immediately, which still satisfies the wg balance.
	if amSweeper != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			amSweeper.Run(ctx)
		}()
	}

	// Kube auto-discovery: the materializer was built above so the
	// scheduler's plan source can read its CurrentPlans(). Here we
	// wire the actual watcher loop + the removal sink that runs
	// when an ingress disappears.
	if kc := opts.Config.Kube; kc != nil {
		removalSink := &kubeRemovalSink{repo: repo, notifier: notifier, log: log}
		kubeOpts := kube.Options{
			ResyncInterval: kc.ResyncInterval.AsDuration(),
			Materializer:   materializer,
			RemovalSink:    removalSink,
			Logger:         log,
		}
		var watcher *kube.Watcher
		var lister kube.IngressLister
		if opts.KubeIngressLister != nil {
			lister = opts.KubeIngressLister
			watcher = kube.New(repo, lister, kubeOpts)
		} else {
			watcher, err = kube.NewWithCluster(ctx, repo, kubeOpts, opts.KubeconfigPath)
			if err != nil {
				return fmt.Errorf("kube watcher: %w", err)
			}
			lister = watcher.Lister()
		}
		// Namespace annotations reach the cascade through the watcher's
		// informer. Wired after construction because the watcher owns
		// the informer and the materializer is built earlier.
		if materializer != nil {
			materializer.SetNamespaceAnnotationSource(watcher)
		}
		// Wire the cascade source so the discovery detail page can
		// re-run merger.ResolveWithTrace against the live cache.
		srv.SetCascadeSource(&cascadeSource{lister: lister, rules: kc.Match, mat: materializer})
		wg.Add(1)
		go func() {
			defer wg.Done()
			watcher.Run(ctx)
		}()
	}

	// Optional outbound heartbeat. When the YAML omits the block we
	// skip starting the loop entirely.
	var hb *heartbeat.Heartbeat
	if hbCfg := opts.Config.Heartbeat; hbCfg != nil {
		hb = heartbeat.New(heartbeat.Options{
			URL:                 hbCfg.URL,
			Interval:            hbCfg.Interval.AsDuration(),
			FailOnStalledWorker: hbCfg.FailOnStalledWorker,
			Source:              hbSource,
			Logger:              log,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			hb.Run(ctx)
		}()
	}

	<-ctx.Done()
	log.Info("shutdown signal received")

	// Graceful shutdown ordering per docs/design-decisions.md §Resilience
	// & lifecycle:
	//   1. Stop accepting new HTTP requests.
	//   2. Cancel in-flight checks via ctx (already done — ctx is the
	//      parent of every per-monitor goroutine).
	//   3. Cancel the ingress informer (Issue 8 onwards; same ctx).
	//   4. Wait for in-flight goroutines (scheduler, workspace watcher,
	//      heartbeat) to drain.
	//   5. Flush pending DB writes / close pool.
	//   6. Emit the final shutdown heartbeat.
	//   7. Return 0.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
	wg.Wait()
	pool.Close()
	if hb != nil {
		hb.SendShutdown(shutdownCtx)
	}
	log.Info("shutdown complete")
	return nil
}

// buildSentryConfig flattens the optional ServeOptions.Config.Sentry
// block into the internal/sentry.Config the SDK needs. Returns a
// zero (Enabled=false) Config when the YAML block is absent, which
// is how the bridge stays no-op for operators who don't run Sentry.
//
// Defaulting rules:
//   - SampleRate: 0 → 1.0 (so omitting the field doesn't silently
//     drop every event).
//   - Environment: "" → "production".
//   - DSN: opts.SentryDSN (CLI-resolved env var); empty + Enabled is
//     a startup-time error caught upstream in cli/serve.go.
func buildSentryConfig(opts ServeOptions) tmsentry.Config {
	if opts.Config.Sentry == nil || !opts.Config.Sentry.Enabled {
		return tmsentry.Config{}
	}
	s := opts.Config.Sentry
	env := s.Environment
	if env == "" {
		env = "production"
	}
	sampleRate := s.SampleRate
	if sampleRate == 0 {
		sampleRate = 1.0
	}
	return tmsentry.Config{
		Enabled:          true,
		DSN:              opts.SentryDSN,
		Environment:      env,
		SampleRate:       sampleRate,
		TracesSampleRate: s.TracesSampleRate,
		ServerName:       s.ServerName,
	}
}

// compileSectionMatch converts a config.SectionMatch (which carries
// hostRegex as a string) into a templates.StatusMatch with the
// regex pre-compiled. Config validation guarantees every hostRegex
// is a valid Go regexp, so MustCompile-via-Compile-checked is safe
// here; on the unexpected error path we fall back to an empty
// matcher so a single bad regex doesn't crash the server.
func compileSectionMatch(m config.SectionMatch) templates.StatusMatch {
	out := templates.StatusMatch{
		Tags: append([]string(nil), m.Tags...),
	}
	if m.HostRegex != "" {
		if re, err := regexp.Compile("^(?:" + m.HostRegex + ")$"); err == nil {
			out.HostRegex = re
		}
	}
	for _, c := range m.Any {
		out.Any = append(out.Any, compileSectionMatch(c))
	}
	for _, c := range m.All {
		out.All = append(out.All, compileSectionMatch(c))
	}
	return out
}

func buildPlans(cfg config.Config, proxies *proxypool.Pool) []scheduler.Plan {
	out := make([]scheduler.Plan, 0, len(cfg.Monitors)+len(cfg.SMTPMonitors))
	for _, m := range cfg.Monitors {
		isHTTPS := strings.HasPrefix(m.URL, "https://")
		out = append(out, scheduler.Plan{
			Slug:                m.Slug,
			Kind:                "http",
			FriendlyName:        m.FriendlyName,
			URL:                 m.URL,
			HTTPMethod:          m.HTTPMethod,
			AcceptedStatusCodes: m.AcceptedStatusCodes,
			Prober: httpcheck.Config{
				URL:                   m.URL,
				Method:                m.HTTPMethod,
				AcceptedStatusCodes:   m.AcceptedStatusCodes,
				Timeout:               m.Timeout.AsDuration(),
				FollowRedirects:       m.FollowRedirects,
				TLSInsecureSkipVerify: m.TLSInsecureSkipVerify,
				ProxyDialer:           proxies.Get(m.Proxy),
				UserAgent:             cfg.HTTPClient.UserAgent,
			},
			Interval:               m.Interval.AsDuration(),
			Timeout:                m.Timeout.AsDuration(),
			Retries:                m.Retries,
			RetryBackoff:           m.RetryBackoff.AsDuration(),
			FollowRedirects:        m.FollowRedirects,
			TLSInsecureSkipVerify:  m.TLSInsecureSkipVerify,
			ProxyDialer:            proxies.Get(m.Proxy),
			Proxy:                  m.Proxy,
			UserAgent:              cfg.HTTPClient.UserAgent,
			ReminderInterval:       m.ReminderInterval.AsDuration(),
			ChannelSlug:            m.Slack,
			Mentions:               slack.ResolveMentions(m.Notify, cfg.Slack.UserMapping),
			DependsOn:              m.DependsOn,
			Critical:               m.Critical,
			TLSBearing:             isHTTPS,
			SSLAlertThreshold:      m.SSLAlertThreshold.AsDuration(),
			SSLEscalationThreshold: m.SSLEscalationThreshold.AsDuration(),
			SSLReminderInterval:    m.SSLReminderInterval.AsDuration(),
		})
	}
	for _, m := range cfg.SMTPMonitors {
		tlsMode := m.TLSMode()
		tlsBearing := tlsMode != smtpcheck.TLSNone
		out = append(out, scheduler.Plan{
			Slug:         m.Slug,
			Kind:         "smtp",
			FriendlyName: m.FriendlyName,
			URL:          m.URL(),
			Host:         m.Host,
			Port:         m.Port,
			TLSMode:      tlsMode,
			Prober: smtpcheck.Config{
				Host:               m.Host,
				Port:               m.Port,
				TLSMode:            tlsMode,
				EHLOName:           m.EHLOName,
				Timeout:            m.Timeout.AsDuration(),
				InsecureSkipVerify: m.TLSInsecureSkipVerify,
				ProxyDialer:        proxies.Get(m.Proxy),
			},
			Interval:               m.Interval.AsDuration(),
			Timeout:                m.Timeout.AsDuration(),
			Retries:                m.Retries,
			RetryBackoff:           m.RetryBackoff.AsDuration(),
			TLSInsecureSkipVerify:  m.TLSInsecureSkipVerify,
			ProxyDialer:            proxies.Get(m.Proxy),
			Proxy:                  m.Proxy,
			ReminderInterval:       m.ReminderInterval.AsDuration(),
			ChannelSlug:            m.Slack,
			Mentions:               slack.ResolveMentions(m.Notify, cfg.Slack.UserMapping),
			DependsOn:              m.DependsOn,
			Critical:               m.Critical,
			TLSBearing:             tlsBearing,
			SSLAlertThreshold:      m.SSLAlertThreshold.AsDuration(),
			SSLEscalationThreshold: m.SSLEscalationThreshold.AsDuration(),
			SSLReminderInterval:    m.SSLReminderInterval.AsDuration(),
		})
	}
	return out
}

// combinedPlanSource concatenates the static-config plans (immutable
// for the run) with the materializer's current kube plans (changes
// every kube reconcile). userMapping powers the reverse-lookup that
// renders mentions as "<slug> U…" in the config dialog.
type combinedPlanSource struct {
	static       []scheduler.Plan
	materializer *merger.Materializer
	idToSlug     map[string]string // U…/S… → slug; built once from cfg.Slack.UserMapping
}

func (c *combinedPlanSource) CurrentPlans() []scheduler.Plan {
	if c.materializer == nil {
		return c.static
	}
	kube := c.materializer.CurrentPlans()
	out := make([]scheduler.Plan, 0, len(c.static)+len(kube))
	out = append(out, c.static...)
	out = append(out, kube...)
	return out
}

// ConfigFor satisfies web.ConfigLookup: the monitor detail page
// renders the live plan so static + kube monitors look identical to
// the operator. Returns (_, false) for slugs we don't currently have
// a plan for (e.g. a kube monitor whose ingress was pruned this pass
// and the soft-delete hasn't landed yet).
func (c *combinedPlanSource) ConfigFor(slug string) (templates.MonitorConfig, bool) {
	for _, p := range c.CurrentPlans() {
		if p.Slug != slug {
			continue
		}
		return templates.MonitorConfig{
			Preset:                 p.Preset,
			HTTPMethod:             p.HTTPMethod,
			AcceptedStatusCodes:    append([]int(nil), p.AcceptedStatusCodes...),
			Interval:               p.Interval,
			Timeout:                p.Timeout,
			Retries:                p.Retries,
			RetryBackoff:           p.RetryBackoff,
			FollowRedirects:        p.FollowRedirects,
			TLSInsecureSkipVerify:  p.TLSInsecureSkipVerify,
			Proxy:                  p.Proxy,
			UserAgent:              p.UserAgent,
			ReminderInterval:       p.ReminderInterval,
			SlackChannelSlug:       p.ChannelSlug,
			Mentions:               displayMentions(p.Mentions, c.idToSlug),
			IsHTTPS:                p.TLSBearing,
			SSLAlertThreshold:      p.SSLAlertThreshold,
			SSLEscalationThreshold: p.SSLEscalationThreshold,
			SSLReminderInterval:    p.SSLReminderInterval,
		}, true
	}
	return templates.MonitorConfig{}, false
}

// displayMentions parses each resolved Slack mention back into a
// (slug, ID, raw-marker) triple for the config dialog. Inverse of
// slack.ResolveMentions, modulo the slug being best-effort —
// userMapping can be edited between resolution and display, and an
// unknown ID just renders without a slug prefix.
func displayMentions(mentions []string, idToSlug map[string]string) []templates.MentionDisplay {
	if len(mentions) == 0 {
		return nil
	}
	out := make([]templates.MentionDisplay, 0, len(mentions))
	for _, m := range mentions {
		switch {
		case strings.HasPrefix(m, "<@") && strings.HasSuffix(m, ">"):
			id := strings.TrimSuffix(strings.TrimPrefix(m, "<@"), ">")
			out = append(out, templates.MentionDisplay{Slug: idToSlug[id], ID: id})
		case strings.HasPrefix(m, "<!subteam^") && strings.HasSuffix(m, ">"):
			id := strings.TrimSuffix(strings.TrimPrefix(m, "<!subteam^"), ">")
			out = append(out, templates.MentionDisplay{Slug: idToSlug[id], ID: id})
		default:
			out = append(out, templates.MentionDisplay{Raw: m})
		}
	}
	return out
}

// mappingAdapter converts slack.UserMappingValidator.Snapshot() into
// the web.MappingHealthReader shape. Keeps the web package free of a
// hard slack import.
type mappingAdapter struct{ v *slack.UserMappingValidator }

func (a *mappingAdapter) Snapshot() (entries []web.MappingEntry, lastRun time.Time) {
	if a == nil || a.v == nil {
		return nil, time.Time{}
	}
	src, run := a.v.Snapshot()
	out := make([]web.MappingEntry, 0, len(src))
	for _, e := range src {
		out = append(out, web.MappingEntry{
			Slug: e.Slug, ID: e.ID, OK: e.OK, Reason: e.Reason, Checked: e.Checked,
		})
	}
	return out, run
}

// missingParentAdapter converts scheduler.Scheduler.MissingParents()
// into the web.MissingParentReader shape. Keeps the web package free
// of a hard scheduler import.
type missingParentAdapter struct{ s *scheduler.Scheduler }

func (a *missingParentAdapter) MissingParents() []web.MissingParent {
	if a == nil || a.s == nil {
		return nil
	}
	src := a.s.MissingParents()
	out := make([]web.MissingParent, 0, len(src))
	for _, mp := range src {
		out = append(out, web.MissingParent{
			Parent: mp.Parent, Children: mp.Children, LastSeen: mp.LastSeen,
		})
	}
	return out
}

// annotationIssueAdapter flattens the materializer's per-monitor record
// of rejected annotation values into the one-line-per-value shape
// /issues renders.
type annotationIssueAdapter struct {
	m *merger.Materializer
}

func (a *annotationIssueAdapter) AnnotationIssues() []web.AnnotationIssue {
	var out []web.AnnotationIssue
	for _, mw := range a.m.AnnotationWarnings() {
		for _, w := range mw.Warnings {
			out = append(out, web.AnnotationIssue{
				Slug:        mw.Slug,
				Namespace:   mw.Namespace,
				IngressName: mw.IngressName,
				Host:        mw.Host,
				Field:       w.Field,
				Key:         w.Key,
				Scope:       w.Scope,
				Value:       w.Value,
				Reason:      w.Reason,
			})
		}
	}
	return out
}

// cascadeSource implements web.CascadeSource by combining the kube
// informer's IngressLister with the currently-loaded kube.match[]
// tree. The discovery detail page calls into this seam to re-run
// merger.ResolveWithTrace against the live cluster state on every
// render — no schema additions, no stale trace risk.
type cascadeSource struct {
	lister kube.IngressLister
	rules  []config.KubeMatchRule
	// mat supplies the annotation environment. Nil when the kube block
	// parsed but no materializer could be built, in which case the
	// recompute falls back to literals-only resolution.
	mat *merger.Materializer
}

func (c *cascadeSource) GetIngress(namespace, name string) (*networkingv1.Ingress, error) {
	ing, err := c.lister.Get(namespace, name)
	if err != nil {
		if errors.Is(err, kube.ErrIngressNotFound) {
			return nil, web.ErrIngressNotInCascadeSource
		}
		return nil, err
	}
	return ing, nil
}

func (c *cascadeSource) MatchRules() []config.KubeMatchRule { return c.rules }

func (c *cascadeSource) ResolveEnv(namespace string) merger.Env {
	if c.mat == nil {
		return merger.Env{}
	}
	return c.mat.ResolveEnv(namespace)
}

// kubeRemovalSink dispatches the same soft-delete + Slack closeout +
// warning flow used for static removals, against monitors materialized
// from a now-disappeared Ingress.
type kubeRemovalSink struct {
	repo     *store.Repo
	notifier *slack.Notifier
	log      *slog.Logger
}

func (k *kubeRemovalSink) OnKubeMonitorRemoved(ctx context.Context, monitorSlug, reason string) {
	row, err := k.repo.GetMonitor(ctx, monitorSlug)
	if err != nil {
		// May already be archived from a prior pass, or never made it
		// into the table (slug-failure snapshot rows skip
		// ReconcileMonitor) — neither is fatal.
		k.log.Warn("kube removal: monitor lookup", "slug", monitorSlug, "error", err)
		return
	}
	if row.Archived {
		return
	}
	view := monitorViewFromRow(row)
	if err := k.repo.SoftDeleteMonitor(ctx, monitorSlug, reason); err != nil {
		k.log.Warn("kube removal: soft-delete", "slug", monitorSlug, "error", err)
		return
	}
	k.log.Info("kube monitor removed (soft-deleted)", "slug", monitorSlug, "was_status", row.Status, "reason", reason)
	if row.SlackChannelSlug != "" {
		k.notifier.NotifyRemoved(ctx, row.SlackChannelSlug, view, reason, "k8s ingress")
	}
}

// heartbeatSource adapts the store + observability to heartbeat.Source.
type heartbeatSource struct {
	repo    *store.Repo
	metrics *observability.Metrics
}

func (h *heartbeatSource) LastTick() time.Time {
	return h.metrics.LastTick()
}

func (h *heartbeatSource) OpenIncidents(ctx context.Context) (int, error) {
	return h.repo.CountOpenIncidents(ctx)
}

// resolveSlackTokens reads each channel's tokenEnv from the process
// environment and builds the lookup maps the notifier and the
// workspace watcher need. Returns an error if any tokenEnv resolves
// to an empty value.
func resolveSlackTokens(cfg config.Config) (
	channelByMonitor map[string]slack.ChannelInfo,
	tokensByEnv map[string]secret.SecretString,
	err error,
) {
	channelByMonitor = make(map[string]slack.ChannelInfo, len(cfg.Slack.Channels))
	tokensByEnv = make(map[string]secret.SecretString)
	for _, ch := range cfg.Slack.Channels {
		raw := os.Getenv(ch.TokenEnv)
		if raw == "" {
			return nil, nil, fmt.Errorf("slack channel %q: env var %q is unset or empty", ch.Slug, ch.TokenEnv)
		}
		tok := secret.SecretString(raw)
		channelByMonitor[ch.Slug] = slack.ChannelInfo{ID: ch.ChannelID, Token: tok}
		tokensByEnv[ch.TokenEnv] = tok
	}
	return channelByMonitor, tokensByEnv, nil
}

// buildSSLSink turns the slack.Notifier into the scheduler.SSLSink
// shape so the scheduler stays free of a hard slack dep.
func buildSSLSink(n *slack.Notifier) scheduler.SSLSink {
	return func(ctx context.Context, row store.MonitorRow, channelSlug string, mentions []string, event *alert.SSLEvent) error {
		mv := monitorViewFromRow(row)
		ssl := slack.SSLView{ExpiresAt: event.ExpiresAt}
		return n.NotifySSL(ctx, channelSlug, mentions, mv, ssl, event)
	}
}

// monitorViewFromRow is the common projection. Shared by the uptime
// and SSL sinks so both see the same SSL/uptime thread refs.
func monitorViewFromRow(row store.MonitorRow) slack.MonitorView {
	mv := slack.MonitorView{
		Slug:         row.Slug,
		FriendlyName: row.FriendlyName,
		Tags:         row.Tags,
		URL:          row.URL,
	}
	if row.OpenedAt != nil {
		mv.OpenedAt = *row.OpenedAt
	}
	if row.LastStatusCode != nil {
		mv.StatusCode = *row.LastStatusCode
	}
	if row.LastError != nil {
		mv.LastError = *row.LastError
	}
	if row.UptimeThreadChannel != nil {
		mv.UptimeThreadChannel = *row.UptimeThreadChannel
	}
	if row.UptimeThreadTS != nil {
		mv.UptimeThreadTS = *row.UptimeThreadTS
	}
	if row.SSLThreadChannel != nil {
		mv.SSLThreadChannel = *row.SSLThreadChannel
	}
	if row.SSLThreadTS != nil {
		mv.SSLThreadTS = *row.SSLThreadTS
	}
	if row.SSLIssuer != nil {
		mv.SSLIssuer = *row.SSLIssuer
	}
	if row.SSLSubject != nil {
		mv.SSLSubject = *row.SSLSubject
	}
	mv.StatusText = http.StatusText(mv.StatusCode)
	return mv
}

// buildSink turns the slack.Notifier into the scheduler.EventSink
// shape (which deliberately doesn't import slack).
func buildSink(n *slack.Notifier) scheduler.EventSink {
	return func(ctx context.Context, row store.MonitorRow, channelSlug string, mentions []string, event *alert.Event) error {
		mv := monitorViewFromRow(row)
		// Open: the event carries the fresh status/error that's about
		// to be persisted. Resolve: the row has the just-cleared but
		// still relevant last fields.
		if event.StatusCode != 0 {
			mv.StatusCode = event.StatusCode
		}
		if event.Error != "" {
			mv.LastError = event.Error
		}
		mv.StatusText = http.StatusText(mv.StatusCode)
		return n.Notify(ctx, channelSlug, mentions, mv, event)
	}
}
