// Package lifecycle orchestrates startup ordering and graceful
// shutdown on SIGTERM (stop listener → cancel checks → flush DB).
//
// Issue 2 scope: the happy path of load-config → connect-DB →
// check-schema-version → reconcile-monitors → run-scheduler+web.
// The complete SIGTERM ordering (including the final heartbeat POST)
// lands in Issue 16.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/heartbeat"
	"github.com/toggle-corp/toggle-monitor/internal/kube"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/observability"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web"
)

// ServeOptions parameterizes serve startup. CLI parses these from
// flags + env; tests construct them directly.
type ServeOptions struct {
	Config     config.Config
	DBConfig   db.Config
	ListenAddr string              // e.g. ":8080"
	Logger     *slog.Logger        // nil → slog.Default()
	OnReady    func(addr net.Addr) // optional, called once listener is bound

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

	// Build the Slack client + notifier up front so the monitor
	// reconcile pass can dispatch removal warnings + closeouts via it.
	channelByMonitor, tokens, err := resolveSlackTokens(opts.Config)
	if err != nil {
		return err
	}
	slackOpts := []slack.Option{}
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

	notifier := slack.NewNotifier(slack.NotifierOptions{
		Client: slackClient,
		Store:  repo,
		Channels: func(slug string) (slack.ChannelInfo, bool) {
			info, ok := channelByMonitor[slug]
			return info, ok
		},
		BodyMaxChars:      opts.Config.Slack.BodyMaxChars,
		DependentsNoteMax: opts.Config.Slack.DependentsNoteMax,
		PublicBase:        opts.Config.PublicBaseURL,
		Logger:            log,
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
	declared := make(map[string]struct{}, len(opts.Config.Monitors))
	for _, m := range opts.Config.Monitors {
		declared[m.Slug] = struct{}{}
		spec := store.MonitorSpec{
			Slug:             m.Slug,
			FriendlyName:     m.FriendlyName,
			URL:              m.URL,
			GroupSlug:        m.Group,
			Source:           store.SourceStatic,
			DependsOn:        m.DependsOn,
			SlackChannelSlug: m.Slack,
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

	metrics := observability.New()

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
	groupSlugs := make([]string, 0, len(opts.Config.Groups))
	for _, g := range opts.Config.Groups {
		groupSlugs = append(groupSlugs, g.Slug)
	}
	srv.SetKnownGroups(groupSlugs)
	listener, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("bind listen address %q: %w", opts.ListenAddr, err)
	}
	httpServer := &http.Server{
		Handler:           srv.Routes(),
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
		materializer = merger.New(repo, opts.Config)
	}

	sched := scheduler.New(repo,
		scheduler.WithLogger(log),
		scheduler.WithEventSink(buildSink(notifier)),
		scheduler.WithSSLSink(buildSSLSink(notifier)),
		scheduler.WithMetrics(metrics),
	)
	staticPlans := buildPlans(opts.Config)
	planSource := &combinedPlanSource{
		static:       staticPlans,
		materializer: materializer,
	}

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
	// up without restarting the worker. For all-static deployments
	// the refresh is still cheap (no diff).
	refresh := 30 * time.Second
	if kc := opts.Config.Kube; kc != nil && kc.ResyncInterval.AsDuration() > 0 {
		// Match the watcher cadence so kube monitor lifecycle and
		// scheduling refresh stay in lockstep.
		refresh = kc.ResyncInterval.AsDuration()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.RunDynamic(ctx, planSource, refresh)
	}()

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

	// Kube auto-discovery: the materializer was built above so the
	// scheduler's plan source can read its CurrentPlans(). Here we
	// wire the actual watcher loop + the removal sink that runs
	// when an ingress disappears.
	if kc := opts.Config.Kube; kc != nil {
		removalSink := &kubeRemovalSink{repo: repo, notifier: notifier, log: log}
		kubeOpts := kube.Options{
			AnnotationDomain: kc.AnnotationDomain,
			ResyncInterval:   kc.ResyncInterval.AsDuration(),
			Materializer:     materializer,
			RemovalSink:      removalSink,
			Logger:           log,
		}
		var watcher *kube.Watcher
		if opts.KubeIngressLister != nil {
			watcher = kube.New(repo, opts.KubeIngressLister, kubeOpts)
		} else {
			watcher, err = kube.NewWithCluster(ctx, repo, kubeOpts, opts.KubeconfigPath)
			if err != nil {
				return fmt.Errorf("kube watcher: %w", err)
			}
		}
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

func buildPlans(cfg config.Config) []scheduler.Plan {
	groupNotify := map[string][]string{}
	for _, g := range cfg.Groups {
		if len(g.Notify) > 0 {
			groupNotify[g.Slug] = g.Notify
		}
	}
	out := make([]scheduler.Plan, 0, len(cfg.Monitors))
	for _, m := range cfg.Monitors {
		// Union: group-level entries are applied first so monitor-level
		// values get dedup priority. Order within the resulting markup
		// list follows the source lists; ResolveMentions handles dedup.
		merged := make([]string, 0, len(m.Notify)+len(groupNotify[m.Group]))
		merged = append(merged, groupNotify[m.Group]...)
		merged = append(merged, m.Notify...)
		isHTTPS := strings.HasPrefix(m.URL, "https://")
		out = append(out, scheduler.Plan{
			Slug:                   m.Slug,
			FriendlyName:           m.FriendlyName,
			URL:                    m.URL,
			HTTPMethod:             m.HTTPMethod,
			AcceptedStatusCodes:    m.AcceptedStatusCodes,
			Interval:               m.Interval.AsDuration(),
			Timeout:                m.Timeout.AsDuration(),
			Retries:                m.Retries,
			RetryBackoff:           m.RetryBackoff.AsDuration(),
			FollowRedirects:        m.FollowRedirects,
			TLSInsecureSkipVerify:  m.TLSInsecureSkipVerify,
			UserAgent:              cfg.HTTPClient.UserAgent,
			ReminderInterval:       m.ReminderInterval.AsDuration(),
			ChannelSlug:            m.Slack,
			Mentions:               slack.ResolveMentions(merged, cfg.Slack.UserMapping),
			DependsOn:              m.DependsOn,
			IsHTTPS:                isHTTPS,
			SSLAlertThreshold:      m.SSLAlertThreshold.AsDuration(),
			SSLEscalationThreshold: m.SSLEscalationThreshold.AsDuration(),
			SSLReminderInterval:    m.SSLReminderInterval.AsDuration(),
		})
	}
	return out
}

// combinedPlanSource concatenates the static-config plans (immutable
// for the run) with the materializer's current kube plans (changes
// every kube reconcile).
type combinedPlanSource struct {
	static       []scheduler.Plan
	materializer *merger.Materializer
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

// kubeRemovalSink dispatches the same soft-delete + Slack closeout +
// warning flow used for static removals, against monitors materialized
// from a now-disappeared Ingress.
type kubeRemovalSink struct {
	repo     *store.Repo
	notifier *slack.Notifier
	log      *slog.Logger
}

func (k *kubeRemovalSink) OnKubeMonitorRemoved(ctx context.Context, monitorSlug string) {
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
	if err := k.repo.SoftDeleteMonitor(ctx, monitorSlug, "kube ingress removed"); err != nil {
		k.log.Warn("kube removal: soft-delete", "slug", monitorSlug, "error", err)
		return
	}
	k.log.Info("kube monitor removed (soft-deleted)", "slug", monitorSlug, "was_status", row.Status)
	if row.SlackChannelSlug != "" {
		k.notifier.NotifyRemoved(ctx, row.SlackChannelSlug, view, "kube ingress removed", "k8s ingress")
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
		GroupSlug:    row.GroupSlug,
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
