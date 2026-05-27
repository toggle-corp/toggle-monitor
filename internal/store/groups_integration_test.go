//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

func TestIncidentGroupLifecycle(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	// No open group initially.
	if _, ok, err := repo.FindOpenIncidentGroup(ctx, "ops"); err != nil || ok {
		t.Fatalf("expected no open group: ok=%v err=%v", ok, err)
	}

	id, err := repo.CreateIncidentGroup(ctx, "ops", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Save header + two members.
	down := now
	g := store.IncidentGroupRow{
		ID:            id,
		ChannelSlug:   "ops",
		DigestChannel: "C123",
		DigestTS:      "1700000000.0001",
		Posted:        true,
		LastFlushAt:   &now,
		Members: []store.IncidentGroupMemberRow{
			{MonitorSlug: "a", State: "down", JoinedAt: now, DownSince: &down, ChangedAt: now, Rendered: "active"},
			{MonitorSlug: "b", State: "recovered", JoinedAt: now, DownSince: &down, ChangedAt: now, Rendered: "recovered"},
		},
	}
	if err := repo.SaveIncidentGroup(ctx, g); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := repo.FindOpenIncidentGroup(ctx, "ops")
	if err != nil || !ok {
		t.Fatalf("find open: ok=%v err=%v", ok, err)
	}
	if !got.Posted || got.DigestTS != "1700000000.0001" || len(got.Members) != 2 {
		t.Fatalf("reloaded group mismatch: %+v", got)
	}
	if got.Members[0].MonitorSlug != "a" || got.Members[0].State != "down" {
		t.Fatalf("member a mismatch: %+v", got.Members[0])
	}

	// Upsert: flip a to recovered, confirm no duplicate row.
	got.Members[0].State = "recovered"
	got.Members[0].Rendered = "recovered"
	if err := repo.SaveIncidentGroup(ctx, got); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	reloaded, _, _ := repo.FindOpenIncidentGroup(ctx, "ops")
	if len(reloaded.Members) != 2 || reloaded.Members[0].State != "recovered" {
		t.Fatalf("upsert produced wrong members: %+v", reloaded.Members)
	}

	// ListOpenIncidentGroups sees it.
	open, err := repo.ListOpenIncidentGroups(ctx)
	if err != nil || len(open) != 1 || open[0].ID != id {
		t.Fatalf("list open: %v / %+v", err, open)
	}

	// Close it: the partial unique index frees the channel for a new group.
	reloaded.Closed = true
	if err := repo.SaveIncidentGroup(ctx, reloaded); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, ok, _ := repo.FindOpenIncidentGroup(ctx, "ops"); ok {
		t.Fatal("closed group should not be found as open")
	}
	if _, err := repo.CreateIncidentGroup(ctx, "ops", now.Add(time.Hour)); err != nil {
		t.Fatalf("should be able to open a new group after close: %v", err)
	}
}
