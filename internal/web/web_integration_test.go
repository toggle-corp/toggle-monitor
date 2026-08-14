//go:build integration

package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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
		{Slug: "api", FriendlyName: "API", URL: "http://api", Tags: []string{"prod"}, Source: store.SourceStatic},
		{Slug: "web", FriendlyName: "Web", URL: "http://web", Tags: []string{"prod"}, Source: store.SourceStatic},
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
		{Slug: "api", FriendlyName: "API", URL: "http://api", Tags: []string{"prod"}, Source: store.SourceStatic},
		{Slug: "web", FriendlyName: "Web", URL: "http://web", Tags: []string{"prod"}, Source: store.SourceStatic},
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

func TestMonitorsListing_tagFilter(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	for _, s := range []store.MonitorSpec{
		{Slug: "a", FriendlyName: "Alpha", URL: "http://a", Tags: []string{"prod", "web"}, Source: store.SourceStatic},
		{Slug: "b", FriendlyName: "Beta", URL: "http://b", Tags: []string{"prod", "api"}, Source: store.SourceStatic},
		{Slug: "c", FriendlyName: "Gamma", URL: "http://c", Tags: []string{"staging", "web"}, Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %s: %v", s.Slug, err)
		}
	}

	// The tag dropdown is rendered with every distinct tag.
	resp, body := get(t, srv.Routes(), "/monitors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/monitors: got %d, want 200", resp.StatusCode)
	}
	for _, tag := range []string{"prod", "web", "api", "staging"} {
		if !strings.Contains(body, `value="`+tag+`"`) {
			t.Errorf("tag dropdown missing option %q; first 1500:\n%s", tag, firstN(body, 1500))
		}
	}

	// ?tag=prod narrows to Alpha + Beta.
	resp, body = get(t, srv.Routes(), "/monitors?tag=prod")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/monitors?tag=prod: got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "/monitor/a") || !strings.Contains(body, "/monitor/b") {
		t.Errorf("?tag=prod should include a and b; first 1500:\n%s", firstN(body, 1500))
	}
	if strings.Contains(body, "/monitor/c") {
		t.Errorf("?tag=prod should exclude c (staging)")
	}
	// The selected chip round-trips as a checked input.
	if !strings.Contains(body, `value="prod" checked`) {
		t.Errorf("?tag=prod should mark the chip checked; first 1500:\n%s", firstN(body, 1500))
	}

	// ?tag=prod&tag=web is AND — only Alpha matches.
	resp, body = get(t, srv.Routes(), "/monitors?tag=prod&tag=web")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/monitors?tag=prod&tag=web: got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "/monitor/a") {
		t.Errorf("?tag=prod&tag=web should include a")
	}
	if strings.Contains(body, "/monitor/b") || strings.Contains(body, "/monitor/c") {
		t.Errorf("?tag=prod&tag=web should exclude b and c (AND semantics)")
	}
}

func TestMonitorDetail_renders(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	if err := repo.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug: "api", FriendlyName: "API", URL: "http://api/health",
		Tags: []string{"prod"}, Source: store.SourceStatic,
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
		Tags: []string{"prod"}, Source: store.SourceKube,
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
		if _, err := repo.UpsertDiscoverySnapshot(ctx, row); err != nil {
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

func TestNav_issueBadgeAndStatusLink(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	// Insert a kube-invalid row → issueCount becomes 1, badge should render.
	reason := "no preset annotation"
	if _, err := repo.UpsertDiscoverySnapshot(ctx, store.DiscoverySnapshotRow{
		Namespace: "prod", IngressName: "api", Host: "api.example.com",
		Status: "kube-invalid", Reason: &reason,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, body := get(t, srv.Routes(), "/")
	if !strings.Contains(body, `href="/status"`) {
		t.Errorf("homepage nav should include the Status link; first 600:\n%s", firstN(body, 600))
	}
	if !strings.Contains(body, "Issues") {
		t.Fatalf("homepage nav should include the Issues link; first 600:\n%s", firstN(body, 600))
	}
	// The badge body for count=1 is just "1" wrapped in the rose chip.
	// The chip class string fingerprint distinguishes it from a stray "1".
	if !strings.Contains(body, "bg-rose-100") || !strings.Contains(body, ">1<") {
		t.Errorf("expected issue-count badge with count=1; first 800:\n%s", firstN(body, 800))
	}
}

func TestNav_noBadgeWhenZero(t *testing.T) {
	srv, _ := newServer(t)
	_, body := get(t, srv.Routes(), "/")
	// No discovery rows + no mapping reader → count is 0 → no badge.
	// The Issues link itself still renders; the chip class shouldn't appear inside the nav anchor.
	if strings.Contains(body, "bg-rose-100") {
		t.Errorf("count=0 should not render any rose chip in the nav; first 600:\n%s", firstN(body, 600))
	}
}

func TestStatusIndex_emptyWhenNoConfig(t *testing.T) {
	srv, _ := newServer(t)
	resp, body := get(t, srv.Routes(), "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status status: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "No status pages configured.") {
		t.Errorf("empty placeholder expected; first 400:\n%s", firstN(body, 400))
	}
}

func TestStatusIndex_listsConfiguredPages(t *testing.T) {
	srv, _ := newServer(t)
	// FriendlyNames deliberately chosen to defeat both ascending and
	// descending alphabetical sorts — Mango/Apple/Zebra in config
	// order is neither sort. If a future change reintroduces an
	// alphabetical sort the position assertions below will fail.
	srv.SetStatusConfigs([]*templates.StatusConfig{
		{Slug: "mango", FriendlyName: "Mango", Sections: []templates.StatusConfigSection{
			{Title: "Mango", Match: templates.StatusMatch{HostRegex: regexp.MustCompile(`.*\.example\.com`)}},
		}},
		{Slug: "apple", FriendlyName: "Apple", Sections: []templates.StatusConfigSection{
			{Title: "Apple", Match: templates.StatusMatch{Tags: []string{"apple"}}},
		}},
		{Slug: "zebra", FriendlyName: "Zebra", Sections: []templates.StatusConfigSection{
			{Title: "Zebra", Match: templates.StatusMatch{Tags: []string{"zebra"}}},
		}},
	})
	resp, body := get(t, srv.Routes(), "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status status: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`href="/status/mango"`,
		`href="/status/apple"`,
		`href="/status/zebra"`,
		"Mango",
		"Apple",
		"Zebra",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
	// Config order: Mango before Apple before Zebra.
	iMango := strings.Index(body, `href="/status/mango"`)
	iApple := strings.Index(body, `href="/status/apple"`)
	iZebra := strings.Index(body, `href="/status/zebra"`)
	if !(iMango < iApple && iApple < iZebra) {
		t.Errorf("status pages not in config order: mango=%d apple=%d zebra=%d", iMango, iApple, iZebra)
	}
}

func TestStatusPage_unknownSlugIs404(t *testing.T) {
	srv, _ := newServer(t)
	srv.SetStatusConfigs([]*templates.StatusConfig{
		{Slug: "public", FriendlyName: "Public", Sections: []templates.StatusConfigSection{
			{Title: "Public", Match: templates.StatusMatch{HostRegex: regexp.MustCompile(`.*\.example\.com`)}},
		}},
	})
	resp, _ := get(t, srv.Routes(), "/status/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown slug should 404; got %d", resp.StatusCode)
	}
}

func TestStatusPage_sectionsAndMatching(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	for _, s := range []store.MonitorSpec{
		{Slug: "api", FriendlyName: "API", URL: "https://api.example.com/health", Source: store.SourceStatic, Tags: []string{"prod", "public"}},
		{Slug: "ui", FriendlyName: "UI", URL: "https://ui.example.com/health", Source: store.SourceStatic, Tags: []string{"prod", "public"}},
		{Slug: "internal", FriendlyName: "Internal", URL: "http://internal/health", Source: store.SourceStatic, Tags: []string{"ops", "internal"}},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}
	srv.SetStatusConfigs([]*templates.StatusConfig{{
		Slug:         "public",
		FriendlyName: "Toggle status",
		Sections: []templates.StatusConfigSection{
			{
				Title: "Public",
				Match: templates.StatusMatch{HostRegex: regexp.MustCompile(`.*\.example\.com`)},
			},
			{
				Title: "Internal tools",
				Match: templates.StatusMatch{Tags: []string{"internal"}},
			},
		},
	}})

	resp, body := get(t, srv.Routes(), "/status/public")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status/public status: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		"Toggle status",
		"Public",
		"Internal tools",
		"API",
		"UI",
		"Internal",
		"Operational", // page-level 3-state badge (kind=up)
		">public<",    // slug chip in the header
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
	// The public-section monitors must NOT appear under Internal tools
	// — verify by checking that "Internal" only renders once.
	if strings.Count(body, ">Internal<") != 1 {
		t.Errorf("expected the Internal monitor to render in exactly one section; got %d occurrences", strings.Count(body, ">Internal<"))
	}

	// New URL column: the 🔗 link points at the application root
	// (scheme+host) so operators can open the app, while the full URL
	// underneath links to the health-check endpoint the monitor probes.
	for _, want := range []string{
		"🔗",
		">api.example.com<",
		`href="https://api.example.com"`,        // app-root link
		`href="https://api.example.com/health"`, // health-check link
		">https://api.example.com/health<",      // visible URL text
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status URL column missing %q; first 1500:\n%s", want, firstN(body, 1500))
		}
	}
}

// fakeMissingParents implements web.MissingParentReader for the
// /issues-page test. Lives here (not in the issues test below) so
// the existing test stays focused on discovery-side issues.
type fakeMissingParents []web.MissingParent

func (f fakeMissingParents) MissingParents() []web.MissingParent { return f }

func TestIssuesPage_missingParentsSection(t *testing.T) {
	srv, _ := newServer(t)
	srv.SetMissingParentReader(fakeMissingParents{
		{Parent: "bastion-proxy", Children: []string{"api", "web"}, LastSeen: time.Now()},
	})
	_, body := get(t, srv.Routes(), "/issues")
	for _, want := range []string{
		"Missing dependsOn parents",
		">bastion-proxy<",
		"api, web",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
	// Nav badge picks up the missing parent in the count.
	if !strings.Contains(body, "bg-rose-100") {
		t.Errorf("nav should show issue chip when missing parents exist; first 600:\n%s", firstN(body, 600))
	}
}

func TestStatusPage_groupRegexMatch(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()
	for _, s := range []store.MonitorSpec{
		{Slug: "tc-api", FriendlyName: "TC API", URL: "https://tc-api/health", Tags: []string{"tc-prod"}, Source: store.SourceStatic},
		{Slug: "tc-web", FriendlyName: "TC Web", URL: "https://tc-web/health", Tags: []string{"tc-stage"}, Source: store.SourceStatic},
		{Slug: "other", FriendlyName: "Other", URL: "https://other/health", Tags: []string{"infra"}, Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}
	srv.SetStatusConfigs([]*templates.StatusConfig{{
		Slug: "tc", FriendlyName: "TC",
		Sections: []templates.StatusConfigSection{
			{
				Title: "Toggle services",
				Match: templates.StatusMatch{Any: []templates.StatusMatch{
					{Tags: []string{"tc-prod"}},
					{Tags: []string{"tc-stage"}},
				}},
			},
		},
	}})
	_, body := get(t, srv.Routes(), "/status/tc")
	for _, want := range []string{"TC API", "TC Web", "Toggle services"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
	// `other` (tags=infra) must NOT land in the tc-* section.
	if strings.Contains(body, "Other") {
		t.Errorf("tag infra should not match tc-prod/tc-stage; body:\n%s", firstN(body, 600))
	}
}

func TestIssuesPage_emptyAndKubeInvalid(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	// Empty state: no mapping issues, no invalid discovery.
	resp, body := get(t, srv.Routes(), "/issues")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/issues status: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "No issues detected.") {
		t.Errorf("empty state should say 'No issues detected.'; first 400:\n%s", firstN(body, 400))
	}

	// Insert a kube-invalid row and verify it surfaces.
	reason := "no preset annotation"
	if _, err := repo.UpsertDiscoverySnapshot(ctx, store.DiscoverySnapshotRow{
		Namespace: "prod", IngressName: "api", Host: "api.example.com",
		Status: "kube-invalid", Reason: &reason,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, body = get(t, srv.Routes(), "/issues")
	for _, want := range []string{
		"Kube-invalid ingresses",
		"prod",
		"api.example.com",
		"no preset annotation",
		"/discovery?status=kube-invalid",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issues body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
}

// ADR-0012: kube-ignored rows are acknowledgements, so they render below
// the fold, stay out of the issue count, and cap at IgnoredPreviewMax so
// a broad ignore rule can't grow the page with the cluster.
func TestIssuesPage_skippedIngressesSectionIsCappedAndUncounted(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	total := templates.IgnoredPreviewMax + 2
	for i := 0; i < total; i++ {
		reason := "kube-ignored: match[0] () — wildcard host not probeable"
		if _, err := repo.UpsertDiscoverySnapshot(ctx, store.DiscoverySnapshotRow{
			Namespace:   "static",
			IngressName: "router-" + strconv.Itoa(i),
			Host:        "*.static.example.test",
			Status:      "kube-ignored",
			Reason:      &reason,
		}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	_, body := get(t, srv.Routes(), "/issues")
	for _, want := range []string{
		"Skipped ingresses (" + strconv.Itoa(total) + ")",
		"/discovery?status=kube-ignored",
		"wildcard host not probeable",
		"and 2 more",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issues body missing %q; first 600:\n%s", want, firstN(body, 600))
		}
	}
	// The preview stops at the cap even though every row is in the DB.
	if n := strings.Count(body, ">router-"); n != templates.IgnoredPreviewMax {
		t.Errorf("preview rows: got %d, want %d (cap)", n, templates.IgnoredPreviewMax)
	}
	// Acknowledged rows are not issues: no count, no nav chip.
	if !strings.Contains(body, "No issues detected.") {
		t.Errorf("ignored rows must not turn into issues; first 600:\n%s", firstN(body, 600))
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

func TestStatusPage_sslTilesSplitExpiringAndExpired(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	now := time.Now()
	for _, s := range []store.MonitorSpec{
		{Slug: "tile-soon", FriendlyName: "Soon", URL: "https://soon/", Source: store.SourceStatic, Tags: []string{"public"}},
		{Slug: "tile-past1", FriendlyName: "P1", URL: "https://p1/", Source: store.SourceStatic, Tags: []string{"public"}},
		{Slug: "tile-past2", FriendlyName: "P2", URL: "https://p2/", Source: store.SourceStatic, Tags: []string{"public"}},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}
	seedSSLState(t, repo, "tile-soon", alert.SSLStatusExpiring, now.Add(5*24*time.Hour))
	seedSSLState(t, repo, "tile-past1", alert.SSLStatusExpiring, now.Add(-1*24*time.Hour))
	seedSSLState(t, repo, "tile-past2", alert.SSLStatusExpiring, now.Add(-5*24*time.Hour))

	srv.SetStatusConfigs([]*templates.StatusConfig{{
		Slug: "pub", FriendlyName: "Pub",
		Sections: []templates.StatusConfigSection{{
			Title: "All", Match: templates.StatusMatch{Tags: []string{"public"}},
		}},
	}})

	resp, body := get(t, srv.Routes(), "/status/pub")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status/pub status: got %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"SSL expiring", "SSL expired"} {
		if !strings.Contains(body, want) {
			t.Errorf("status-page tiles should include %q; first 2000:\n%s", want, firstN(body, 2000))
		}
	}
	// The expired tile must link to ?ssl=expired so an operator can
	// drill in.
	if !strings.Contains(body, "ssl=expired") {
		t.Errorf("expired tile should link to ?ssl=expired; first 2000:\n%s", firstN(body, 2000))
	}
}

func TestMonitorsListing_sslExpiredFilter(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	now := time.Now()
	for _, s := range []store.MonitorSpec{
		{Slug: "soon", FriendlyName: "Soon", URL: "https://soon/", Source: store.SourceStatic},
		{Slug: "past", FriendlyName: "Past", URL: "https://past/", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}
	// Both rows live in the data layer as ssl-expiring; only their
	// expiry timestamps differ. The ?ssl=expired filter must select
	// only the past one.
	seedSSLState(t, repo, "soon", alert.SSLStatusExpiring, now.Add(3*24*time.Hour))
	seedSSLState(t, repo, "past", alert.SSLStatusExpiring, now.Add(-3*24*time.Hour))

	resp, body := get(t, srv.Routes(), "/monitors?ssl=expired")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/monitors?ssl=expired status: got %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, ">Past<") {
		t.Errorf("?ssl=expired should include Past row; first 2000:\n%s", firstN(body, 2000))
	}
	if strings.Contains(body, ">Soon<") {
		t.Errorf("?ssl=expired should exclude Soon row; first 2000:\n%s", firstN(body, 2000))
	}
	// Dropdown must surface the new option so operators can pick it.
	if !strings.Contains(body, `value="expired"`) {
		t.Errorf("/monitors SSL dropdown should expose value=\"expired\"; first 2000:\n%s", firstN(body, 2000))
	}
}

func TestMonitorsListing_sslColumnRendersAllStates(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	now := time.Now()
	for _, s := range []store.MonitorSpec{
		{Slug: "m-ok", FriendlyName: "OK", URL: "https://ok/", Source: store.SourceStatic},
		{Slug: "m-expired", FriendlyName: "Expd", URL: "https://expd/", Source: store.SourceStatic},
		{Slug: "m-skipped", FriendlyName: "Skip", URL: "http://skip/", Source: store.SourceStatic},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}
	seedSSLState(t, repo, "m-ok", alert.SSLStatusOK, now.Add(60*24*time.Hour+12*time.Hour))
	seedSSLState(t, repo, "m-expired", alert.SSLStatusExpiring, now.Add(-2*24*time.Hour-12*time.Hour))
	seedSSLState(t, repo, "m-skipped", alert.SSLStatusSkipped, time.Time{})

	resp, body := get(t, srv.Routes(), "/monitors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/monitors status: got %d, want 200", resp.StatusCode)
	}
	for _, label := range []string{">ok<", ">expired<", ">skipped<"} {
		if !strings.Contains(body, label) {
			t.Errorf("/monitors body missing SSL chip label %q; first 2000:\n%s", label, firstN(body, 2000))
		}
	}
	if !strings.Contains(body, "bg-rose-100") {
		t.Errorf("/monitors expired row should use rose chip classes; first 2000:\n%s", firstN(body, 2000))
	}
}

// seedSSLState writes an SSL state row for an existing monitor. Test
// helper used by SSL-display tests. Pass a zero expiresAt to leave the
// expiry column NULL (used for the skipped case).
func seedSSLState(t *testing.T, repo *store.Repo, slug string, status alert.SSLStatus, expiresAt time.Time) {
	t.Helper()
	state := alert.SSLState{Status: status}
	if status == alert.SSLStatusExpiring {
		state.OpenedAt = time.Now()
	}
	if err := repo.ApplySSLCheck(context.Background(), slug, state, expiresAt, "Test Issuer", "test-subject", nil); err != nil {
		t.Fatalf("ApplySSLCheck(%s): %v", slug, err)
	}
}

func TestStatusPage_sslColumnRendersAllStates(t *testing.T) {
	srv, repo := newServer(t)
	ctx := context.Background()

	now := time.Now()
	for _, s := range []store.MonitorSpec{
		{Slug: "cert-ok", FriendlyName: "OK Cert", URL: "https://ok.example.com/health", Source: store.SourceStatic, Tags: []string{"public"}},
		{Slug: "cert-expiring", FriendlyName: "Expiring Cert", URL: "https://expiring.example.com/health", Source: store.SourceStatic, Tags: []string{"public"}},
		{Slug: "cert-expired", FriendlyName: "Expired Cert", URL: "https://expired.example.com/health", Source: store.SourceStatic, Tags: []string{"public"}},
		{Slug: "cert-skipped", FriendlyName: "Skipped", URL: "http://plain.example.com/health", Source: store.SourceStatic, Tags: []string{"public"}},
		{Slug: "cert-nil", FriendlyName: "Never Checked", URL: "https://never.example.com/health", Source: store.SourceStatic, Tags: []string{"public"}},
	} {
		if err := repo.ReconcileMonitor(ctx, s); err != nil {
			t.Fatalf("reconcile %q: %v", s.Slug, err)
		}
	}
	// Add a half-day buffer past the integer-day boundary so a few
	// seconds of test runtime don't flip humanDuration's day bucket
	// down (3d → 2d, 2d → 1d).
	seedSSLState(t, repo, "cert-ok", alert.SSLStatusOK, now.Add(60*24*time.Hour+12*time.Hour))
	seedSSLState(t, repo, "cert-expiring", alert.SSLStatusExpiring, now.Add(3*24*time.Hour+12*time.Hour))
	seedSSLState(t, repo, "cert-expired", alert.SSLStatusExpiring, now.Add(-2*24*time.Hour-12*time.Hour))
	seedSSLState(t, repo, "cert-skipped", alert.SSLStatusSkipped, time.Time{})
	// cert-nil: deliberately no ApplySSLCheck call.

	srv.SetStatusConfigs([]*templates.StatusConfig{{
		Slug: "public", FriendlyName: "Public status",
		Sections: []templates.StatusConfigSection{{
			Title: "All",
			Match: templates.StatusMatch{Tags: []string{"public"}},
		}},
	}})

	resp, body := get(t, srv.Routes(), "/status/public")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status/public status: got %d, want 200", resp.StatusCode)
	}

	// Each chip label must appear at least once in the body.
	for _, label := range []string{">ok<", ">expiring<", ">expired<", ">skipped<"} {
		if !strings.Contains(body, label) {
			t.Errorf("body missing SSL chip label %q; first 2000:\n%s", label, firstN(body, 2000))
		}
	}

	// The expired chip must use the red/rose palette (not amber), per the
	// presentation-only override.
	if !strings.Contains(body, "bg-rose-100") {
		t.Errorf("expired cert should render with rose/red chip classes; first 2000:\n%s", firstN(body, 2000))
	}

	// Relative dates next to expiring (future) and expired (past) certs.
	if !strings.Contains(body, "in 3d") {
		t.Errorf("expiring cell should show relative 'in 3d'; first 2000:\n%s", firstN(body, 2000))
	}
	if !strings.Contains(body, "2d ago") {
		t.Errorf("expired cell should show relative '2d ago'; first 2000:\n%s", firstN(body, 2000))
	}

	// RFC3339 timestamp must appear as a tooltip on the OK row.
	okStamp := now.Add(60*24*time.Hour + 12*time.Hour).UTC().Format(time.RFC3339)
	// Match prefix (truncated to hour precision) so sub-second drift is OK.
	okStampPrefix := okStamp[:13]
	if !strings.Contains(body, `title="`+okStampPrefix) {
		t.Errorf("ok cell should carry RFC3339 in title attr (prefix %q); first 2000:\n%s", okStampPrefix, firstN(body, 2000))
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

// fakeAnnotationWarnings is a stand-in for the merger's per-monitor
// record of rejected annotation values (ADR-0009).
type fakeAnnotationWarnings []web.AnnotationIssue

func (f fakeAnnotationWarnings) AnnotationIssues() []web.AnnotationIssue { return f }

// An annotation a chart got wrong is a misconfiguration the operator
// owns, and the monitor keeps probing meanwhile — so it belongs on
// /issues rather than in a log line nobody reads.
func TestIssuesPage_annotationWarningsSection(t *testing.T) {
	srv, _ := newServer(t)
	srv.SetAnnotationIssueReader(fakeAnnotationWarnings{{
		Slug:        "kube-acme-api-1__api__api-example-test",
		Namespace:   "acme-api-1",
		IngressName: "api",
		Host:        "api.example.test",
		Field:       "notify",
		Key:         "app.example.test/notify",
		Scope:       "namespaceAnnotation",
		Value:       "zed",
		Reason:      "is not a slack.userMapping slug",
	}})
	_, body := get(t, srv.Routes(), "/issues")
	for _, want := range []string{
		"Rejected annotation values",
		"app.example.test/notify",
		">zed<",
		"is not a slack.userMapping slug",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; first 900:\n%s", want, firstN(body, 900))
		}
	}
	if !strings.Contains(body, "bg-rose-100") {
		t.Errorf("nav should show the issue chip when annotations were rejected; first 600:\n%s", firstN(body, 600))
	}
}
