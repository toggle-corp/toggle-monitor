//go:build integration

package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
	"github.com/toggle-corp/toggle-monitor/internal/web"
	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// newServer spins up a Postgres container, applies migrations, seeds a
// handful of monitors, and returns a Server wired to a real Repo.
func newServer(t *testing.T) (*web.Server, *store.Repo) {
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
	repo := store.New(pool)
	return web.New(repo, nil), repo
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body, _ := io.ReadAll(rr.Result().Body)
	return rr.Result(), string(body)
}

func TestHealthz_alwaysOk(t *testing.T) {
	srv, _ := newServer(t)
	resp, body := get(t, srv.Routes(), "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status: got %d, want 200", resp.StatusCode)
	}
	if body != "ok" {
		t.Errorf("healthz body: got %q, want ok", body)
	}
}

func TestReadyz_flipsAfterMarkReady(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Routes()

	resp, _ := get(t, h, "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz before MarkReady: got %d, want 503", resp.StatusCode)
	}

	srv.MarkReady()
	resp, _ = get(t, h, "/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz after MarkReady: got %d, want 200", resp.StatusCode)
	}
}

func TestHomepage_rendersStatsAndLatestAlerts(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	for _, s := range []store.MonitorSpec{
		{Slug: "api", FriendlyName: "API", URL: "http://api", GroupSlug: "prod", Source: store.SourceStatic},
		{Slug: "web", FriendlyName: "Web", URL: "http://web", GroupSlug: "prod", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	_ = repo.ApplyCheck(ctx, "api",
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 503, "Service Unavailable",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "Service Unavailable"},
	)

	resp, body := get(t, srv.Routes(), "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("homepage status: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"Overview", "Latest alerts", "open", "api"} {
		if !strings.Contains(body, want) {
			t.Errorf("homepage body missing %q", want)
		}
	}
}

func TestMonitorsListing_rendersAndFilters(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	for _, s := range []store.MonitorSpec{
		{Slug: "api", FriendlyName: "API", URL: "http://api", GroupSlug: "prod", Source: store.SourceStatic},
		{Slug: "web", FriendlyName: "Web", URL: "http://web", GroupSlug: "prod", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	// Unfiltered: both monitors visible.
	resp, body := get(t, srv.Routes(), "/monitors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("monitors status: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"API", "Web", "/monitor/api", "/monitor/web"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Search filter narrows to one monitor.
	resp, body = get(t, srv.Routes(), "/monitors?q=API")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered status: got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "API") || strings.Contains(body, "/monitor/web") {
		t.Errorf("search 'API' should narrow to API only, got body:\n%s", firstN(body, 400))
	}

	// Empty result shows the clear-filters link.
	_, body = get(t, srv.Routes(), "/monitors?q=ZZZ_NONE")
	if !strings.Contains(body, "Clear filters") {
		t.Errorf("empty result should show clear-filters; got:\n%s", firstN(body, 400))
	}
}

func TestMonitorDetail_renders(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://api/health",
		GroupSlug: "prod", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	t0 := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	_ = repo.ApplyCheck(ctx, "api",
		alert.State{Status: alert.StatusDown, OpenedAt: t0},
		t0, 503, "Service Unavailable",
		&alert.Event{Type: alert.EventOpen, At: t0, StatusCode: 503, Error: "Service Unavailable"},
	)

	resp, body := get(t, srv.Routes(), "/monitor/api")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("monitor detail status: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"API", "http://api/health", "Service Unavailable", "503", "DOWN", "open"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
}

// fakeConfigLookup feeds the detail page a canned MonitorConfig per
// slug, mirroring what lifecycle's combinedPlanSource would produce
// at runtime. Tests assert that the rendered detail page actually
// contains the values from the lookup (interval, timeout, etc.).
type fakeConfigLookup map[string]templates.MonitorConfig

func (f fakeConfigLookup) ConfigFor(slug string) (templates.MonitorConfig, bool) {
	c, ok := f[slug]
	return c, ok
}

func TestMonitorDetail_rendersConfigDialogAndPreset(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "https://api/health",
		GroupSlug: "prod", Source: store.SourceKube,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	srv.SetConfigLookup(fakeConfigLookup{
		"api": {
			Preset:                "internal-api",
			HTTPMethod:            "GET",
			AcceptedStatusCodes:   []int{200, 204},
			Interval:              45 * time.Second,
			Timeout:               7 * time.Second,
			Retries:               2,
			RetryBackoff:          3 * time.Second,
			FollowRedirects:       true,
			TLSInsecureSkipVerify: false,
			Proxy:                 "corp",
			ReminderInterval:      time.Hour,
			SlackChannelSlug:      "ops-alerts",
			Mentions:              []templates.MentionDisplay{{Slug: "alice", ID: "U1"}, {Raw: "<!here>"}},
			IsHTTPS:               true,
			SSLAlertThreshold:     14 * 24 * time.Hour,
		},
	})

	resp, body := get(t, srv.Routes(), "/monitor/api")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("monitor detail status: got %d, want 200", resp.StatusCode)
	}
	// Preset surfaces in the visible state section AND inside the dialog.
	if strings.Count(body, "internal-api") < 2 {
		t.Errorf("expected preset %q to appear in both state section and dialog; body:\n%s", "internal-api", firstN(body, 1200))
	}
	// Dialog wiring: the trigger button + the <dialog> + the inline JS
	// for backdrop-click-to-close.
	for _, want := range []string{
		"Show config",
		`id="monitor-config-dialog"`,
		"showModal()",
		"Effective configuration",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing dialog wiring %q", want)
		}
	}
	// Config values live inside the pre-rendered dialog (hidden until
	// the operator clicks the button).
	for _, want := range []string{
		"GET",
		">200<",
		">204<",
		"45s",
		"7s",
		"3s",
		"corp",
		"ops-alerts",
		"alice",
		">U1<",
		"&lt;!here&gt;",
		"1h",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog body missing %q", want)
		}
	}
}

func TestDiscoveryListing_namespaceAndStatusFilter(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	reason := func(s string) *string { return &s }
	for _, row := range []store.DiscoverySnapshotRow{
		{Namespace: "prod", IngressName: "api", Host: "api.example.com", Status: "added", Reason: reason("added")},
		{Namespace: "test-1", IngressName: "api", Host: "t1.example.com", Status: "kube-ignored", Reason: reason("ignored (via match[0]: namespace=test-*)")},
		{Namespace: "test-2", IngressName: "api", Host: "t2.example.com", Status: "kube-ignored", Reason: reason("ignored (via match[0]: namespace=test-*)")},
		{Namespace: "review", IngressName: "api", Host: "rev.example.com", Status: "kube-invalid", Reason: reason("no preset annotation")},
	} {
		if err := repo.UpsertDiscoverySnapshot(ctx, row); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Glob filter: only the test-* rows survive.
	_, body := get(t, srv.Routes(), "/discovery?ns=test-*")
	if !strings.Contains(body, "t1.example.com") || !strings.Contains(body, "t2.example.com") {
		t.Errorf("ns=test-* should keep both test-* rows; body:\n%s", firstN(body, 800))
	}
	if strings.Contains(body, "api.example.com") || strings.Contains(body, "rev.example.com") {
		t.Errorf("ns=test-* should drop non-matching rows; body:\n%s", firstN(body, 800))
	}

	// Status filter narrows further.
	_, body = get(t, srv.Routes(), "/discovery?status=kube-ignored")
	if strings.Contains(body, "api.example.com") || strings.Contains(body, "rev.example.com") {
		t.Errorf("status=kube-ignored should drop added/invalid rows; body:\n%s", firstN(body, 800))
	}
	if !strings.Contains(body, "t1.example.com") || !strings.Contains(body, "t2.example.com") {
		t.Errorf("status=kube-ignored should keep ignored rows; body:\n%s", firstN(body, 800))
	}

	// Empty-result state with an active filter offers a clear-filters link.
	_, body = get(t, srv.Routes(), "/discovery?ns=does-not-exist")
	if !strings.Contains(body, "Clear filters") {
		t.Errorf("empty result with active filter should offer 'Clear filters'; body:\n%s", firstN(body, 800))
	}
}

func TestGroupsIndex_listsGroupsWithCounts(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	for _, s := range []store.MonitorSpec{
		{Slug: "api", FriendlyName: "API", URL: "http://api", GroupSlug: "prod", Source: store.SourceStatic},
		{Slug: "web", FriendlyName: "Web", URL: "http://web", GroupSlug: "prod", Source: store.SourceStatic},
		{Slug: "staging-api", FriendlyName: "Staging API", URL: "http://stg", GroupSlug: "staging", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}

	resp, body := get(t, srv.Routes(), "/groups")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/groups status: got %d, want 200", resp.StatusCode)
	}
	// Both group slugs render as clickable rows.
	for _, want := range []string{">prod<", ">staging<", "/group/prod", "/group/staging"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; first 400:\n%s", want, firstN(body, 400))
		}
	}
}

func TestGroupPage_rendersStatsHeader(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	for _, s := range []store.MonitorSpec{
		{Slug: "api", FriendlyName: "API", URL: "http://api", GroupSlug: "prod", Source: store.SourceStatic},
		{Slug: "web", FriendlyName: "Web", URL: "http://web", GroupSlug: "prod", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}

	resp, body := get(t, srv.Routes(), "/group/prod")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/group/prod status: got %d, want 200", resp.StatusCode)
	}
	// Stats tiles render with pre-filtered /monitors hrefs scoped to this group.
	for _, want := range []string{
		"/monitors?group=prod&amp;status=up",
		"/monitors?group=prod&amp;status=down",
		"/monitors?group=prod&amp;ssl=ssl-expiring",
		"/monitor/api",
		"/monitor/web",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
}

func TestGroupLink_clickableInMonitorsListing(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://api", GroupSlug: "prod", Source: store.SourceStatic,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_, body := get(t, srv.Routes(), "/monitors")
	if !strings.Contains(body, `href="/group/prod"`) {
		t.Errorf("monitors listing should link the group slug to /group/prod; first 600:\n%s", firstN(body, 600))
	}
}

func TestMonitorDetail_notFound(t *testing.T) {
	srv, _ := newServer(t)
	resp, _ := get(t, srv.Routes(), "/monitor/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestStatic_servesEmbeddedCSS(t *testing.T) {
	srv, _ := newServer(t)
	resp, body := get(t, srv.Routes(), "/static/css/app.css")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("css status: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "tailwindcss") {
		t.Errorf("css body should look like compiled Tailwind output, got first chunk: %q", firstN(body, 120))
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestHomepage_dbUnavailable_returns503WithFallback(t *testing.T) {
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	repo := store.New(pool)
	srv := web.New(repo, nil)

	// Close the pool out from under the server to simulate a DB outage.
	pool.Close()

	resp, body := get(t, srv.Routes(), "/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(body, "Database temporarily unavailable") {
		t.Errorf("body missing fallback message, got: %q", firstN(body, 200))
	}
}
