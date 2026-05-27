package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// IncidentGroupRow is one coalescing group (a living per-channel
// incident) plus its members. It mirrors group.Group; the scheduler
// wiring converts between the two. Persisted so a restart reloads open
// groups and reattaches to the existing digest message instead of
// re-storming.
type IncidentGroupRow struct {
	ID             int64
	ChannelSlug    string
	OpenedAt       time.Time
	DigestChannel  string
	DigestTS       string
	Posted         bool
	Closed         bool
	LastFlushAt    *time.Time
	LastReminderAt *time.Time
	Members        []IncidentGroupMemberRow
}

// IncidentGroupMemberRow is one monitor's membership in a group.
type IncidentGroupMemberRow struct {
	MonitorSlug string
	State       string // down | recovering | recovered | paused
	JoinedAt    time.Time
	DownSince   *time.Time
	UpSince     *time.Time
	ChangedAt   time.Time
	Rendered    string
}

// CreateIncidentGroup opens a new group for a channel and returns its
// id. The partial unique index (closed = FALSE) enforces at most one
// open group per channel; a concurrent create surfaces as a constraint
// error the caller can treat as "already exists, reload it".
func (r *Repo) CreateIncidentGroup(ctx context.Context, channelSlug string, openedAt time.Time) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO incident_groups (channel_slug, opened_at)
		VALUES ($1, $2)
		RETURNING id
	`, channelSlug, openedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create incident group: %w", err)
	}
	return id, nil
}

// FindOpenIncidentGroup returns the open group for a channel (with its
// members) and true, or false when none is open.
func (r *Repo) FindOpenIncidentGroup(ctx context.Context, channelSlug string) (IncidentGroupRow, bool, error) {
	var g IncidentGroupRow
	err := r.pool.QueryRow(ctx, `
		SELECT id, channel_slug, opened_at, digest_channel, digest_ts,
		       posted, closed, last_flush_at, last_reminder_at
		FROM incident_groups
		WHERE channel_slug = $1 AND closed = FALSE
	`, channelSlug).Scan(&g.ID, &g.ChannelSlug, &g.OpenedAt, &g.DigestChannel,
		&g.DigestTS, &g.Posted, &g.Closed, &g.LastFlushAt, &g.LastReminderAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return IncidentGroupRow{}, false, nil
	}
	if err != nil {
		return IncidentGroupRow{}, false, fmt.Errorf("find open incident group: %w", err)
	}
	members, err := r.loadGroupMembers(ctx, g.ID)
	if err != nil {
		return IncidentGroupRow{}, false, err
	}
	g.Members = members
	return g, true, nil
}

// ListOpenIncidentGroups returns every open group (with members) for the
// startup reattach pass.
func (r *Repo) ListOpenIncidentGroups(ctx context.Context) ([]IncidentGroupRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, channel_slug, opened_at, digest_channel, digest_ts,
		       posted, closed, last_flush_at, last_reminder_at
		FROM incident_groups
		WHERE closed = FALSE
		ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("list open incident groups: %w", err)
	}
	var out []IncidentGroupRow
	for rows.Next() {
		var g IncidentGroupRow
		if err := rows.Scan(&g.ID, &g.ChannelSlug, &g.OpenedAt, &g.DigestChannel,
			&g.DigestTS, &g.Posted, &g.Closed, &g.LastFlushAt, &g.LastReminderAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan incident group: %w", err)
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Load members per group (small N of open groups).
	for i := range out {
		members, err := r.loadGroupMembers(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Members = members
	}
	return out, nil
}

func (r *Repo) loadGroupMembers(ctx context.Context, groupID int64) ([]IncidentGroupMemberRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT monitor_slug, state, joined_at, down_since, up_since, changed_at, rendered
		FROM incident_group_members
		WHERE group_id = $1
		ORDER BY monitor_slug
	`, groupID)
	if err != nil {
		return nil, fmt.Errorf("load group members: %w", err)
	}
	defer rows.Close()
	var out []IncidentGroupMemberRow
	for rows.Next() {
		var m IncidentGroupMemberRow
		if err := rows.Scan(&m.MonitorSlug, &m.State, &m.JoinedAt, &m.DownSince,
			&m.UpSince, &m.ChangedAt, &m.Rendered); err != nil {
			return nil, fmt.Errorf("scan group member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveIncidentGroup persists the group header and upserts its members in
// one transaction. Members are never deleted — recovered/struck rows
// stay as the incident record until the group closes. Called after each
// Evaluate so on-disk state always matches the in-memory group.
func (r *Repo) SaveIncidentGroup(ctx context.Context, g IncidentGroupRow) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		UPDATE incident_groups
		SET digest_channel = $2,
		    digest_ts = $3,
		    posted = $4,
		    closed = $5,
		    last_flush_at = $6,
		    last_reminder_at = $7,
		    updated_at = now()
		WHERE id = $1
	`, g.ID, g.DigestChannel, g.DigestTS, g.Posted, g.Closed,
		g.LastFlushAt, g.LastReminderAt); err != nil {
		return fmt.Errorf("update incident group: %w", err)
	}

	for _, m := range g.Members {
		if _, err := tx.Exec(ctx, `
			INSERT INTO incident_group_members
			    (group_id, monitor_slug, state, joined_at, down_since, up_since, changed_at, rendered)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (group_id, monitor_slug) DO UPDATE SET
			    state = EXCLUDED.state,
			    joined_at = EXCLUDED.joined_at,
			    down_since = EXCLUDED.down_since,
			    up_since = EXCLUDED.up_since,
			    changed_at = EXCLUDED.changed_at,
			    rendered = EXCLUDED.rendered
		`, g.ID, m.MonitorSlug, m.State, m.JoinedAt, m.DownSince,
			m.UpSince, m.ChangedAt, m.Rendered); err != nil {
			return fmt.Errorf("upsert group member %q: %w", m.MonitorSlug, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
