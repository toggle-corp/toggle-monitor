package coalesce

import (
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/group"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// toRow projects a live group into its persisted form.
func (m *Manager) toRow(lg *liveGroup) store.IncidentGroupRow {
	row := store.IncidentGroupRow{
		ID:             lg.id,
		ChannelSlug:    lg.g.Channel,
		OpenedAt:       lg.g.OpenedAt,
		DigestChannel:  lg.g.DigestChannel,
		DigestTS:       lg.g.DigestTS,
		Posted:         lg.g.Posted,
		Closed:         lg.g.Closed,
		LastFlushAt:    timePtr(lg.g.LastFlushAt),
		LastReminderAt: timePtr(lg.g.LastReminderAt),
	}
	for _, mem := range lg.g.Members {
		row.Members = append(row.Members, store.IncidentGroupMemberRow{
			MonitorSlug: mem.Slug,
			State:       string(mem.State),
			JoinedAt:    mem.JoinedAt,
			DownSince:   timePtr(mem.DownSince),
			UpSince:     timePtr(mem.UpSince),
			ChangedAt:   mem.ChangedAt,
			Rendered:    mem.Rendered,
		})
	}
	return row
}

// fromRow rehydrates a live group from its persisted form, re-applying
// the current config (intervals are not persisted). Display info is
// empty until members next transition.
func (m *Manager) fromRow(row store.IncidentGroupRow) *liveGroup {
	g := group.New(row.ChannelSlug, row.OpenedAt, m.cfg)
	g.DigestChannel = row.DigestChannel
	g.DigestTS = row.DigestTS
	g.Posted = row.Posted
	g.Closed = row.Closed
	g.LastFlushAt = derefTime(row.LastFlushAt)
	g.LastReminderAt = derefTime(row.LastReminderAt)
	g.Members = make(map[string]*group.Member, len(row.Members))
	for _, mr := range row.Members {
		g.Members[mr.MonitorSlug] = &group.Member{
			Slug:      mr.MonitorSlug,
			State:     group.MemberState(mr.State),
			JoinedAt:  mr.JoinedAt,
			DownSince: derefTime(mr.DownSince),
			UpSince:   derefTime(mr.UpSince),
			ChangedAt: mr.ChangedAt,
			Rendered:  mr.Rendered,
		}
	}
	return &liveGroup{id: row.ID, g: g, info: map[string]MemberInfo{}}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
