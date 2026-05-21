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
