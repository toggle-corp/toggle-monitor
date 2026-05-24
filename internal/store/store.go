// Package store is the repository layer over Postgres for monitors,
// alert events, Slack thread refs, and the auto-discovery snapshot.
// Issue 2 scope: static monitors and uptime alert events only.
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	Slug             string
	FriendlyName     string
	URL              string
	GroupSlug        string
	Source           MonitorSource
	DependsOn        []string
	SlackChannelSlug string // remembered so removal can still address Slack
	Tags             []string
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

	SSLStatus         *alert.SSLStatus
	SSLExpiresAt      *time.Time
	SSLIssuer         *string
	SSLSubject        *string
	SSLOpenedAt       *time.Time
	SSLLastReminderAt *time.Time
	SSLThreadChannel  *string
	SSLThreadTS       *string
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

// SSL returns the SSL-side state for the state machine. Zero state
// (Status="") is fine — ApplySSL treats it like SSLStatusOK.
func (r MonitorRow) SSL() alert.SSLState {
	s := alert.SSLState{}
	if r.SSLStatus != nil {
		s.Status = *r.SSLStatus
	}
	if r.SSLOpenedAt != nil {
		s.OpenedAt = *r.SSLOpenedAt
	}
	if r.SSLLastReminderAt != nil {
		s.LastReminderAt = *r.SSLLastReminderAt
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
	tags := spec.Tags
	if tags == nil {
		tags = []string{}
	}
	var slackSlugArg any
	if spec.SlackChannelSlug != "" {
		slackSlugArg = spec.SlackChannelSlug
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO monitors (slug, friendly_name, url, group_slug, source, depends_on, slack_channel_slug, tags)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (slug) DO UPDATE SET
			friendly_name      = EXCLUDED.friendly_name,
			url                = EXCLUDED.url,
			group_slug         = EXCLUDED.group_slug,
			source             = EXCLUDED.source,
			depends_on         = EXCLUDED.depends_on,
			slack_channel_slug = EXCLUDED.slack_channel_slug,
			tags               = EXCLUDED.tags,
			archived           = FALSE,
			archived_at        = NULL,
			archive_reason     = NULL,
			updated_at         = now()
	`, spec.Slug, spec.FriendlyName, spec.URL, spec.GroupSlug, string(src), deps, slackSlugArg, tags)
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
	listing, err := r.ListMonitors(ctx, ListMonitorsOpts{Limit: 0})
	if err != nil {
		return nil, err
	}
	return listing.Items, nil
}

// ListMonitorsOpts controls filtering + pagination of the listing.
// All fields are optional: zero values mean "no filter", and Limit==0
// means "no limit" (used by integrations that page in memory).
type ListMonitorsOpts struct {
	Search          string // substring match on friendly_name OR slug
	Status          string // "" → no status filter
	SSL             string // "" → no ssl_status filter ('ok' | 'ssl-expiring' | 'ssl-skipped')
	GroupSlug       string // "" → no group filter
	IncludeArchived bool
	// Sort is a logical column key (one of ListSortKeys); "" → default
	// "status priority then group then name" order. Unknown keys fall
	// back to the default to keep the API forgiving.
	Sort     string
	SortDesc bool
	Offset   int
	Limit    int
}

// ListSortKeys is the whitelist of values acceptable for
// ListMonitorsOpts.Sort. Exported so the web layer can validate the
// query param against the same set. Order here is the order shown in
// the UI's column-picker dropdown if one is ever added.
var ListSortKeys = []string{"name", "group", "status", "url", "code", "checked", "ssl_expires"}

// sortClause maps a Sort key to a SQL ORDER BY fragment. NULL handling
// stays consistent: missing values sort last regardless of direction
// so empty rows don't dominate either end of the listing.
func sortClause(key string, desc bool) string {
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	// Default tiebreaker so equal sort-key rows have a stable order.
	tiebreak := ", group_slug, friendly_name"
	switch key {
	case "name":
		return "friendly_name " + dir + ", group_slug"
	case "group":
		return "group_slug " + dir + ", friendly_name"
	case "url":
		return "url " + dir + tiebreak
	case "code":
		return "last_status_code " + dir + " NULLS LAST" + tiebreak
	case "checked":
		return "last_checked_at " + dir + " NULLS LAST" + tiebreak
	case "ssl_expires":
		return "ssl_expires_at " + dir + " NULLS LAST" + tiebreak
	case "status":
		// Severity priority — "down first" makes more sense than
		// alphabetical even when the user clicks 'sort by status'.
		statusCase := `
			CASE status
				WHEN 'down' THEN 0
				WHEN 'temporary-paused' THEN 1
				WHEN 'kube-paused' THEN 2
				WHEN 'up' THEN 4
				ELSE 5
			END ` + dir
		return statusCase + tiebreak
	default:
		// Default order: down > paused > kube-paused > up, then
		// group, then name. Matches the original behavior.
		return `
			CASE status
				WHEN 'down' THEN 0
				WHEN 'temporary-paused' THEN 1
				WHEN 'kube-paused' THEN 2
				WHEN 'up' THEN 4
				ELSE 5
			END,
			group_slug,
			friendly_name`
	}
}

// MonitorListing is the paginated result of ListMonitors.
type MonitorListing struct {
	Items []MonitorRow
	Total int
}

// ListMonitors returns a filtered + paginated slice of monitors,
// along with the total matching the filter (for pagination UI).
// Sort order matches ListActiveMonitors.
func (r *Repo) ListMonitors(ctx context.Context, opts ListMonitorsOpts) (MonitorListing, error) {
	conds := []string{}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, "$"+itoa(len(args))))
	}
	if !opts.IncludeArchived {
		conds = append(conds, "archived = FALSE")
	}
	if opts.Status != "" {
		add("status = %s", opts.Status)
	}
	if opts.SSL != "" {
		add("ssl_status = %s", opts.SSL)
	}
	if opts.GroupSlug != "" {
		add("group_slug = %s", opts.GroupSlug)
	}
	if opts.Search != "" {
		args = append(args, "%"+opts.Search+"%")
		conds = append(conds, fmt.Sprintf("(friendly_name ILIKE $%d OR slug ILIKE $%d)", len(args), len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	order := " ORDER BY " + sortClause(opts.Sort, opts.SortDesc)

	// total
	var total int
	countQ := "SELECT COUNT(*) FROM monitors" + where
	if err := r.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return MonitorListing{}, fmt.Errorf("count monitors: %w", err)
	}

	// items
	q := selectMonitor + where + order
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
		args = append(args, opts.Offset)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
	}
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return MonitorListing{}, fmt.Errorf("list monitors: %w", err)
	}
	defer rows.Close()
	out := MonitorListing{Total: total}
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return MonitorListing{}, err
		}
		out.Items = append(out.Items, m)
	}
	return out, rows.Err()
}

func itoa(n int) string {
	// small-allocation friendly integer-to-decimal
	return strconv.Itoa(n)
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

// GroupStats is the same shape as HomepageStats but scoped to one
// group slug. Powers both the /groups index (one row per group) and
// the per-group page header tiles.
type GroupStats struct {
	GroupSlug       string
	Total           int
	Up              int
	Down            int
	TemporaryPaused int
	SSLExpiring     int
	SSLSkipped      int
}

// DiscoverySnapshotRow is one row from the discovery_snapshot table.
//
// The `Annotations` field that used to surface the raw Ingress
// annotation map is gone per ADR-0002 — annotations no longer drive
// any monitor behaviour, so persisting them was just bloat. The
// `PresetSlug` field was dropped alongside it (presets were deleted
// per ADR-0002; migration 0009 removes the column).
type DiscoverySnapshotRow struct {
	ID          int64
	Namespace   string
	IngressName string
	Host        string
	Status      string // 'added' | 'kube-paused' | 'kube-invalid' | 'kube-ignored'
	Reason      *string
	MonitorSlug *string
	LastSeenAt  time.Time
}

// UpsertDiscoverySnapshot writes (or refreshes) one snapshot row.
// Called per-ingress by the reconcile pass.
func (r *Repo) UpsertDiscoverySnapshot(ctx context.Context, row DiscoverySnapshotRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO discovery_snapshot
			(namespace, ingress_name, host, status, reason, monitor_slug, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (namespace, ingress_name, host) DO UPDATE SET
			status        = EXCLUDED.status,
			reason        = EXCLUDED.reason,
			monitor_slug  = EXCLUDED.monitor_slug,
			last_seen_at  = now()
	`, row.Namespace, row.IngressName, row.Host, row.Status, row.Reason, row.MonitorSlug)
	if err != nil {
		return fmt.Errorf("upsert snapshot %s/%s/%s: %w", row.Namespace, row.IngressName, row.Host, err)
	}
	return nil
}

// PruneDiscoverySnapshot removes snapshot rows whose last_seen_at is
// older than the supplied threshold. Returns the count of pruned rows
// plus the monitor slugs those rows pointed at (if any) — the kube
// removal flow walks those to soft-delete the materialized monitor
// and dispatch a closeout + warning.
func (r *Repo) PruneDiscoverySnapshot(ctx context.Context, before time.Time) (int64, []string, error) {
	rows, err := r.pool.Query(ctx,
		`DELETE FROM discovery_snapshot WHERE last_seen_at < $1 RETURNING monitor_slug`,
		before)
	if err != nil {
		return 0, nil, fmt.Errorf("prune discovery_snapshot: %w", err)
	}
	defer rows.Close()
	var pruned int64
	var prunedMonitors []string
	for rows.Next() {
		var slug *string
		if err := rows.Scan(&slug); err != nil {
			return 0, nil, err
		}
		pruned++
		if slug != nil && *slug != "" {
			prunedMonitors = append(prunedMonitors, *slug)
		}
	}
	return pruned, prunedMonitors, rows.Err()
}

// CountKubeInvalid returns the number of discovery_snapshot rows
// currently at status='kube-invalid'. Used by the operator nav to
// surface a count badge on the Issues tab; intentionally a thin
// COUNT(*) query rather than reusing ListDiscoverySnapshot so the
// per-page-render cost stays predictable.
func (r *Repo) CountKubeInvalid(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM discovery_snapshot WHERE status = 'kube-invalid'`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count kube-invalid: %w", err)
	}
	return n, nil
}

// ListDiscoverySnapshot returns every snapshot row, ordered by
// namespace then ingress name then host. Used by the auto-discovery
// UI (Issue 12).
func (r *Repo) ListDiscoverySnapshot(ctx context.Context) ([]DiscoverySnapshotRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, namespace, ingress_name, host, status, reason, monitor_slug, last_seen_at
		FROM discovery_snapshot
		ORDER BY namespace, ingress_name, host
	`)
	if err != nil {
		return nil, fmt.Errorf("list discovery_snapshot: %w", err)
	}
	defer rows.Close()
	var out []DiscoverySnapshotRow
	for rows.Next() {
		var row DiscoverySnapshotRow
		if err := rows.Scan(
			&row.ID, &row.Namespace, &row.IngressName, &row.Host,
			&row.Status, &row.Reason, &row.MonitorSlug,
			&row.LastSeenAt,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
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
			COUNT(*) FILTER (WHERE ssl_status = 'ssl-expiring'),
			COUNT(*) FILTER (WHERE ssl_status = 'ssl-skipped')
		FROM monitors WHERE archived = FALSE`)
	var s HomepageStats
	if err := row.Scan(&s.Up, &s.Down, &s.TemporaryPaused, &s.SSLExpiring, &s.SSLSkipped); err != nil {
		return HomepageStats{}, fmt.Errorf("homepage stats: %w", err)
	}
	return s, nil
}

// ListGroupStats returns per-group status counts for the /groups
// index, sorted by group slug. Archived monitors are excluded so the
// index reflects what's currently being checked.
func (r *Repo) ListGroupStats(ctx context.Context) ([]GroupStats, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			group_slug,
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status = 'up'),
			COUNT(*) FILTER (WHERE status = 'down'),
			COUNT(*) FILTER (WHERE status = 'temporary-paused'),
			COUNT(*) FILTER (WHERE ssl_status = 'ssl-expiring'),
			COUNT(*) FILTER (WHERE ssl_status = 'ssl-skipped')
		FROM monitors
		WHERE archived = FALSE
		GROUP BY group_slug
		ORDER BY group_slug`)
	if err != nil {
		return nil, fmt.Errorf("list group stats: %w", err)
	}
	defer rows.Close()
	var out []GroupStats
	for rows.Next() {
		var g GroupStats
		if err := rows.Scan(&g.GroupSlug, &g.Total, &g.Up, &g.Down, &g.TemporaryPaused, &g.SSLExpiring, &g.SSLSkipped); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupStatsForSlug returns the same stats shape scoped to one group.
// Used by the per-group page header. Returns (_, false) when no
// monitors live in that group — the caller can render an empty-state.
func (r *Repo) GroupStatsForSlug(ctx context.Context, slug string) (GroupStats, bool, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE status = 'up'),
			COUNT(*) FILTER (WHERE status = 'down'),
			COUNT(*) FILTER (WHERE status = 'temporary-paused'),
			COUNT(*) FILTER (WHERE ssl_status = 'ssl-expiring'),
			COUNT(*) FILTER (WHERE ssl_status = 'ssl-skipped')
		FROM monitors
		WHERE archived = FALSE AND group_slug = $1`, slug)
	var g GroupStats
	if err := row.Scan(&g.Total, &g.Up, &g.Down, &g.TemporaryPaused, &g.SSLExpiring, &g.SSLSkipped); err != nil {
		return GroupStats{}, false, fmt.Errorf("group stats %q: %w", slug, err)
	}
	if g.Total == 0 {
		return GroupStats{}, false, nil
	}
	g.GroupSlug = slug
	return g, true, nil
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

// ListActiveBySource returns every non-archived monitor with the
// given source. Used by the startup reconcile pass to diff
// YAML-declared static monitors against the DB.
func (r *Repo) ListActiveBySource(ctx context.Context, src MonitorSource) ([]MonitorRow, error) {
	rows, err := r.pool.Query(ctx, selectMonitor+` WHERE archived = FALSE AND source = $1`, string(src))
	if err != nil {
		return nil, fmt.Errorf("list active by source: %w", err)
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

// SoftDeleteMonitor flips a monitor to archived with the given reason
// timestamped now(). History is preserved; slug reuse on a future
// reconcile resurrects the row (ReconcileMonitor clears archived).
func (r *Repo) SoftDeleteMonitor(ctx context.Context, slug, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE monitors
		SET archived       = TRUE,
		    archived_at    = now(),
		    archive_reason = $1,
		    updated_at     = now()
		WHERE slug = $2
	`, reason, slug)
	if err != nil {
		return fmt.Errorf("soft-delete %q: %w", slug, err)
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

// ListChildrenOf returns the slugs of every active monitor whose
// depends_on list contains `parentSlug` — i.e. the monitors that would
// be marked temporary-paused if the parent went down. Used by the
// Slack notifier to surface a "this alert pauses X" note on the
// parent message. Sorted by slug for stable output. Archived monitors
// are excluded.
func (r *Repo) ListChildrenOf(ctx context.Context, parentSlug string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT slug
		FROM monitors
		WHERE $1 = ANY(depends_on) AND archived = FALSE
		ORDER BY slug
	`, parentSlug)
	if err != nil {
		return nil, fmt.Errorf("list children of %q: %w", parentSlug, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan child slug: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ApplySSLCheck mirrors ApplyCheck for the SSL state machine. Updates
// the ssl_* columns and, when event != nil, appends an alert_event of
// the matching SSL type (ssl_open / ssl_reminder / ssl_resolve). On
// resolve, the SSL thread refs are cleared.
func (r *Repo) ApplySSLCheck(
	ctx context.Context,
	monitorSlug string,
	next alert.SSLState,
	expiresAt time.Time,
	issuer, subject string,
	event *alert.SSLEvent,
) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		statusArg    any
		expiresArg   any
		issuerArg    any
		subjectArg   any
		openedAtArg  any
		lastReminder any
	)
	if next.Status != "" {
		statusArg = string(next.Status)
	}
	if !expiresAt.IsZero() {
		expiresArg = expiresAt
	}
	if issuer != "" {
		issuerArg = issuer
	}
	if subject != "" {
		subjectArg = subject
	}
	if !next.OpenedAt.IsZero() {
		openedAtArg = next.OpenedAt
	}
	if !next.LastReminderAt.IsZero() {
		lastReminder = next.LastReminderAt
	}

	clearThread := event != nil && event.Type == alert.EventSSLResolve
	if clearThread {
		_, err = tx.Exec(ctx, `
			UPDATE monitors
			SET ssl_status            = $1,
				ssl_expires_at        = $2,
				ssl_issuer            = COALESCE($3, ssl_issuer),
				ssl_subject           = COALESCE($4, ssl_subject),
				ssl_opened_at         = $5,
				ssl_last_reminder_at  = $6,
				ssl_thread_channel    = NULL,
				ssl_thread_ts         = NULL,
				updated_at = now()
			WHERE slug = $7
		`, statusArg, expiresArg, issuerArg, subjectArg, openedAtArg, lastReminder, monitorSlug)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE monitors
			SET ssl_status            = $1,
				ssl_expires_at        = $2,
				ssl_issuer            = COALESCE($3, ssl_issuer),
				ssl_subject           = COALESCE($4, ssl_subject),
				ssl_opened_at         = $5,
				ssl_last_reminder_at  = $6,
				updated_at = now()
			WHERE slug = $7
		`, statusArg, expiresArg, issuerArg, subjectArg, openedAtArg, lastReminder, monitorSlug)
	}
	if err != nil {
		return fmt.Errorf("update monitor row (ssl): %w", err)
	}

	if event != nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO alert_events (monitor_slug, type, at, status_code, error, downtime_seconds)
			VALUES ($1, $2, $3, NULL, NULL, NULL)
		`, monitorSlug, string(event.Type), event.At)
		if err != nil {
			return fmt.Errorf("insert ssl alert_event: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit (ssl): %w", err)
	}
	return nil
}

// SetSSLThread persists the Slack thread ref for the currently-open
// SSL incident on this monitor. Called by the notifier after
// successfully posting the SSL parent message.
func (r *Repo) SetSSLThread(ctx context.Context, slug, channel, ts string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE monitors
		SET ssl_thread_channel = $1,
			ssl_thread_ts      = $2,
			updated_at = now()
		WHERE slug = $3
	`, channel, ts, slug)
	if err != nil {
		return fmt.Errorf("set ssl thread for %q: %w", slug, err)
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
// FriendlyName is only populated by queries that join monitors (the
// homepage latest-alerts feed); per-monitor history leaves it empty
// because the caller already knows which monitor it's on.
type AlertEventRow struct {
	ID              int64
	MonitorSlug     string
	FriendlyName    string
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

// LatestAlertsListing wraps the paginated latest-alerts query.
type LatestAlertsListing struct {
	Items []AlertEventRow
	Total int
}

// ListLatestAlerts returns recent alert events across all monitors,
// newest first. Powers the homepage latest-alerts list.
func (r *Repo) ListLatestAlerts(ctx context.Context, limit, offset int) (LatestAlertsListing, error) {
	var total int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alert_events`).Scan(&total); err != nil {
		return LatestAlertsListing{}, fmt.Errorf("count latest alerts: %w", err)
	}
	rows, err := r.pool.Query(ctx, `
		SELECT ae.id, ae.monitor_slug,
		       COALESCE(m.friendly_name, ae.monitor_slug) AS friendly_name,
		       ae.type, ae.at, ae.status_code, ae.error, ae.downtime_seconds
		FROM alert_events ae
		LEFT JOIN monitors m ON m.slug = ae.monitor_slug
		ORDER BY ae.at DESC, ae.id DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return LatestAlertsListing{}, fmt.Errorf("list latest alerts: %w", err)
	}
	defer rows.Close()
	out := LatestAlertsListing{Total: total}
	for rows.Next() {
		var ev AlertEventRow
		var typ string
		if err := rows.Scan(&ev.ID, &ev.MonitorSlug, &ev.FriendlyName, &typ, &ev.At, &ev.StatusCode, &ev.Error, &ev.DowntimeSeconds); err != nil {
			return LatestAlertsListing{}, err
		}
		ev.Type = alert.EventType(typ)
		out.Items = append(out.Items, ev)
	}
	return out, rows.Err()
}

const selectMonitor = `
	SELECT slug, friendly_name, url, group_slug, source, depends_on, slack_channel_slug, tags,
	       status, opened_at, last_reminder_at, last_checked_at, last_status_code, last_error,
	       archived, archived_at, archive_reason,
	       uptime_thread_channel, uptime_thread_ts,
	       ssl_status, ssl_expires_at, ssl_issuer, ssl_subject,
	       ssl_opened_at, ssl_last_reminder_at,
	       ssl_thread_channel, ssl_thread_ts
	FROM monitors`

// rowScanner abstracts pgx.Row and pgx.Rows for scanMonitor.
type rowScanner interface {
	Scan(...any) error
}

func scanMonitor(row rowScanner) (MonitorRow, error) {
	var m MonitorRow
	var src, status string
	var sslStatus, slackSlug *string
	err := row.Scan(
		&m.Slug, &m.FriendlyName, &m.URL, &m.GroupSlug, &src, &m.DependsOn, &slackSlug, &m.Tags,
		&status, &m.OpenedAt, &m.LastReminderAt, &m.LastCheckedAt, &m.LastStatusCode, &m.LastError,
		&m.Archived, &m.ArchivedAt, &m.ArchiveReason,
		&m.UptimeThreadChannel, &m.UptimeThreadTS,
		&sslStatus, &m.SSLExpiresAt, &m.SSLIssuer, &m.SSLSubject,
		&m.SSLOpenedAt, &m.SSLLastReminderAt,
		&m.SSLThreadChannel, &m.SSLThreadTS,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MonitorRow{}, ErrNotFound
		}
		return MonitorRow{}, err
	}
	m.Source = MonitorSource(src)
	m.Status = alert.Status(status)
	if sslStatus != nil {
		ss := alert.SSLStatus(*sslStatus)
		m.SSLStatus = &ss
	}
	if slackSlug != nil {
		m.SlackChannelSlug = *slackSlug
	}
	return m, nil
}
