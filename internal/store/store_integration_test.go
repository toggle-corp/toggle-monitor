//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// newRepo spins up a Postgres container, applies migrations, and
// returns a Repo against the resulting pool.
func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

func TestReconcileMonitor_insertsAndIsIdempotent(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	spec := store.MonitorSpec{
		Slug:         "bastion",
		FriendlyName: "Bastion",
		URL:          "http://bastion/health",
		Tags: []string{"gateways"},
		Source:       store.SourceStatic,
	}

	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Reconcile again — should not error and should preserve runtime state.
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("second reconcile (idempotency): %v", err)
	}

	got, err := repo.GetMonitor(ctx, "bastion")
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if got.FriendlyName != "Bastion" {
		t.Errorf("FriendlyName: got %q, want Bastion", got.FriendlyName)
	}
	if got.Status != alert.StatusUp {
		t.Errorf("default Status: got %q, want %q", got.Status, alert.StatusUp)
	}
	if got.Archived {
		t.Error("expected non-archived after reconcile")
	}
}

func TestReconcileMonitor_preservesRuntimeStateOnConfigChange(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	spec := store.MonitorSpec{
		Slug:         "api",
		FriendlyName: "API v1",
		URL:          "http://api/health",
		Tags: []string{"production-apis"},
		Source:       store.SourceStatic,
	}
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	// Simulate a transition: monitor went down.
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	down := alert.State{Status: alert.StatusDown, OpenedAt: t0}
	openEvent := &alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 500, Error: "down"}
	if err := repo.ApplyCheck(ctx, "api", down, t0, 500, "down", openEvent); err != nil {
		t.Fatalf("apply down: %v", err)
	}

	// Now re-reconcile with a renamed friendly name. Runtime state must
	// stay put.
	spec.FriendlyName = "API v1 (renamed)"
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	got, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.FriendlyName != "API v1 (renamed)" {
		t.Errorf("FriendlyName: got %q, want %q", got.FriendlyName, "API v1 (renamed)")
	}
	if got.Status != alert.StatusDown {
		t.Errorf("Status: got %q, want %q (reconcile must preserve runtime state)", got.Status, alert.StatusDown)
	}
	if got.OpenedAt == nil || !got.OpenedAt.Equal(t0) {
		t.Errorf("OpenedAt: got %v, want %v", got.OpenedAt, t0)
	}
}

func TestApplyCheck_fullUptimeLifecycle(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	spec := store.MonitorSpec{
		Slug:         "api",
		FriendlyName: "API",
		URL:          "http://api/health",
		Tags: []string{"production-apis"},
		Source:       store.SourceStatic,
	}
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	downEvent := &alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "Service Unavailable"}
	if err := repo.ApplyCheck(ctx,
		"api",
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 503, "Service Unavailable", downEvent,
	); err != nil {
		t.Fatalf("ApplyCheck (down): %v", err)
	}

	// 15 minutes later, monitor recovers.
	t1 := t0.Add(15 * time.Minute)
	resolveEvent := &alert.Event{Type: alert.EventResolve, At: t1, Downtime: 15 * time.Minute}
	if err := repo.ApplyCheck(ctx, "api", alert.State{Status: alert.StatusUp}, t1, 200, "", resolveEvent); err != nil {
		t.Fatalf("ApplyCheck (resolve): %v", err)
	}

	// Monitor row should now be up with cleared opened_at.
	row, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != alert.StatusUp {
		t.Errorf("Status: got %q, want up", row.Status)
	}
	if row.OpenedAt != nil {
		t.Errorf("OpenedAt: got %v, want nil after resolve", row.OpenedAt)
	}

	// Two alert events recorded.
	events, err := repo.ListAlertsForMonitor(ctx, "api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("alert event count: got %d, want 2", len(events))
	}
	// Newest first.
	if events[0].Type != alert.EventResolve {
		t.Errorf("events[0].Type: got %q, want resolve", events[0].Type)
	}
	if events[0].DowntimeSeconds == nil || *events[0].DowntimeSeconds != int64((15 * time.Minute).Seconds()) {
		t.Errorf("downtime: got %v, want %d", events[0].DowntimeSeconds, int64((15 * time.Minute).Seconds()))
	}
	if events[1].Type != alert.EventOpen {
		t.Errorf("events[1].Type: got %q, want open", events[1].Type)
	}
}

func TestApplyCheck_nilEvent_onlyUpdatesLastFields(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	spec := store.MonitorSpec{Slug: "api", FriendlyName: "API", URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic}
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Need a group; the store doesn't validate cross-refs (config does).

	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	if err := repo.ApplyCheck(ctx, "api", alert.State{Status: alert.StatusUp}, t0, 200, "", nil); err != nil {
		t.Fatalf("ApplyCheck (no event): %v", err)
	}

	events, err := repo.ListAlertsForMonitor(ctx, "api", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected no alert events, got %d", len(events))
	}

	row, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if row.LastCheckedAt == nil || !row.LastCheckedAt.Equal(t0) {
		t.Errorf("LastCheckedAt: got %v, want %v", row.LastCheckedAt, t0)
	}
	if row.LastStatusCode == nil || *row.LastStatusCode != 200 {
		t.Errorf("LastStatusCode: got %v, want 200", row.LastStatusCode)
	}
}

func TestListActiveMonitors_ordersByStatusGroupName(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	// Insert three monitors in deliberately unsorted order.
	specs := []store.MonitorSpec{
		{Slug: "ups-up", FriendlyName: "Beta", URL: "http://x", Tags: []string{"g2"}, Source: store.SourceStatic},
		{Slug: "down-1", FriendlyName: "Alpha", URL: "http://x", Tags: []string{"g1"}, Source: store.SourceStatic},
		{Slug: "down-2", FriendlyName: "Gamma", URL: "http://x", Tags: []string{"g1"}, Source: store.SourceStatic},
	}
	for _, s := range specs {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %s: %v", s.Slug, err)
		}
	}
	// Mark down-1 and down-2 as down.
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	for _, s := range []string{"down-1", "down-2"} {
		if err := repo.ApplyCheck(ctx, s,
			alert.State{Status: alert.StatusDown, OpenedAt: t0},
			t0, 500, "x",
			&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 500, Error: "x"},
		); err != nil {
			t.Fatalf("apply down %s: %v", s, err)
		}
	}

	list, err := repo.ListActiveMonitors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("count: got %d, want 3", len(list))
	}
	// Expect: down-1 (down/g1/Alpha), down-2 (down/g1/Gamma), ups-up (up/g2/Beta).
	wantSlugs := []string{"down-1", "down-2", "ups-up"}
	for i, slug := range wantSlugs {
		if list[i].Slug != slug {
			t.Errorf("list[%d].Slug: got %q, want %q", i, list[i].Slug, slug)
		}
	}
}

func TestSoftDeleteMonitor_archivesAndPreservesHistory(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://api", Tags: []string{"g"}, Source: store.SourceStatic,
	}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	_ = repo.ApplyCheck(ctx, "api",
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 503, "x",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "x"},
	)

	if err := repo.SoftDeleteMonitor(ctx, "api", "removed from config"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	got, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Archived {
		t.Error("expected archived=true")
	}
	if got.ArchiveReason == nil || *got.ArchiveReason != "removed from config" {
		t.Errorf("archive_reason: got %v, want 'removed from config'", got.ArchiveReason)
	}
	events, _ := repo.ListAlertsForMonitor(ctx, "api", 10)
	if len(events) != 1 {
		t.Errorf("history should be preserved across soft-delete, got %d events", len(events))
	}

	// Default listing should hide it.
	listing, _ := repo.ListMonitors(ctx, store.ListMonitorsOpts{})
	for _, r := range listing.Items {
		if r.Slug == "api" {
			t.Error("archived monitor should be hidden from default listing")
		}
	}
	// Archived="any" should surface it alongside active rows.
	listing, _ = repo.ListMonitors(ctx, store.ListMonitorsOpts{Archived: "any"})
	found := false
	for _, r := range listing.Items {
		if r.Slug == "api" {
			found = true
		}
	}
	if !found {
		t.Error("archived monitor should appear with Archived=any")
	}
	// Archived="archived" should return ONLY the archived row.
	listing, _ = repo.ListMonitors(ctx, store.ListMonitorsOpts{Archived: "archived"})
	if len(listing.Items) != 1 || listing.Items[0].Slug != "api" {
		t.Errorf("Archived=archived should return only the archived row, got %+v", listing.Items)
	}
}

func TestListActiveBySource_filtersAndExcludesArchived(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	specs := []store.MonitorSpec{
		{Slug: "s1", FriendlyName: "S1", URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic},
		{Slug: "s2", FriendlyName: "S2", URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic},
		{Slug: "k1", FriendlyName: "K1", URL: "http://x", Tags: []string{"g"}, Source: store.SourceKube},
	}
	for _, s := range specs {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	_ = repo.SoftDeleteMonitor(ctx, "s2", "removed")

	out, err := repo.ListActiveBySource(ctx, store.SourceStatic)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Slug != "s1" {
		t.Errorf("expected only s1, got %+v", out)
	}
}

func TestHomepageStats_countsByStatus(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	for _, s := range []string{"a", "b", "c"} {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: s, FriendlyName: s, URL: "http://x", Tags: []string{"g"}, Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", s, err)
		}
	}
	// Knock "c" down.
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	if err := repo.ApplyCheck(ctx, "c",
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 500, "x",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 500, Error: "x"},
	); err != nil {
		t.Fatalf("apply down: %v", err)
	}

	stats, err := repo.HomepageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Up != 2 {
		t.Errorf("Up: got %d, want 2", stats.Up)
	}
	if stats.Down != 1 {
		t.Errorf("Down: got %d, want 1", stats.Down)
	}
}
