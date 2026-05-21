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
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web"
)

// ServeOptions parameterizes serve startup. CLI parses these from
// flags + env; tests construct them directly.
type ServeOptions struct {
	Config     config.Config
	DBConfig   db.Config
	ListenAddr string         // e.g. ":8080"
	Logger     *slog.Logger   // nil → slog.Default()
	OnReady    func(addr net.Addr) // optional, called once listener is bound
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
		}
		if err := repo.ReconcileMonitor(ctx, spec); err != nil {
			return fmt.Errorf("reconcile %q: %w", m.Slug, err)
		}
	}

	srv := web.New(repo, log)
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

	sched := scheduler.New(repo, scheduler.WithLogger(log))
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
		})
	}
	return out
}
