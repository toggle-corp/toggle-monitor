//go:build integration

package web_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// seedAMIncident is the AM-flavored sibling of the baseInsert helper in
// the store package: builds a varied AMIncidentInsert from the supplied
// knobs, leaves the rest of the fields at sensible defaults.
func seedAMIncident(t *testing.T, repo *store.Repo, fp, alertname, severity, channel, receiver string, started time.Time) *store.AMIncident {
	t.Helper()
	in := store.AMIncidentInsert{
		Fingerprint:    fp,
		Alertname:      alertname,
		Labels:         map[string]string{"alertname": alertname, "severity": severity, "instance": "pod-1", "namespace": "acme"},
		Annotations:    map[string]string{"summary": "CPU is hot", "runbook_url": "https://runbooks.example.test/cpu", "description": "Detailed body of the alert"},
		StartedAt:      started,
		ChannelSlug:    channel,
		RuleChain:      "root>critical",
		ResolvedNotify: []string{"ops-team", "<!channel>"},
		ExternalURL:    "https://am.prod.example.test",
		Receiver:       receiver,
	}
	row, _, err := repo.InsertOpenAMIncident(context.Background(), in)
	if err != nil {
		t.Fatalf("seed am incident %q: %v", fp, err)
	}
	return row
}

func TestAlertsListing_emptyState(t *testing.T) {
	srv, _ := newServer(t)
	resp, body := get(t, srv.Routes(), "/alerts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/alerts: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "No AM alerts have ever fired") {
		t.Errorf("expected empty-state message; first 800:\n%s", firstN(body, 800))
	}
}

func TestAlertsListing_rendersRowsAndStatusFilter(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	seedAMIncident(t, repo, "fp-a", "HighCPU", "critical", "ops", "toggle_monitor", t0)
	seedAMIncident(t, repo, "fp-b", "LowDisk", "warning", "infra", "toggle_monitor", t0.Add(time.Minute))
	if _, err := repo.MarkAMResolved(context.Background(), "fp-b", t0.Add(5*time.Minute)); err != nil {
		t.Fatalf("resolve fp-b: %v", err)
	}
	seedAMIncident(t, repo, "fp-c", "HighCPU", "warning", "ops", "other_receiver", t0.Add(2*time.Minute))

	// Unfiltered: all three present.
	resp, body := get(t, srv.Routes(), "/alerts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/alerts: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"HighCPU", "LowDisk", "ops", "infra"} {
		if !strings.Contains(body, want) {
			t.Errorf("/alerts body missing %q; first 1500:\n%s", want, firstN(body, 1500))
		}
	}

	// status=firing keeps a and c (LowDisk was resolved).
	_, body = get(t, srv.Routes(), "/alerts?status=firing")
	if !strings.Contains(body, "HighCPU") {
		t.Errorf("status=firing should keep HighCPU rows; first 1500:\n%s", firstN(body, 1500))
	}
	if strings.Contains(body, "LowDisk") {
		t.Errorf("status=firing should drop resolved LowDisk row; first 1500:\n%s", firstN(body, 1500))
	}

	// status=resolved keeps only b.
	_, body = get(t, srv.Routes(), "/alerts?status=resolved")
	if !strings.Contains(body, "LowDisk") {
		t.Errorf("status=resolved should keep LowDisk row; first 1500:\n%s", firstN(body, 1500))
	}
	if strings.Contains(body, "HighCPU") {
		t.Errorf("status=resolved should drop firing HighCPU rows; first 1500:\n%s", firstN(body, 1500))
	}
}

func TestAlertsListing_severityFilter(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	seedAMIncident(t, repo, "fp-a", "HighCPU", "critical", "ops", "toggle_monitor", t0)
	seedAMIncident(t, repo, "fp-b", "LowDisk", "warning", "infra", "toggle_monitor", t0.Add(time.Minute))

	_, body := get(t, srv.Routes(), "/alerts?severity=critical")
	if !strings.Contains(body, "HighCPU") {
		t.Errorf("severity=critical should keep HighCPU; first 1500:\n%s", firstN(body, 1500))
	}
	if strings.Contains(body, "LowDisk") {
		t.Errorf("severity=critical should drop LowDisk (warning); first 1500:\n%s", firstN(body, 1500))
	}
}

func TestAlertsListing_alertnameFilter(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	seedAMIncident(t, repo, "fp-a", "HighCPU", "critical", "ops", "toggle_monitor", t0)
	seedAMIncident(t, repo, "fp-b", "LowDisk", "warning", "infra", "toggle_monitor", t0.Add(time.Minute))

	_, body := get(t, srv.Routes(), "/alerts?alertname=HighCPU")
	if !strings.Contains(body, "HighCPU") {
		t.Errorf("alertname=HighCPU should keep HighCPU; first 1500:\n%s", firstN(body, 1500))
	}
	if strings.Contains(body, "LowDisk") {
		t.Errorf("alertname=HighCPU should drop LowDisk; first 1500:\n%s", firstN(body, 1500))
	}
}

func TestAlertsListing_fromToFilter(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	seedAMIncident(t, repo, "fp-a", "HighCPU", "critical", "ops", "toggle_monitor", t0)
	seedAMIncident(t, repo, "fp-b", "LowDisk", "warning", "infra", "toggle_monitor", t0.Add(time.Hour))

	// Window around the second insert only.
	from := t0.Add(30 * time.Minute).Format("2006-01-02T15:04")
	to := t0.Add(2 * time.Hour).Format("2006-01-02T15:04")
	_, body := get(t, srv.Routes(), "/alerts?from="+from+"&to="+to)
	if !strings.Contains(body, "LowDisk") {
		t.Errorf("from/to window should keep LowDisk; first 1500:\n%s", firstN(body, 1500))
	}
	if strings.Contains(body, "HighCPU") {
		t.Errorf("from/to window should drop HighCPU (outside window); first 1500:\n%s", firstN(body, 1500))
	}
}

func TestAlertsListing_pagination(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	// Use unique alertnames so the page body uniquely identifies each
	// row regardless of which incident-id Postgres allocated.
	alertnames := []string{"AlertA", "AlertB", "AlertC", "AlertD", "AlertE"}
	for i, name := range alertnames {
		seedAMIncident(t, repo, fmt.Sprintf("fp-%s", name), name, "critical", "ops", "toggle_monitor", t0.Add(time.Duration(i)*time.Minute))
	}

	// Page 1 with per_page=2: gets the two newest (AlertE then AlertD).
	_, body := get(t, srv.Routes(), "/alerts?per_page=2&page=1")
	if !strings.Contains(body, "AlertE") || !strings.Contains(body, "AlertD") {
		t.Errorf("page 1 with per_page=2 should hold AlertE and AlertD; first 2000:\n%s", firstN(body, 2000))
	}
	if strings.Contains(body, "AlertA") || strings.Contains(body, "AlertB") {
		t.Errorf("page 1 with per_page=2 should not include AlertA/AlertB; first 2000:\n%s", firstN(body, 2000))
	}

	// Page 2 picks up the middle of the list.
	_, body = get(t, srv.Routes(), "/alerts?per_page=2&page=2")
	if !strings.Contains(body, "AlertC") || !strings.Contains(body, "AlertB") {
		t.Errorf("page 2 with per_page=2 should hold AlertC and AlertB; first 2000:\n%s", firstN(body, 2000))
	}
	if strings.Contains(body, "AlertE") || strings.Contains(body, "AlertD") {
		t.Errorf("page 2 should not include AlertE/AlertD; first 2000:\n%s", firstN(body, 2000))
	}
}

func TestAlertDetail_rendersAllSections(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	row := seedAMIncident(t, repo, "fp-1", "HighCPU", "critical", "ops", "toggle_monitor", t0)
	if err := repo.UpdateAMSlackRef(ctx, row.ID, "C12345", "1700000000.0001"); err != nil {
		t.Fatalf("update slack ref: %v", err)
	}
	if err := repo.AppendAMEvent(ctx, row.ID, store.AMEventFiring, []byte(`{"version":"4","status":"firing"}`)); err != nil {
		t.Fatalf("append event: %v", err)
	}

	resp, body := get(t, srv.Routes(), fmt.Sprintf("/alert/%d", row.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/alert/{id}: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		"HighCPU",
		"critical",
		"CPU is hot",           // annotations.summary
		"Detailed body",        // annotations.description
		"runbooks.example",     // annotations.runbook_url
		"toggle_monitor",       // receiver
		"am.prod.example.test", // externalURL
		"ops",                  // channel
		"root&gt;critical",     // rule chain (templ html-escapes ">")
		"ops-team",             // notify
		"C12345",               // slack channel
		"Raw payload",          // raw payload section header
		"FIRING",               // status label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/alert/{id} body missing %q; first 3000:\n%s", want, firstN(body, 3000))
		}
	}
}

func TestAlertDetail_notFound(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := get(t, srv.Routes(), "/alert/99999")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/alert/99999: got %d, want 404", resp.StatusCode)
	}
}

func TestAlertDetail_invalidID(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := get(t, srv.Routes(), "/alert/not-a-number")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/alert/not-a-number: got %d, want 404", resp.StatusCode)
	}
}

func TestAlertDetail_resolvedShowsDowntime(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	row := seedAMIncident(t, repo, "fp-1", "HighCPU", "critical", "ops", "toggle_monitor", t0)
	if _, err := repo.MarkAMResolved(ctx, "fp-1", t0.Add(15*time.Minute)); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	_, body := get(t, srv.Routes(), fmt.Sprintf("/alert/%d", row.ID))
	if !strings.Contains(body, "RESOLVED") {
		t.Errorf("resolved incident should render RESOLVED label; first 3000:\n%s", firstN(body, 3000))
	}
}

func TestAlertDetail_fingerprintHistory(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	// Three incidents on the same fingerprint, each separated by a resolve.
	var rows []*store.AMIncident
	for i := 0; i < 3; i++ {
		started := t0.Add(time.Duration(i) * time.Hour)
		row := seedAMIncident(t, repo, "fp-history", "HighCPU", "critical", "ops", "toggle_monitor", started)
		rows = append(rows, row)
		if i < 2 {
			if _, err := repo.MarkAMResolved(ctx, "fp-history", started.Add(30*time.Minute)); err != nil {
				t.Fatalf("resolve #%d: %v", i, err)
			}
		}
	}

	// Detail page of the latest incident should list the prior two.
	_, body := get(t, srv.Routes(), fmt.Sprintf("/alert/%d", rows[2].ID))
	for _, priorID := range []int64{rows[0].ID, rows[1].ID} {
		want := fmt.Sprintf("/alert/%d", priorID)
		if !strings.Contains(body, want) {
			t.Errorf("history section should link to prior incident %d (%q); first 3000:\n%s", priorID, want, firstN(body, 3000))
		}
	}
	// The current incident should not appear in its own history list — we
	// only spot-check that the prior IDs are present; the current ID is
	// in the page header for the back-link to /alerts so we cannot assert
	// non-presence directly, but we can at least confirm a "Fingerprint
	// history" section header is rendered.
	if !strings.Contains(body, "Fingerprint history") {
		t.Errorf("expected 'Fingerprint history' section; first 3000:\n%s", firstN(body, 3000))
	}
}

func TestAlertDetail_rawPayloadFallsBackGracefully(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

	// Insert the incident but record no events.
	row := seedAMIncident(t, repo, "fp-noevt", "HighCPU", "critical", "ops", "toggle_monitor", t0)

	resp, body := get(t, srv.Routes(), fmt.Sprintf("/alert/%d", row.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/alert/{id}: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "Raw payload") {
		t.Errorf("raw payload section should always render; first 3000:\n%s", firstN(body, 3000))
	}
}

func TestAlertsListing_filteredEmptyOffersClear(t *testing.T) {
	srv, repo := newServer(t)
	t0 := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	seedAMIncident(t, repo, "fp-a", "HighCPU", "critical", "ops", "toggle_monitor", t0)

	_, body := get(t, srv.Routes(), "/alerts?alertname=NoSuchAlertName")
	if !strings.Contains(body, "No alerts match") {
		t.Errorf("filtered-empty state should explain that filters narrowed to zero; first 1500:\n%s", firstN(body, 1500))
	}
	if !strings.Contains(body, "Clear filters") {
		t.Errorf("filtered-empty state should offer 'Clear filters'; first 1500:\n%s", firstN(body, 1500))
	}
}

func TestAlertsListing_navLink(t *testing.T) {
	srv, _ := newServer(t)
	// Homepage should advertise the Alerts nav link.
	_, body := get(t, srv.Routes(), "/")
	if !strings.Contains(body, `href="/alerts"`) {
		t.Errorf("nav should include /alerts link; first 800:\n%s", firstN(body, 800))
	}
}
