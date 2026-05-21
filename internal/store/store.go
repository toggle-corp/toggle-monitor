// Package store is the repository layer over Postgres for monitors,
// alert events, Slack thread refs, and the auto-discovery snapshot.
// Issue 2 scope: static monitors and uptime alert events only.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
)

// ErrNotFound is returned when a lookup by slug yields no row.
var ErrNotFound = errors.New("monitor not found")

// Repo is the Postgres-backed repository. Methods accept primitive
// types where they cross the package boundary, except for alert.State
// and alert.Event which are the canonical state-machine types.
type Repo struct {
	pool *pgxpool.Pool
}

// New constructs a Repo over an already-open pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// MonitorSource records where a monitor was declared. Static config
// is the only source in Issue 2; kube discovery (Issue 9) adds 'kube'.
type MonitorSource string

const (
	SourceStatic MonitorSource = "static"
	SourceKube   MonitorSource = "kube"
)

// MonitorSpec is the config-side projection of a monitor — the fields
// the YAML (or future kube-discovery pipeline) owns. Runtime fields
// (status, last_*) live separately and are owned by the worker.
type MonitorSpec struct {
	Slug         string
	FriendlyName string
	URL          string
	GroupSlug    string
	Source       MonitorSource
	DependsOn    []string
}

// MonitorRow is the full row as the UI sees it. The embedded
// MonitorSpec carries the YAML-side fields including DependsOn.
type MonitorRow struct {
	MonitorSpec
	Status              alert.Status
	OpenedAt            *time.Time
	LastReminderAt      *time.Time
	LastCheckedAt       *time.Time
	LastStatusCode      *int
	LastError           *string
	Archived            bool
	ArchivedAt          *time.Time
	ArchiveReason       *string
	UptimeThreadChannel *string
	UptimeThreadTS      *string
}

// State returns the alert.State that the state machine would consume.
func (r MonitorRow) State() alert.State {
	s := alert.State{Status: r.Status}
	if r.OpenedAt != nil {
		s.OpenedAt = *r.OpenedAt
	}
	if r.LastReminderAt != nil {
		s.LastReminderAt = *r.LastReminderAt
	}
	return s
}

// ReconcileMonitor upserts the config-side fields of a monitor. Runtime
// state (status, opened_at, last_*) is preserved. Called once per
// monitor at serve startup after the YAML config is loaded.
func (r *Repo) ReconcileMonitor(ctx context.Context, spec MonitorSpec) error {
	src := spec.Source
	if src == "" {
		src = SourceStatic
	}
	deps := spec.DependsOn
	if deps == nil {
		deps = []string{}
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO monitors (slug, friendly_name, url, group_slug, source, depends_on)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (slug) DO UPDATE SET
			friendly_name = EXCLUDED.friendly_name,
			url           = EXCLUDED.url,
			group_slug    = EXCLUDED.group_slug,
			source        = EXCLUDED.source,
			depends_on    = EXCLUDED.depends_on,
			archived      = FALSE,
			archived_at   = NULL,
			archive_reason= NULL,
			updated_at    = now()
	`, spec.Slug, spec.FriendlyName, spec.URL, spec.GroupSlug, string(src), deps)
	if err != nil {
		return fmt.Errorf("reconcile monitor %q: %w", spec.Slug, err)
	}
	return nil
}

// GetMonitor returns one row by slug. Returns ErrNotFound if missing.
func (r *Repo) GetMonitor(ctx context.Context, slug string) (MonitorRow, error) {
	row := r.pool.QueryRow(ctx, selectMonitor+` WHERE slug = $1`, slug)
	return scanMonitor(row)
}

// ListActiveMonitors returns all non-archived monitors, ordered for the
// listing page (down → paused → ssl-expiring → up, then group, then
// friendly name).
func (r *Repo) ListActiveMonitors(ctx context.Context) ([]MonitorRow, error) {
	rows, err := r.pool.Query(ctx, selectMonitor+`
		WHERE archived = FALSE
		ORDER BY
			CASE status
				WHEN 'down' THEN 0
				WHEN 'temporary-paused' THEN 1
				WHEN 'kube-paused' THEN 2
				WHEN 'ssl-expiring' THEN 3
				WHEN 'up' THEN 4
				ELSE 5
			END,
			group_slug,
			friendly_name`)
	if err != nil {
		return nil, fmt.Errorf("list active monitors: %w", err)
	}
	defer rows.Close()
	var out []MonitorRow
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HomepageStats returns the count of monitors in each status. Used by
// the homepage stats tiles.
type HomepageStats struct {
	Up              int
	Down            int
	TemporaryPaused int
	SSLExpiring     int
	SSLSkipped      int
}

// CountOpenIncidents returns the number of currently-down monitors —
// used by the heartbeat body and ad-hoc queries.
func (r *Repo) CountOpenIncidents(ctx context.Context) (int, error) {
	row := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM monitors WHERE status = 'down' AND archived = FALSE`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count open incidents: %w", err)
	}
	return n, nil
}

func (r *Repo) HomepageStats(ctx context.Context) (HomepageStats, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'up'),
			COUNT(*) FILTER (WHERE status = 'down'),
			COUNT(*) FILTER (WHERE status = 'temporary-paused'),
			COUNT(*) FILTER (WHERE status = 'ssl-expiring'),
			COUNT(*) FILTER (WHERE status = 'ssl-skipped')
		FROM monitors WHERE archived = FALSE`)
	var s HomepageStats
	if err := row.Scan(&s.Up, &s.Down, &s.TemporaryPaused, &s.SSLExpiring, &s.SSLSkipped); err != nil {
		return HomepageStats{}, fmt.Errorf("homepage stats: %w", err)
	}
	return s, nil
}

// ApplyCheck records the result of one check tick. If event is non-nil
// (a state-changing transition or a reminder), the monitor row is
// updated AND an alert_event row is appended, atomically. If event is
// nil, only the last_* columns move. Either way, last_checked_at and
// the optional status code / error are updated.
//
// On a resolve event, the uptime thread refs are cleared as well so a
// subsequent open begins a fresh thread.
func (r *Repo) ApplyCheck(
	ctx context.Context,
	slug string,
	next alert.State,
	at time.Time,
	statusCode int,
	checkErr string,
	event *alert.Event,
) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var openedAt any
	if !next.OpenedAt.IsZero() {
		openedAt = next.OpenedAt
	}
	var lastReminderAt any
	if !next.LastReminderAt.IsZero() {
		lastReminderAt = next.LastReminderAt
	}
	var sc any
	if statusCode != 0 {
		sc = statusCode
	}
	var msg any
	if checkErr != "" {
		msg = checkErr
	}

	// On resolve, also clear the thread refs.
	clearThread := event != nil && event.Type == alert.EventResolve

	if clearThread {
		_, err = tx.Exec(ctx, `
			UPDATE monitors
			SET status = $1,
				opened_at = $2,
				last_reminder_at = $3,
				last_checked_at = $4,
				last_status_code = $5,
				last_error = $6,
				uptime_thread_channel = NULL,
				uptime_thread_ts = NULL,
				updated_at = now()
			WHERE slug = $7
		`, string(next.Status), openedAt, lastReminderAt, at, sc, msg, slug)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE monitors
			SET status = $1,
				opened_at = $2,
				last_reminder_at = $3,
				last_checked_at = $4,
				last_status_code = $5,
				last_error = $6,
				updated_at = now()
			WHERE slug = $7
		`, string(next.Status), openedAt, lastReminderAt, at, sc, msg, slug)
	}
	if err != nil {
		return fmt.Errorf("update monitor row: %w", err)
	}

	if event != nil {
		var downtime any
		if event.Type == alert.EventResolve {
			downtime = int64(event.Downtime.Seconds())
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO alert_events (monitor_slug, type, at, status_code, error, downtime_seconds)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, slug, string(event.Type), event.At, sc, msg, downtime)
		if err != nil {
			return fmt.Errorf("insert alert_event: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// MarkTemporaryPaused sets a monitor's status to 'temporary-paused'
// without touching last_* fields or appending an alert_event. Called
// by the scheduler when at least one dependsOn parent is currently
// down. Idempotent — repeat calls while paused are no-ops.
func (r *Repo) MarkTemporaryPaused(ctx context.Context, slug string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE monitors
		SET status = 'temporary-paused',
			updated_at = now()
		WHERE slug = $1 AND status <> 'temporary-paused'
	`, slug)
	if err != nil {
		return fmt.Errorf("mark temporary-paused %q: %w", slug, err)
	}
	return nil
}

// SetUptimeThread persists the Slack thread ref for the currently-open
// uptime incident on this monitor. Called by the notifier after
// successfully posting the parent down message.
func (r *Repo) SetUptimeThread(ctx context.Context, slug, channel, ts string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE monitors
		SET uptime_thread_channel = $1,
			uptime_thread_ts = $2,
			updated_at = now()
		WHERE slug = $3
	`, channel, ts, slug)
	if err != nil {
		return fmt.Errorf("set uptime thread for %q: %w", slug, err)
	}
	return nil
}

// AlertEventRow is the read-side projection of one alert_events row.
type AlertEventRow struct {
	ID              int64
	MonitorSlug     string
	Type            alert.EventType
	At              time.Time
	StatusCode      *int
	Error           *string
	DowntimeSeconds *int64
}

// ListAlertsForMonitor returns the alert history for a single monitor,
// newest first.
func (r *Repo) ListAlertsForMonitor(ctx context.Context, slug string, limit int) ([]AlertEventRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, monitor_slug, type, at, status_code, error, downtime_seconds
		FROM alert_events
		WHERE monitor_slug = $1
		ORDER BY at DESC, id DESC
		LIMIT $2
	`, slug, limit)
	if err != nil {
		return nil, fmt.Errorf("list alerts for %q: %w", slug, err)
	}
	defer rows.Close()
	var out []AlertEventRow
	for rows.Next() {
		var ev AlertEventRow
		var typ string
		if err := rows.Scan(&ev.ID, &ev.MonitorSlug, &typ, &ev.At, &ev.StatusCode, &ev.Error, &ev.DowntimeSeconds); err != nil {
			return nil, err
		}
		ev.Type = alert.EventType(typ)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListLatestAlerts returns recent alert events across all monitors,
// newest first. Powers the homepage latest-alerts list.
func (r *Repo) ListLatestAlerts(ctx context.Context, limit int) ([]AlertEventRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, monitor_slug, type, at, status_code, error, downtime_seconds
		FROM alert_events
		ORDER BY at DESC, id DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list latest alerts: %w", err)
	}
	defer rows.Close()
	var out []AlertEventRow
	for rows.Next() {
		var ev AlertEventRow
		var typ string
		if err := rows.Scan(&ev.ID, &ev.MonitorSlug, &typ, &ev.At, &ev.StatusCode, &ev.Error, &ev.DowntimeSeconds); err != nil {
			return nil, err
		}
		ev.Type = alert.EventType(typ)
		out = append(out, ev)
	}
	return out, rows.Err()
}

const selectMonitor = `
	SELECT slug, friendly_name, url, group_slug, source, depends_on,
	       status, opened_at, last_reminder_at, last_checked_at, last_status_code, last_error,
	       archived, archived_at, archive_reason,
	       uptime_thread_channel, uptime_thread_ts
	FROM monitors`

// rowScanner abstracts pgx.Row and pgx.Rows for scanMonitor.
type rowScanner interface {
	Scan(...any) error
}

func scanMonitor(row rowScanner) (MonitorRow, error) {
	var m MonitorRow
	var src, status string
	err := row.Scan(
		&m.Slug, &m.FriendlyName, &m.URL, &m.GroupSlug, &src, &m.DependsOn,
		&status, &m.OpenedAt, &m.LastReminderAt, &m.LastCheckedAt, &m.LastStatusCode, &m.LastError,
		&m.Archived, &m.ArchivedAt, &m.ArchiveReason,
		&m.UptimeThreadChannel, &m.UptimeThreadTS,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MonitorRow{}, ErrNotFound
		}
		return MonitorRow{}, err
	}
	m.Source = MonitorSource(src)
	m.Status = alert.Status(status)
	return m, nil
}
