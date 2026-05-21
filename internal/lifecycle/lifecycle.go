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
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
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
	defer pool.Close()

	if err := migrate.Check(opts.DBConfig.DSN()); err != nil {
		return fmt.Errorf("schema version check: %w (run `toggle-monitor migrate`)", err)
	}

	repo := store.New(pool)

	for _, m := range opts.Config.Monitors {
		spec := store.MonitorSpec{
			Slug:         m.Slug,
			FriendlyName: m.FriendlyName,
			URL:          m.URL,
			GroupSlug:    m.Group,
			Source:       store.SourceStatic,
			DependsOn:    m.DependsOn,
		}
		if err := repo.ReconcileMonitor(ctx, spec); err != nil {
			return fmt.Errorf("reconcile %q: %w", m.Slug, err)
		}
	}

	// Resolve every Slack channel's bot token from the env. Missing
	// vars are a hard error so the operator notices before a real
	// alert needs to fire.
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
		BodyMaxChars: opts.Config.Slack.BodyMaxChars,
		PublicBase:   opts.Config.PublicBaseURL,
		Logger:       log,
	})

	metrics := observability.New()

	srv := web.New(repo, log)
	srv.SetMetricsHandler(metrics.Handler())
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

	sched := scheduler.New(repo,
		scheduler.WithLogger(log),
		scheduler.WithEventSink(buildSink(notifier)),
		scheduler.WithMetrics(metrics),
	)
	plans := buildPlans(opts.Config)

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

	// Scheduler.
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.Run(ctx, plans)
	}()

	// Hourly workspace re-check.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wsWatcher.Run(ctx, time.Hour)
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	// Issue-2 shutdown: stop accepting HTTP, then wait for goroutines.
	// Full graceful-shutdown ordering (cancel watcher, flush DB, final
	// heartbeat) lands in Issue 16.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "error", err)
	}
	wg.Wait()
	return nil
}

func buildPlans(cfg config.Config) []scheduler.Plan {
	out := make([]scheduler.Plan, 0, len(cfg.Monitors))
	for _, m := range cfg.Monitors {
		out = append(out, scheduler.Plan{
			Slug:                m.Slug,
			FriendlyName:        m.FriendlyName,
			URL:                 m.URL,
			HTTPMethod:          m.HTTPMethod,
			AcceptedStatusCodes: m.AcceptedStatusCodes,
			Interval:            m.Interval.AsDuration(),
			Timeout:             m.Timeout.AsDuration(),
			Retries:             m.Retries,
			RetryBackoff:        m.RetryBackoff.AsDuration(),
			FollowRedirects:     m.FollowRedirects,
			UserAgent:           cfg.HTTPClient.UserAgent,
			ReminderInterval:    m.ReminderInterval.AsDuration(),
			ChannelSlug:         m.Slack,
			Mentions:            m.Notify,
			DependsOn:           m.DependsOn,
		})
	}
	return out
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

// buildSink turns the slack.Notifier into the scheduler.EventSink
// shape (which deliberately doesn't import slack).
func buildSink(n *slack.Notifier) scheduler.EventSink {
	return func(ctx context.Context, row store.MonitorRow, channelSlug string, mentions []string, event *alert.Event) error {
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
