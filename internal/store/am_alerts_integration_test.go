//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// baseInsert is a convenience factory used by most am_alerts tests —
// fingerprint and channelSlug are the routinely-varied knobs, the rest
// stays uniform so test bodies focus on the methods under test.
func baseInsert(fp, channel string, started time.Time) store.AMIncidentInsert {
	return store.AMIncidentInsert{
		Fingerprint:    fp,
		Alertname:      "HighCPU",
		Labels:         map[string]string{"alertname": "HighCPU", "severity": "critical", "instance": "pod-1"},
		Annotations:    map[string]string{"summary": "CPU is hot", "runbook_url": "https://runbooks.example.test/cpu"},
		StartedAt:      started,
		ChannelSlug:    channel,
		RuleChain:      "root>critical",
		ResolvedNotify: []string{"ops-team", "<!channel>"},
		ExternalURL:    "https://am.prod.example.test",
		Receiver:       "toggle_monitor",
	}
}

func TestInsertOpenAMIncident_insertAndIdempotency(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	started := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	first, inserted, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started))
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert should report inserted=true")
	}
	if first == nil || first.ID == 0 {
		t.Fatalf("first insert returned nil/zero-id row: %+v", first)
	}
	if first.Alertname != "HighCPU" {
		t.Errorf("Alertname: got %q, want HighCPU", first.Alertname)
	}
	if first.Labels["severity"] != "critical" {
		t.Errorf("Labels[severity]: got %q", first.Labels["severity"])
	}
	if first.Annotations["summary"] != "CPU is hot" {
		t.Errorf("Annotations[summary]: got %q", first.Annotations["summary"])
	}
	if len(first.ResolvedNotify) != 2 || first.ResolvedNotify[0] != "ops-team" {
		t.Errorf("ResolvedNotify roundtrip: got %v", first.ResolvedNotify)
	}
	if first.EndedAt.Valid {
		t.Errorf("EndedAt should be NULL for a fresh insert, got %+v", first.EndedAt)
	}
	if first.ChannelSlug != "ops" {
		t.Errorf("ChannelSlug: got %q", first.ChannelSlug)
	}

	// Second call for the same fingerprint while the first is open:
	// the partial unique index makes the INSERT a no-op; we still get
	// the existing live row back so the handler can read slack_ts.
	again, inserted, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started.Add(time.Minute)))
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Error("second insert (same fingerprint open) should report inserted=false")
	}
	if again == nil || again.ID != first.ID {
		t.Errorf("expected same ID on idempotency replay, got %+v", again)
	}
}

func TestInsertOpenAMIncident_newRowAfterResolve(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	started := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	first, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started))
	if err != nil {
		t.Fatalf("insert #1: %v", err)
	}
	resolveAt := started.Add(10 * time.Minute)
	if _, err := repo.MarkAMResolved(ctx, "fp-1", resolveAt); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	second, inserted, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started.Add(time.Hour)))
	if err != nil {
		t.Fatalf("insert #2: %v", err)
	}
	if !inserted {
		t.Fatal("re-firing after resolve should produce a fresh row (inserted=true)")
	}
	if second.ID == first.ID {
		t.Errorf("expected a new ID after resolve, got same %d", first.ID)
	}
}

func TestUpdateAMSlackRef_roundtrip(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	started := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	row, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.UpdateAMSlackRef(ctx, row.ID, "C123", "1700000000.0001"); err != nil {
		t.Fatalf("update slack ref: %v", err)
	}
	got, err := repo.GetAMIncident(ctx, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SlackChannel != "C123" || got.SlackTS != "1700000000.0001" {
		t.Errorf("slack ref roundtrip: got (%q, %q)", got.SlackChannel, got.SlackTS)
	}
	// Idempotent: same values, no error.
	if err := repo.UpdateAMSlackRef(ctx, row.ID, "C123", "1700000000.0001"); err != nil {
		t.Errorf("idempotent re-update: %v", err)
	}
}

func TestMarkAMResolved_happyPath(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	started := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	row, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	endAt := started.Add(20 * time.Minute)
	resolved, err := repo.MarkAMResolved(ctx, "fp-1", endAt)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved == nil || resolved.ID != row.ID {
		t.Fatalf("expected resolved row id=%d, got %+v", row.ID, resolved)
	}
	if !resolved.EndedAt.Valid || !resolved.EndedAt.Time.Equal(endAt) {
		t.Errorf("EndedAt: got %+v, want %v", resolved.EndedAt, endAt)
	}
}

func TestMarkAMResolved_noOpenRow(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	end := time.Date(2026, 6, 4, 10, 30, 0, 0, time.UTC)
	got, err := repo.MarkAMResolved(ctx, "fp-never-fired", end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil incident for late-resolve, got %+v", got)
	}
}

func TestAppendAMEvent_andSweepCascades(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	started := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	row, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-1", "ops", started))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.AppendAMEvent(ctx, row.ID, store.AMEventFiring, []byte(`{"k":"v1"}`)); err != nil {
		t.Fatalf("append #1: %v", err)
	}
	if err := repo.AppendAMEvent(ctx, row.ID, store.AMEventRepeatFiring, []byte(`{"k":"v2"}`)); err != nil {
		t.Fatalf("append #2: %v", err)
	}
	endAt := started.Add(5 * time.Minute)
	if _, err := repo.MarkAMResolved(ctx, "fp-1", endAt); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Sweep keeps rows resolved before cutoff; here cutoff is well after
	// endAt so the row should go away, taking events with it via cascade.
	cutoff := endAt.Add(time.Hour)
	n, err := repo.SweepAMResolved(ctx, cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("Sweep rows: got %d, want 1", n)
	}
	gone, err := repo.GetAMIncident(ctx, row.ID)
	if err != nil {
		t.Fatalf("get after sweep: %v", err)
	}
	if gone != nil {
		t.Errorf("expected nil after sweep, got %+v", gone)
	}
}

func TestSweepAMResolved_leavesFiringRowsUntouched(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	started := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	openRow, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-open", "ops", started))
	if err != nil {
		t.Fatalf("insert open: %v", err)
	}
	resolvedRow, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp-resolved", "ops", started))
	if err != nil {
		t.Fatalf("insert resolved: %v", err)
	}
	endAt := started.Add(5 * time.Minute)
	if _, err := repo.MarkAMResolved(ctx, "fp-resolved", endAt); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	n, err := repo.SweepAMResolved(ctx, endAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("Sweep rows: got %d, want 1", n)
	}
	open, err := repo.GetAMIncident(ctx, openRow.ID)
	if err != nil {
		t.Fatalf("get open: %v", err)
	}
	if open == nil {
		t.Error("firing row should survive sweep")
	}
	gone, err := repo.GetAMIncident(ctx, resolvedRow.ID)
	if err != nil {
		t.Fatalf("get resolved: %v", err)
	}
	if gone != nil {
		t.Errorf("resolved row should be swept, got %+v", gone)
	}
}

func TestGetAMIncident_missingReturnsNilNil(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	got, err := repo.GetAMIncident(ctx, 99999)
	if err != nil {
		t.Errorf("expected nil error for missing row, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil row, got %+v", got)
	}
}

func TestListAMIncidents_filtersAcrossEveryDimension(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	// Build a varied population that lets each filter discriminate.
	inserts := []store.AMIncidentInsert{
		{
			Fingerprint: "a", Alertname: "HighCPU",
			Labels: map[string]string{"alertname": "HighCPU", "severity": "critical"},
			StartedAt:   t0,
			ChannelSlug: "ops", Receiver: "toggle_monitor", RuleChain: "root",
			Annotations: map[string]string{}, ResolvedNotify: []string{}, ExternalURL: "https://am1.example.test",
		},
		{
			Fingerprint: "b", Alertname: "LowDisk",
			Labels: map[string]string{"alertname": "LowDisk", "severity": "warning"},
			StartedAt:   t0.Add(time.Minute),
			ChannelSlug: "infra", Receiver: "toggle_monitor", RuleChain: "root",
			Annotations: map[string]string{}, ResolvedNotify: []string{}, ExternalURL: "https://am1.example.test",
		},
		{
			Fingerprint: "c", Alertname: "HighCPU",
			Labels: map[string]string{"alertname": "HighCPU", "severity": "warning"},
			StartedAt:   t0.Add(2 * time.Minute),
			ChannelSlug: "ops", Receiver: "other_receiver", RuleChain: "root",
			Annotations: map[string]string{}, ResolvedNotify: []string{}, ExternalURL: "https://am1.example.test",
		},
	}
	for _, in := range inserts {
		if _, _, err := repo.InsertOpenAMIncident(ctx, in); err != nil {
			t.Fatalf("insert %s: %v", in.Fingerprint, err)
		}
	}
	// Resolve `b` so we can test status filter.
	if _, err := repo.MarkAMResolved(ctx, "b", t0.Add(5*time.Minute)); err != nil {
		t.Fatalf("resolve b: %v", err)
	}

	check := func(name string, f store.AMListFilter, wantFps ...string) {
		t.Helper()
		got, err := repo.ListAMIncidents(ctx, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		gotFps := make([]string, 0, len(got))
		for _, r := range got {
			gotFps = append(gotFps, r.Fingerprint)
		}
		if len(gotFps) != len(wantFps) {
			t.Errorf("%s: got %v, want %v", name, gotFps, wantFps)
			return
		}
		// Order: started_at DESC. Verify membership and that the slice
		// contains exactly the wanted set (test cases pass want in
		// DESC-by-started-at order to also pin ordering).
		for i := range wantFps {
			if gotFps[i] != wantFps[i] {
				t.Errorf("%s: got %v, want %v", name, gotFps, wantFps)
				return
			}
		}
	}

	check("no filter", store.AMListFilter{}, "c", "b", "a")
	check("status=firing", store.AMListFilter{Status: "firing"}, "c", "a")
	check("status=resolved", store.AMListFilter{Status: "resolved"}, "b")
	check("severity=critical", store.AMListFilter{Severity: "critical"}, "a")
	check("alertname=HighCPU", store.AMListFilter{Alertname: "HighCPU"}, "c", "a")
	check("channel=infra", store.AMListFilter{ChannelSlug: "infra"}, "b")
	check("receiver=other", store.AMListFilter{Receiver: "other_receiver"}, "c")

	from := t0.Add(time.Minute).Add(-time.Second)
	to := t0.Add(time.Minute).Add(time.Second)
	check("from/to window", store.AMListFilter{From: &from, To: &to}, "b")

	// Limit + offset: total 3, slice [1:2] in DESC order is "b".
	check("limit+offset", store.AMListFilter{Limit: 1, Offset: 1}, "b")
}

func TestListAMIncidentsByFingerprint_DESCAndLimit(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	// Three incidents with the same fingerprint, separated by resolves.
	for i := 0; i < 3; i++ {
		started := t0.Add(time.Duration(i) * time.Hour)
		if _, _, err := repo.InsertOpenAMIncident(ctx, baseInsert("fp", "ops", started)); err != nil {
			t.Fatalf("insert #%d: %v", i, err)
		}
		if i < 2 {
			if _, err := repo.MarkAMResolved(ctx, "fp", started.Add(30*time.Minute)); err != nil {
				t.Fatalf("resolve #%d: %v", i, err)
			}
		}
	}
	got, err := repo.ListAMIncidentsByFingerprint(ctx, "fp", 10)
	if err != nil {
		t.Fatalf("list by fp: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("count: got %d, want 3", len(got))
	}
	// DESC by started_at: i=2, i=1, i=0
	if !got[0].StartedAt.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("ordering: got[0].StartedAt %v", got[0].StartedAt)
	}
	limited, err := repo.ListAMIncidentsByFingerprint(ctx, "fp", 2)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limited count: got %d, want 2", len(limited))
	}
}
