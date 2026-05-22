// Package web serves the read-only UI, k8s probe endpoints, and the
// Prometheus metrics endpoint. Templates (templ-generated Go), static
// assets, and Tailwind output CSS are embedded via embed.FS.
package web

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// StaticAssets holds the embedded static asset tree (CSS and any other
// small static files). Tailwind compiles into static/css/app.css.
//
//go:embed all:static
var StaticAssets embed.FS

// PageSizes carries the per-listing default + the per_page cap.
// Production wires these from config.UI; tests pass small defaults.
type PageSizes struct {
	HomepageAlerts   int
	MonitorListing   int
	MonitorHistory   int
	DiscoveryListing int
	MaxPerPage       int
}

// DefaultPageSizes is used when a Server is constructed without an
// explicit PageSizes (tests, mostly). Mirrors the schema's
// suggested values.
var DefaultPageSizes = PageSizes{
	HomepageAlerts:   20,
	MonitorListing:   50,
	MonitorHistory:   50,
	DiscoveryListing: 50,
	MaxPerPage:       200,
}

// Server wires the HTTP surface for the read-only UI plus the
// k8s probe endpoints.
type Server struct {
	repo        *store.Repo
	log         *slog.Logger
	metrics     http.Handler // /metrics handler; nil → endpoint is omitted
	pageSizes   PageSizes
	knownGroups []string
	mapping     MappingHealthReader
	ready       atomic.Bool
}

// MappingHealthReader exposes the userMapping validator's current
// snapshot to the UI. Production wires slack.UserMappingValidator;
// tests can pass nil or a fake.
type MappingHealthReader interface {
	Snapshot() (entries []MappingEntry, lastRun time.Time)
}

// MappingEntry is the slim shape the homepage panel renders. Mirrors
// slack.MappingEntryState; web doesn't import slack to keep the
// dependency arrow pointing the right way.
type MappingEntry struct {
	Slug    string
	ID      string
	OK      bool
	Reason  string
	Checked time.Time
}

// SetMappingReader plugs in the userMapping validator. When nil
// (tests), the homepage panel is suppressed.
func (s *Server) SetMappingReader(r MappingHealthReader) { s.mapping = r }

// SetPageSizes overrides the per-listing default page sizes (called
// by lifecycle after the config is loaded).
func (s *Server) SetPageSizes(ps PageSizes) { s.pageSizes = ps }

// SetKnownGroups wires the slugs that appear in the filter dropdown
// on /monitors. Called by lifecycle after config load.
func (s *Server) SetKnownGroups(g []string) { s.knownGroups = g }

// New constructs a Server. Call MarkReady once the DB is connected and
// the config has loaded so /readyz starts returning 200.
func New(repo *store.Repo, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{repo: repo, log: log, pageSizes: DefaultPageSizes}
}

// SetMetricsHandler wires the Prometheus exposition handler. When set,
// /metrics is exposed; when nil (the default in tests that don't care
// about metrics), the endpoint is omitted.
func (s *Server) SetMetricsHandler(h http.Handler) { s.metrics = h }

// MarkReady flips /readyz from 503 to 200. Idempotent.
func (s *Server) MarkReady() { s.ready.Store(true) }

// Routes returns the http.Handler exposing the documented routes.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	staticFS, _ := fs.Sub(StaticAssets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}

	mux.HandleFunc("GET /{$}", s.handleHomepage)
	mux.HandleFunc("GET /monitors", s.handleMonitorsListing)
	mux.HandleFunc("GET /monitor/{slug}", s.handleMonitorDetail)
	mux.HandleFunc("GET /group/{slug}", s.handleGroupPage)
	mux.HandleFunc("GET /discovery", s.handleDiscoveryListing)
	mux.HandleFunc("GET /discovery/{ns}/{name}/{host}", s.handleDiscoveryDetail)

	return mux
}

func (s *Server) handleHomepage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.repo.HomepageStats(ctx)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	page, perPage := s.pagination(r, s.pageSizes.HomepageAlerts)
	alerts, err := s.repo.ListLatestAlerts(ctx, perPage, (page-1)*perPage)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	var mapping templates.MappingHealth
	if s.mapping != nil {
		entries, lastRun := s.mapping.Snapshot()
		mapping.LastRun = lastRun
		for _, e := range entries {
			if !e.OK {
				mapping.Invalid = append(mapping.Invalid, templates.MappingEntry{
					Slug: e.Slug, ID: e.ID, Reason: e.Reason, Checked: e.Checked,
				})
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.Homepage(stats, alerts, page, perPage, mapping).Render(ctx, w)
}

func (s *Server) handleMonitorsListing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, perPage := s.pagination(r, s.pageSizes.MonitorListing)
	includeArchived := r.URL.Query().Get("archived") == "true"
	filter := templates.MonitorsFilter{
		Search: r.URL.Query().Get("q"),
		Status: r.URL.Query().Get("status"),
		Group:  r.URL.Query().Get("group"),
	}
	listing, err := s.repo.ListMonitors(ctx, store.ListMonitorsOpts{
		Search:          filter.Search,
		Status:          filter.Status,
		GroupSlug:       filter.Group,
		IncludeArchived: includeArchived,
		Limit:           perPage,
		Offset:          (page - 1) * perPage,
	})
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.MonitorsPage(listing, filter, nil, s.knownGroups, page, perPage).Render(ctx, w)
}

func (s *Server) handleGroupPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	if !validSlugForURL(slug) {
		http.NotFound(w, r)
		return
	}
	page, perPage := s.pagination(r, s.pageSizes.MonitorListing)
	listing, err := s.repo.ListMonitors(ctx, store.ListMonitorsOpts{
		GroupSlug: slug,
		Limit:     perPage,
		Offset:    (page - 1) * perPage,
	})
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.GroupPage(slug, listing, page, perPage).Render(ctx, w)
}

func (s *Server) handleDiscoveryListing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.repo.ListDiscoverySnapshot(ctx)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.DiscoveryListing(rows).Render(ctx, w)
}

func (s *Server) handleDiscoveryDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	host := r.PathValue("host")
	rows, err := s.repo.ListDiscoverySnapshot(ctx)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	for _, row := range rows {
		if row.Namespace == ns && row.IngressName == name && row.Host == host {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = templates.DiscoveryDetail(row).Render(ctx, w)
			return
		}
	}
	http.NotFound(w, r)
}

// pagination resolves the requested page + per-page from the URL,
// clamping per_page to [1, MaxPerPage].
func (s *Server) pagination(r *http.Request, defPer int) (page, perPage int) {
	page = 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	perPage = defPer
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	if max := s.pageSizes.MaxPerPage; max > 0 && perPage > max {
		perPage = max
	}
	return page, perPage
}

func (s *Server) handleMonitorDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	if !validSlugForURL(slug) {
		http.NotFound(w, r)
		return
	}

	m, err := s.repo.GetMonitor(ctx, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.renderDBUnavailable(ctx, w, err)
		return
	}

	// When the monitor is temporary-paused, surface which parent(s)
	// are currently keeping it gated.
	var gatingParents []string
	if m.Status == "temporary-paused" && len(m.DependsOn) > 0 {
		for _, dep := range m.DependsOn {
			p, perr := s.repo.GetMonitor(ctx, dep)
			if perr == nil && p.Status == "down" {
				gatingParents = append(gatingParents, dep)
			}
		}
	}

	history, err := s.repo.ListAlertsForMonitor(ctx, slug, 50)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.MonitorDetail(m, gatingParents, history).Render(ctx, w)
}

// renderDBUnavailable serves a 503 with the friendly fallback page.
func (s *Server) renderDBUnavailable(ctx context.Context, w http.ResponseWriter, err error) {
	s.log.Warn("db unavailable during UI request", "error", err)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = templates.DBUnavailable().Render(ctx, w)
}

// validSlugForURL is a defensive sanity check on the slug arriving via
// the URL path. It's not the canonical validator (slug.Validate has
// that role), but it cheaply rejects obviously malformed URLs without
// touching the DB.
func validSlugForURL(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	// No directory traversal or other path-like characters.
	if strings.ContainsAny(s, "/\\?#") {
		return false
	}
	return true
}

