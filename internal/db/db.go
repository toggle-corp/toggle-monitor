// Package db owns the Postgres connection lifecycle: startup backoff,
// runtime retry, and the schema-version check that gates serve startup.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is the DSN-shaped subset of the YAML database block. Password
// is resolved separately (from passwordEnv) and supplied here at
// connect time.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
}

// DSN renders the libpq DSN. Used by both pgxpool and golang-migrate
// (which accepts the postgres:// URL form).
func (c Config) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.User, c.Password, c.Host, c.Port, c.Name, c.SSLMode)
}

// MaxStartupWait is the upper bound on retry attempts during initial
// startup. Mirrors the design: ~60s of exponential backoff, then exit
// (k8s restartPolicy keeps retrying).
const MaxStartupWait = 60 * time.Second

// ConnectWithBackoff opens a pgxpool with exponential backoff. Returns
// the pool on success, or the last error after MaxStartupWait.
func ConnectWithBackoff(ctx context.Context, cfg Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(MaxStartupWait)
	delay := 500 * time.Millisecond
	var lastErr error
	for attempt := 1; ; attempt++ {
		pool, err := pgxpool.New(ctx, cfg.DSN())
		if err == nil {
			pingErr := pool.Ping(ctx)
			if pingErr == nil {
				return pool, nil
			}
			pool.Close()
			err = pingErr
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database not reachable after %s: %w", MaxStartupWait, lastErr)
		}
		if logger != nil {
			logger.Warn("database connect failed, retrying",
				"attempt", attempt, "delay", delay, "error", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		// Exponential, capped at 5s.
		if delay < 5*time.Second {
			delay *= 2
		}
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}
