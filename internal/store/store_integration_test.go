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
		Tags:         []string{"gateways"},
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
		Tags:         []string{"production-apis"},
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
		Tags:         []string{"production-apis"},
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
	listing, err := repo.ListAlertsForMonitor(ctx, "api", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	events := listing.Items
	if len(events) != 2 {
		t.Fatalf("alert event count: got %d, want 2", len(events))
	}
	// Newest first.
	if events[0].Type != alert.EventResolve {
		t.Errorf("events[0].Type: got %q, want resolve", events[0].Type)
	}
	if events[0].DowntimeSeconds == nil || *events[0].DowntimeSeconds != int64((15*time.Minute).Seconds()) {
		t.Errorf("downtime: got %v, want %d", events[0].DowntimeSeconds, int64((15 * time.Minute).Seconds()))
	}
	if events[1].Type != alert.EventOpen {
		t.Errorf("events[1].Type: got %q, want open", events[1].Type)
	}
}

// TestUptimeOpenFailed_markAndClear covers the marker that tells a
// recovery whose Open never reached Slack apart from one nobody was
// paged for: it is set only during an open incident, and storing a
// thread ref or resolving clears it.
func TestUptimeOpenFailed_markAndClear(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	spec := store.MonitorSpec{Slug: "api", FriendlyName: "API", URL: "https://api.example.test/health", Source: store.SourceStatic}
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	marked := func(want bool, step string) {
		t.Helper()
		row, err := repo.GetMonitor(ctx, "api")
		if err != nil {
			t.Fatalf("%s: get monitor: %v", step, err)
		}
		if got := row.UptimeOpenFailedAt != nil; got != want {
			t.Errorf("%s: UptimeOpenFailedAt set = %v, want %v", step, got, want)
		}
	}

	// No open incident: nothing to mark.
	if err := repo.MarkUptimeOpenFailed(ctx, "api"); err != nil {
		t.Fatalf("mark while up: %v", err)
	}
	marked(false, "mark while up")

	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	open := &alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503}
	if err := repo.ApplyCheck(ctx, "api", alert.State{Status: alert.StatusDown, OpenedAt: t0}, t0, 503, "", open); err != nil {
		t.Fatalf("ApplyCheck (open): %v", err)
	}
	if err := repo.MarkUptimeOpenFailed(ctx, "api"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	marked(true, "mark during incident")

	if err := repo.SetUptimeThread(ctx, "api", "C1", "1700000000.000100"); err != nil {
		t.Fatalf("SetUptimeThread: %v", err)
	}
	marked(false, "thread ref stored")

	if err := repo.MarkUptimeOpenFailed(ctx, "api"); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	marked(true, "re-mark")

	t1 := t0.Add(15 * time.Minute)
	resolve := &alert.Event{Type: alert.EventResolve, At: t1, Downtime: 15 * time.Minute}
	if err := repo.ApplyCheck(ctx, "api", alert.State{Status: alert.StatusUp}, t1, 200, "", resolve); err != nil {
		t.Fatalf("ApplyCheck (resolve): %v", err)
	}
	marked(false, "resolved")
}

// TestSetUptimeThread_outsideAnIncident_storesNothing: an Open whose
// post finishes after its incident resolved must not leave a thread ref
// on an up monitor, where the next incident would read it as its own.
func TestSetUptimeThread_outsideAnIncident_storesNothing(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	spec := store.MonitorSpec{Slug: "api", FriendlyName: "API", URL: "https://api.example.test/health", Source: store.SourceStatic}
	if err := repo.ReconcileMonitor(ctx, spec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := repo.SetUptimeThread(ctx, "api", "C1", "1700000000.000100"); err != nil {
		t.Fatalf("SetUptimeThread: %v", err)
	}
	row, err := repo.GetMonitor(ctx, "api")
	if err != nil {
		t.Fatal(err)
	}
	if row.UptimeThreadTS != nil {
		t.Errorf("UptimeThreadTS: got %q on an up monitor, want none", *row.UptimeThreadTS)
	}
}

// Total counts the monitor's whole history, not the page — the
// paginator sizes itself from it — and offset walks backwards through
// the newest-first order.
func TestListAlertsForMonitor_pagesAndTotals(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://x", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// A second monitor's events must not leak into either count or page.
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "other", FriendlyName: "Other", URL: "http://y", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile other: %v", err)
	}

	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		if err := repo.ApplyCheck(ctx, "api",
			alert.State{Status: alert.StatusDown, OpenedAt: at}, at, 503, "boom",
			&alert.Event{Type: alert.EventOpen, At: at, StatusCode: 503, Error: "boom"},
		); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if err := repo.ApplyCheck(ctx, "other",
		alert.State{Status: alert.StatusDown, OpenedAt: base}, base, 503, "elsewhere",
		&alert.Event{Type: alert.EventOpen, At: base, StatusCode: 503, Error: "elsewhere"},
	); err != nil {
		t.Fatalf("apply other: %v", err)
	}

	first, err := repo.ListAlertsForMonitor(ctx, "api", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 5 {
		t.Errorf("Total: got %d, want 5 (api's events only)", first.Total)
	}
	if len(first.Items) != 2 {
		t.Fatalf("page size: got %d items, want 2", len(first.Items))
	}
	if !first.Items[0].At.Equal(base.Add(4 * time.Hour)) {
		t.Errorf("first page should start at the newest event, got %v", first.Items[0].At)
	}

	last, err := repo.ListAlertsForMonitor(ctx, "api", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Items) != 1 || !last.Items[0].At.Equal(base) {
		t.Errorf("last page should hold the single oldest event, got %+v", last.Items)
	}

	past, err := repo.ListAlertsForMonitor(ctx, "api", 2, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(past.Items) != 0 || past.Total != 5 {
		t.Errorf("offset past the end: got %d items, total %d; want 0 items, total 5", len(past.Items), past.Total)
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

	listing, err := repo.ListAlertsForMonitor(ctx, "api", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	events := listing.Items
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
	alerts, _ := repo.ListAlertsForMonitor(ctx, "api", 10, 0)
	if len(alerts.Items) != 1 {
		t.Errorf("history should be preserved across soft-delete, got %d events", len(alerts.Items))
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

func TestListGoneMonitorSlugs_archivedAndAbsent(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	for _, s := range []store.MonitorSpec{
		{Slug: "live", FriendlyName: "Live", URL: "http://x", Source: store.SourceKube},
		{Slug: "archived", FriendlyName: "Archived", URL: "http://x", Source: store.SourceKube},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if err := repo.SoftDeleteMonitor(ctx, "archived", "kube ingress removed"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	// "never-existed" has no row at all — as gone as a soft-deleted one.
	gone, err := repo.ListGoneMonitorSlugs(ctx, []string{"live", "archived", "never-existed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 2 || gone[0] != "archived" || gone[1] != "never-existed" {
		t.Fatalf("gone = %v, want [archived never-existed]", gone)
	}

	if gone, err := repo.ListGoneMonitorSlugs(ctx, nil); err != nil || gone != nil {
		t.Fatalf("empty input should be a no-op: %v %v", gone, err)
	}
}

func TestListUpMonitorSlugs_onlyLiveUpMonitors(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	for _, slug := range []string{"up", "down", "paused", "archived"} {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "http://x", Source: store.SourceKube,
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	t0 := time.Now().UTC()
	if err := repo.ApplyCheck(ctx, "down",
		alert.State{Status: alert.StatusDown, OpenedAt: t0, LastReminderAt: t0},
		t0, 503, "down", &alert.Event{Type: alert.EventOpen, At: t0}); err != nil {
		t.Fatalf("apply check: %v", err)
	}
	if err := repo.MarkTemporaryPaused(ctx, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := repo.SoftDeleteMonitor(ctx, "archived", "kube ingress removed"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	up, err := repo.ListUpMonitorSlugs(ctx, []string{"up", "down", "paused", "archived", "never-existed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(up) != 1 || up[0] != "up" {
		t.Fatalf("up = %v, want [up]", up)
	}

	if up, err := repo.ListUpMonitorSlugs(ctx, nil); err != nil || up != nil {
		t.Fatalf("empty input should be a no-op: %v %v", up, err)
	}
}

func TestMonitorStatuses_oneQueryForEveryListedRow(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	for _, slug := range []string{"up", "down", "paused", "archived"} {
		if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug: slug, FriendlyName: slug, URL: "http://monitor.example.test", Source: store.SourceStatic,
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	t0 := time.Now().UTC()
	for _, slug := range []string{"down", "archived"} {
		if err := repo.ApplyCheck(ctx, slug,
			alert.State{Status: alert.StatusDown, OpenedAt: t0, LastReminderAt: t0},
			t0, 503, "down", &alert.Event{Type: alert.EventOpen, At: t0}); err != nil {
			t.Fatalf("apply check: %v", err)
		}
	}
	if err := repo.MarkTemporaryPaused(ctx, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := repo.SoftDeleteMonitor(ctx, "archived", "removed from config"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	got, err := repo.MonitorStatuses(ctx, []string{"up", "down", "paused", "archived", "never-existed"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]alert.Status{
		"up":       alert.StatusUp,
		"down":     alert.StatusDown,
		"paused":   alert.StatusTemporaryPaused,
		"archived": alert.StatusDown,
	}
	if len(got) != len(want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	for slug, status := range want {
		if got[slug] != status {
			t.Errorf("%s: got %q, want %q", slug, got[slug], status)
		}
	}

	if got, err := repo.MonitorStatuses(ctx, nil); err != nil || got != nil {
		t.Fatalf("empty input should be a no-op: %v %v", got, err)
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

func TestListMonitors_tagsFilterIsAndContainment(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	for _, s := range []store.MonitorSpec{
		{Slug: "a", FriendlyName: "A", URL: "http://a", Tags: []string{"prod", "web"}, Source: store.SourceStatic},
		{Slug: "b", FriendlyName: "B", URL: "http://b", Tags: []string{"prod", "api"}, Source: store.SourceStatic},
		{Slug: "c", FriendlyName: "C", URL: "http://c", Tags: []string{"staging", "web"}, Source: store.SourceStatic},
		{Slug: "d", FriendlyName: "D", URL: "http://d", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %s: %v", s.Slug, err)
		}
	}

	// Single tag — every row with `prod` matches.
	listing, err := repo.ListMonitors(ctx, store.ListMonitorsOpts{Tags: []string{"prod"}})
	if err != nil {
		t.Fatal(err)
	}
	gotSlugs := func(l store.MonitorListing) map[string]bool {
		m := map[string]bool{}
		for _, r := range l.Items {
			m[r.Slug] = true
		}
		return m
	}
	got := gotSlugs(listing)
	if !(got["a"] && got["b"]) || got["c"] || got["d"] {
		t.Errorf("Tags=[prod]: got %v, want {a,b}", got)
	}

	// AND containment — only `a` carries both prod AND web.
	listing, err = repo.ListMonitors(ctx, store.ListMonitorsOpts{Tags: []string{"prod", "web"}})
	if err != nil {
		t.Fatal(err)
	}
	got = gotSlugs(listing)
	if !got["a"] || got["b"] || got["c"] || got["d"] {
		t.Errorf("Tags=[prod,web]: got %v, want {a}", got)
	}

	// Empty Tags slice — no predicate.
	listing, _ = repo.ListMonitors(ctx, store.ListMonitorsOpts{Tags: []string{}})
	if listing.Total != 4 {
		t.Errorf("empty Tags: total = %d, want 4", listing.Total)
	}
}

func TestDistinctTags_sortedAcrossAllMonitors(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	for _, s := range []store.MonitorSpec{
		{Slug: "a", FriendlyName: "A", URL: "http://a", Tags: []string{"prod", "web"}, Source: store.SourceStatic},
		{Slug: "b", FriendlyName: "B", URL: "http://b", Tags: []string{"prod", "api"}, Source: store.SourceStatic},
		{Slug: "c", FriendlyName: "C", URL: "http://c", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %s: %v", s.Slug, err)
		}
	}
	// Archive `b` — its tags should still appear.
	if err := repo.SoftDeleteMonitor(ctx, "b", "x"); err != nil {
		t.Fatal(err)
	}

	tags, err := repo.DistinctTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api", "prod", "web"}
	if len(tags) != len(want) {
		t.Fatalf("DistinctTags: got %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("DistinctTags[%d]: got %q, want %q", i, tags[i], want[i])
		}
	}
}

// TestUpsertDiscoverySnapshot_vacatedSlug pins the transition-detection
// contract that drives kube monitor teardown: when a tuple's
// monitor_slug goes from a real slug to gone (added → kube-invalid /
// kube-ignored while the tuple stays observed), the upsert returns the
// slug it just vacated so the watcher can route it through the removal
// sink. No transition → empty string.
func TestUpsertDiscoverySnapshot_vacatedSlug(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	slug := "acme-api-example-test"
	added := store.DiscoverySnapshotRow{
		Namespace: "acme", IngressName: "api", Host: "api.example.test",
		Status: "added", MonitorSlug: &slug,
	}

	// Brand-new tuple — nothing vacated.
	vacated, err := repo.UpsertDiscoverySnapshot(ctx, added)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if vacated != "" {
		t.Errorf("new tuple should vacate nothing, got %q", vacated)
	}

	// Re-upsert unchanged — still nothing vacated.
	vacated, err = repo.UpsertDiscoverySnapshot(ctx, added)
	if err != nil {
		t.Fatalf("idempotent upsert: %v", err)
	}
	if vacated != "" {
		t.Errorf("unchanged slug should vacate nothing, got %q", vacated)
	}

	// Flip to kube-invalid, slug cleared, SAME tuple — vacates the old slug.
	reason := "kube-invalid: wildcard host not probeable"
	invalid := store.DiscoverySnapshotRow{
		Namespace: "acme", IngressName: "api", Host: "api.example.test",
		Status: "kube-invalid", Reason: &reason, MonitorSlug: nil,
	}
	vacated, err = repo.UpsertDiscoverySnapshot(ctx, invalid)
	if err != nil {
		t.Fatalf("invalidating upsert: %v", err)
	}
	if vacated != slug {
		t.Errorf("added→invalid transition should vacate %q, got %q", slug, vacated)
	}

	// Already invalid → no further teardown signal.
	vacated, err = repo.UpsertDiscoverySnapshot(ctx, invalid)
	if err != nil {
		t.Fatalf("repeat invalid upsert: %v", err)
	}
	if vacated != "" {
		t.Errorf("already-vacated tuple should vacate nothing, got %q", vacated)
	}
}
