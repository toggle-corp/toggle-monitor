// Package sentry owns the Sentry SDK lifecycle and the slog→Sentry
// bridge. When the config's sentry block is absent or disabled, every
// exported function is a no-op so callers can wire the package
// unconditionally.
package sentry

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	sentrygo "github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

// Config is the slim subset of config.Sentry the package needs. The
// caller resolves the env-var DSN before calling Init so this package
// has no dependency on internal/config.
type Config struct {
	Enabled          bool
	DSN              string
	Environment      string
	SampleRate       float64
	TracesSampleRate float64
	ServerName       string
}

// FlushFunc is the cleanup closure returned from Init. The lifecycle
// shutdown path calls it under a deadline; tests call it to drain the
// fake transport.
type FlushFunc func()

var noopFlush FlushFunc = func() {}

// Init bootstraps the global Sentry hub. Returns a flush closure for
// the shutdown path. When cfg.Enabled is false it returns a no-op
// closure and leaves the global hub at its zero (no-op) state.
//
// release is stamped onto every event; the lifecycle wires it from
// the binary's build-time version variable.
func Init(cfg Config, release string) (FlushFunc, error) {
	if !cfg.Enabled {
		return noopFlush, nil
	}
	if cfg.DSN == "" {
		return noopFlush, errors.New("sentry enabled but DSN is empty")
	}
	opts := sentrygo.ClientOptions{
		Dsn:              cfg.DSN,
		Environment:      cfg.Environment,
		Release:          release,
		SampleRate:       cfg.SampleRate,
		TracesSampleRate: cfg.TracesSampleRate,
	}
	if cfg.ServerName != "" {
		opts.ServerName = cfg.ServerName
	}
	if err := sentrygo.Init(opts); err != nil {
		return noopFlush, fmt.Errorf("sentry init: %w", err)
	}
	return func() {
		sentrygo.Flush(2 * time.Second)
	}, nil
}

// Handler returns the slog→Sentry forwarder. Always callable; when
// the SDK was not initialized the handler is still safe (events go to
// the no-op global hub).
//
// Caller threads this into the slog handler chain alongside the
// existing JSON stdout handler.
func Handler() slog.Handler {
	return &slogHandler{}
}

// HTTPMiddleware wraps next with sentry-go's panic-capturing
// middleware. Panics are recorded with full request context (URL,
// method, headers) and then repanicked so net/http's default 500
// response still fires.
func HTTPMiddleware(next http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(next)
}

// RecoverPanic is the deferred helper for background goroutines. On
// recover it captures the panic with a stacktrace, logs an ERROR
// record (which the slog bridge will also forward — accepted as a
// duplicate; Sentry fingerprinting merges them), and flushes
// best-effort in case the goroutine is about to exit.
//
// where is a short label naming the call site (e.g. "scheduler.tick",
// "kube.reconcile"). It's stamped into both the Sentry message and
// the slog record so operators can correlate.
func RecoverPanic(log *slog.Logger, where string) {
	r := recover()
	if r == nil {
		return
	}
	err := fmt.Errorf("panic in %s: %v", where, r)
	hub := sentrygo.CurrentHub().Clone()
	hub.Recover(err)
	if log != nil {
		log.Error("recovered panic", "where", where, "error", err)
	}
	sentrygo.Flush(2 * time.Second)
}
