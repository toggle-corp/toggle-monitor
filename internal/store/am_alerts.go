package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AMIncident is the read-side projection of one row in am_alerts. It
// is the in-memory shape every AM-pipeline component speaks: the
// handler reads it after insert to decide whether to post Slack, the
// /alerts pages render from it, the sweeper deletes by id. JSONB
// columns are surfaced as native Go types (maps / slices) so callers
// never touch raw bytes.
type AMIncident struct {
	ID             int64
	Fingerprint    string
	Alertname      string
	Labels         map[string]string
	Annotations    map[string]string
	StartedAt      time.Time
	EndedAt        sql.NullTime
	ChannelSlug    string
	SlackChannel   string
	SlackTS        string
	RuleChain      string
	ResolvedNotify []string
	ExternalURL    string
	Receiver       string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AMIncidentInsert is the write-side projection. Only the fields the
// handler knows at the "first webhook for this fingerprint" boundary —
// no slack_channel / slack_ts (set later via UpdateAMSlackRef), no
// ended_at (set by MarkAMResolved), no timestamps (defaulted).
type AMIncidentInsert struct {
	Fingerprint    string
	Alertname      string
	Labels         map[string]string
	Annotations    map[string]string
	StartedAt      time.Time
	ChannelSlug    string
	RuleChain      string
	ResolvedNotify []string
	ExternalURL    string
	Receiver       string
}

// AMEventType enumerates the kinds of webhook deliveries we record
// against an incident. The vocabulary doubles as the value space of
// the am_alert_events.event_type column.
type AMEventType string

const (
	AMEventFiring       AMEventType = "firing"
	AMEventResolved     AMEventType = "resolved"
	AMEventRepeatFiring AMEventType = "repeat-firing"
	AMEventLateResolve  AMEventType = "late-resolve"
)

// AMListFilter is the predicate set for the /alerts listing. All
// fields are optional; the zero value lists every incident newest-first.
// Limit defaults to 50, capped at 500 — the cap is server-side so a
// stray ?limit= query string can't tip the page render into seconds.
type AMListFilter struct {
	Status      string // "firing" | "resolved" | "" (all)
	Severity    string // matched against labels->>'severity'
	Alertname   string // exact
	ChannelSlug string
	Receiver    string
	From        *time.Time
	To          *time.Time
	Limit       int // default 50, cap 500
	Offset      int
}

const (
	amListDefaultLimit = 50
	amListMaxLimit     = 500
)

// selectAMIncident is the canonical column list for am_alerts. Kept as a
// constant so every "SELECT * FROM am_alerts" call site stays in lockstep
// with scanAMIncident's positional reads. Adding a column is a two-spot
// edit (here + the scan) and the compiler catches the mismatch.
const selectAMIncident = `
	SELECT id, fingerprint, alertname, labels, annotations,
	       started_at, ended_at, channel_slug, slack_channel, slack_ts,
	       rule_chain, resolved_notify, external_url, receiver,
	       created_at, updated_at
	FROM am_alerts`

// InsertOpenAMIncident persists a fresh open incident and reports
// whether the call actually inserted (true) or hit an existing open
// row for the same fingerprint (false). Either way it returns the live
// row so the handler can read slack_ts to decide whether the Slack
// post still needs to be made — the partial unique index plus the
// "fetch on conflict" path is the heart of ADR-0005's
// "DB-INSERT before Slack-post" idempotency story.
func (r *Repo) InsertOpenAMIncident(ctx context.Context, in AMIncidentInsert) (*AMIncident, bool, error) {
	labelsJSON, err := json.Marshal(stringMapOrEmpty(in.Labels))
	if err != nil {
		return nil, false, fmt.Errorf("marshal labels: %w", err)
	}
	annosJSON, err := json.Marshal(stringMapOrEmpty(in.Annotations))
	if err != nil {
		return nil, false, fmt.Errorf("marshal annotations: %w", err)
	}
	notifyJSON, err := json.Marshal(stringSliceOrEmpty(in.ResolvedNotify))
	if err != nil {
		return nil, false, fmt.Errorf("marshal resolved_notify: %w", err)
	}

	// First branch: try to insert. The partial unique index on
	// fingerprint WHERE ended_at IS NULL turns a concurrent
	// double-delivery into a no-op (no rows in RETURNING).
	row := r.pool.QueryRow(ctx, `
		INSERT INTO am_alerts
			(fingerprint, alertname, labels, annotations, started_at,
			 channel_slug, rule_chain, resolved_notify, external_url, receiver)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (fingerprint) WHERE ended_at IS NULL DO NOTHING
		RETURNING id, fingerprint, alertname, labels, annotations,
		          started_at, ended_at, channel_slug, slack_channel, slack_ts,
		          rule_chain, resolved_notify, external_url, receiver,
		          created_at, updated_at
	`, in.Fingerprint, in.Alertname, labelsJSON, annosJSON, in.StartedAt,
		in.ChannelSlug, in.RuleChain, notifyJSON, in.ExternalURL, in.Receiver)
	inc, err := scanAMIncident(row)
	if err == nil {
		return inc, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("insert am_alerts: %w", err)
	}
	// Conflict path: read back the existing live row by fingerprint.
	existing, err := r.findOpenAMIncidentByFingerprint(ctx, in.Fingerprint)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		// Race: the conflicting row was resolved between INSERT and
		// SELECT. Treat as "no open row" and let the caller retry.
		return nil, false, fmt.Errorf("am_alerts: conflict for %q but no open row found (resolve race)", in.Fingerprint)
	}
	return existing, false, nil
}

// findOpenAMIncidentByFingerprint returns the open row for a fingerprint
// (the one with ended_at IS NULL), or nil if none. Used by the conflict
// branch of InsertOpenAMIncident.
func (r *Repo) findOpenAMIncidentByFingerprint(ctx context.Context, fingerprint string) (*AMIncident, error) {
	row := r.pool.QueryRow(ctx,
		selectAMIncident+` WHERE fingerprint = $1 AND ended_at IS NULL`,
		fingerprint)
	inc, err := scanAMIncident(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup open am_alerts by fingerprint: %w", err)
	}
	return inc, nil
}

// UpdateAMSlackRef records the Slack parent message ref against an
// incident. Idempotent: setting the same values is a no-op apart from
// bumping updated_at — re-running the handler after a retry is safe.
func (r *Repo) UpdateAMSlackRef(ctx context.Context, incidentID int64, slackChannel, slackTS string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE am_alerts
		SET slack_channel = $1,
		    slack_ts      = $2,
		    updated_at    = now()
		WHERE id = $3
	`, slackChannel, slackTS, incidentID)
	if err != nil {
		return fmt.Errorf("update am_alerts slack ref: %w", err)
	}
	return nil
}

// MarkAMResolved stamps ended_at on the live row for fingerprint and
// returns it. Returns (nil, nil) when no open row exists — the handler
// treats that as a late-resolve (resolved arrived before any firing
// webhook this process saw) and posts a standalone banner per ADR-0005.
func (r *Repo) MarkAMResolved(ctx context.Context, fingerprint string, endedAt time.Time) (*AMIncident, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE am_alerts
		SET ended_at   = $1,
		    updated_at = now()
		WHERE fingerprint = $2 AND ended_at IS NULL
		RETURNING id, fingerprint, alertname, labels, annotations,
		          started_at, ended_at, channel_slug, slack_channel, slack_ts,
		          rule_chain, resolved_notify, external_url, receiver,
		          created_at, updated_at
	`, endedAt, fingerprint)
	inc, err := scanAMIncident(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("mark am_alerts resolved: %w", err)
	}
	return inc, nil
}

// AppendAMEvent records one webhook delivery against an incident. The
// rawPayload bytes are stored verbatim (as JSONB) so the detail page
// can show the operator what AM actually sent. Caller passes the
// JSON-encoded bytes — we do not re-marshal.
func (r *Repo) AppendAMEvent(ctx context.Context, incidentID int64, eventType AMEventType, rawPayload []byte) error {
	if len(rawPayload) == 0 {
		// JSONB columns cannot be NULL or empty; the handler should
		// always pass at least `{}`. Fail loudly rather than silently
		// inserting an invalid row.
		return errors.New("append am_alert_events: rawPayload is empty")
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO am_alert_events (incident_id, event_type, raw_payload)
		VALUES ($1, $2, $3)
	`, incidentID, string(eventType), rawPayload)
	if err != nil {
		return fmt.Errorf("insert am_alert_events: %w", err)
	}
	return nil
}

// ListAMIncidents returns a filtered+paginated slice of incidents,
// newest first. The filter dimensions mirror the /alerts listing's
// query-string knobs; an empty filter lists every incident under the
// default limit.
func (r *Repo) ListAMIncidents(ctx context.Context, f AMListFilter) ([]AMIncident, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = amListDefaultLimit
	}
	if limit > amListMaxLimit {
		limit = amListMaxLimit
	}

	conds := []string{}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, "$"+itoa(len(args))))
	}
	switch f.Status {
	case "":
		// no predicate
	case "firing":
		conds = append(conds, "ended_at IS NULL")
	case "resolved":
		conds = append(conds, "ended_at IS NOT NULL")
	default:
		// Unknown value falls through to no predicate rather than
		// silently returning nothing — the web layer validates the
		// query-string input, this is just a safety net.
	}
	if f.Severity != "" {
		add("labels->>'severity' = %s", f.Severity)
	}
	if f.Alertname != "" {
		add("alertname = %s", f.Alertname)
	}
	if f.ChannelSlug != "" {
		add("channel_slug = %s", f.ChannelSlug)
	}
	if f.Receiver != "" {
		add("receiver = %s", f.Receiver)
	}
	if f.From != nil {
		add("started_at >= %s", *f.From)
	}
	if f.To != nil {
		add("started_at <= %s", *f.To)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	limitPlace := "$" + itoa(len(args))
	args = append(args, f.Offset)
	offsetPlace := "$" + itoa(len(args))
	q := selectAMIncident + where +
		" ORDER BY started_at DESC, id DESC LIMIT " + limitPlace + " OFFSET " + offsetPlace
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list am_alerts: %w", err)
	}
	defer rows.Close()
	var out []AMIncident
	for rows.Next() {
		inc, err := scanAMIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inc)
	}
	return out, rows.Err()
}

// GetAMIncident returns one incident by id, or (nil, nil) when none
// exists. The detail page uses nil-nil to render its 404.
func (r *Repo) GetAMIncident(ctx context.Context, id int64) (*AMIncident, error) {
	row := r.pool.QueryRow(ctx, selectAMIncident+` WHERE id = $1`, id)
	inc, err := scanAMIncident(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get am_alerts: %w", err)
	}
	return inc, nil
}

// ListAMIncidentsByFingerprint returns the recent incidents (newest
// first) for a given fingerprint, capped at limit. Powers the
// "fingerprint history" section on the detail page.
func (r *Repo) ListAMIncidentsByFingerprint(ctx context.Context, fingerprint string, limit int) ([]AMIncident, error) {
	if limit <= 0 {
		limit = amListDefaultLimit
	}
	if limit > amListMaxLimit {
		limit = amListMaxLimit
	}
	rows, err := r.pool.Query(ctx,
		selectAMIncident+` WHERE fingerprint = $1 ORDER BY started_at DESC, id DESC LIMIT $2`,
		fingerprint, limit)
	if err != nil {
		return nil, fmt.Errorf("list am_alerts by fingerprint: %w", err)
	}
	defer rows.Close()
	var out []AMIncident
	for rows.Next() {
		inc, err := scanAMIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inc)
	}
	return out, rows.Err()
}

// GetLatestAMEventPayload returns the raw_payload JSONB blob of the
// most recent am_alert_events row for the given incident, or
// (nil, pgx.ErrNoRows) when the incident exists but has recorded no
// events. The detail page renders this verbatim (after a pretty-print
// pass) inside its <details>Raw payload</details> section so operators
// can see exactly what AM sent.
func (r *Repo) GetLatestAMEventPayload(ctx context.Context, incidentID int64) ([]byte, error) {
	var payload []byte
	err := r.pool.QueryRow(ctx, `
		SELECT raw_payload
		FROM am_alert_events
		WHERE incident_id = $1
		ORDER BY received_at DESC, id DESC
		LIMIT 1
	`, incidentID).Scan(&payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("get latest am_alert_events payload: %w", err)
	}
	return payload, nil
}

// SweepAMResolved deletes every resolved incident whose ended_at is
// older than the cutoff. The am_alert_events foreign key's ON DELETE
// CASCADE clears the event trail in the same transaction. Active
// (ended_at IS NULL) rows are never touched. Returns the row count for
// the operator-visible "swept N alerts" log line.
func (r *Repo) SweepAMResolved(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM am_alerts
		WHERE ended_at IS NOT NULL AND ended_at < $1
	`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("sweep am_alerts: %w", err)
	}
	return tag.RowsAffected(), nil
}

// scanAMIncident reads a row produced by selectAMIncident (or the
// INSERT … RETURNING variant) into an *AMIncident. JSONB columns are
// scanned into []byte then json.Unmarshalled into the typed in-memory
// fields. The function pgx.ErrNoRows-passes through unchanged so
// callers can branch on it.
func scanAMIncident(row rowScanner) (*AMIncident, error) {
	var inc AMIncident
	var labelsBytes, annosBytes, notifyBytes []byte
	var slackChannel, slackTS sql.NullString
	if err := row.Scan(
		&inc.ID,
		&inc.Fingerprint,
		&inc.Alertname,
		&labelsBytes,
		&annosBytes,
		&inc.StartedAt,
		&inc.EndedAt,
		&inc.ChannelSlug,
		&slackChannel,
		&slackTS,
		&inc.RuleChain,
		&notifyBytes,
		&inc.ExternalURL,
		&inc.Receiver,
		&inc.CreatedAt,
		&inc.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if slackChannel.Valid {
		inc.SlackChannel = slackChannel.String
	}
	if slackTS.Valid {
		inc.SlackTS = slackTS.String
	}
	if len(labelsBytes) > 0 {
		if err := json.Unmarshal(labelsBytes, &inc.Labels); err != nil {
			return nil, fmt.Errorf("decode labels for am_alert id=%d: %w", inc.ID, err)
		}
	}
	if len(annosBytes) > 0 {
		if err := json.Unmarshal(annosBytes, &inc.Annotations); err != nil {
			return nil, fmt.Errorf("decode annotations for am_alert id=%d: %w", inc.ID, err)
		}
	}
	if len(notifyBytes) > 0 {
		if err := json.Unmarshal(notifyBytes, &inc.ResolvedNotify); err != nil {
			return nil, fmt.Errorf("decode resolved_notify for am_alert id=%d: %w", inc.ID, err)
		}
	}
	return &inc, nil
}

// stringMapOrEmpty replaces a nil map with an empty one so JSONB
// columns marked NOT NULL always receive `{}` instead of `null`.
func stringMapOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// stringSliceOrEmpty replaces a nil slice with an empty one so JSONB
// columns marked NOT NULL always receive `[]` instead of `null`.
func stringSliceOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
