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
	"path"
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
	repo            *store.Repo
	log             *slog.Logger
	metrics         http.Handler // /metrics handler; nil → endpoint is omitted
	pageSizes       PageSizes
	knownGroups     []string
	mapping         MappingHealthReader
	configLookup    ConfigLookup
	discoveryStatus templates.DiscoveryStatus
	ready           atomic.Bool
}

// ConfigLookup returns the effective runtime config for a monitor (the
// fields that aren't carried on store.MonitorRow — interval, timeout,
// retries, mentions, etc.). Production wires the live scheduler plan
// source so static + kube monitors render identically; tests can
// supply a fake or omit it entirely (the detail page falls back to
// just the store-side spec).
type ConfigLookup interface {
	ConfigFor(slug string) (templates.MonitorConfig, bool)
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

// SetDiscoveryStatus tells the /discovery page whether kube
// auto-discovery is enabled (and at what cadence) so the empty state
// can explain why no rows are showing.
func (s *Server) SetDiscoveryStatus(d templates.DiscoveryStatus) { s.discoveryStatus = d }

// SetConfigLookup plugs in the runtime-config view (the live
// scheduler.Plan, shaped for the UI). When nil, the monitor detail
// page renders the store-side spec only.
func (s *Server) SetConfigLookup(c ConfigLookup) { s.configLookup = c }

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
		Search:   r.URL.Query().Get("q"),
		Status:   r.URL.Query().Get("status"),
		SSL:      r.URL.Query().Get("ssl"),
		Group:    r.URL.Query().Get("group"),
		Sort:     normalizeSortKey(r.URL.Query().Get("sort")),
		SortDesc: r.URL.Query().Get("dir") == "desc",
	}
	listing, err := s.repo.ListMonitors(ctx, store.ListMonitorsOpts{
		Search:          filter.Search,
		Status:          filter.Status,
		SSL:             filter.SSL,
		GroupSlug:       filter.Group,
		IncludeArchived: includeArchived,
		Sort:            filter.Sort,
		SortDesc:        filter.SortDesc,
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
	filter := templates.DiscoveryFilter{
		Namespace: strings.TrimSpace(r.URL.Query().Get("ns")),
		Status:    strings.TrimSpace(r.URL.Query().Get("status")),
	}
	rows = filterDiscoveryRows(rows, filter)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.DiscoveryListing(rows, filter, s.discoveryStatus).Render(ctx, w)
}

// filterDiscoveryRows keeps the snapshot rows the operator asked for.
// Namespace supports the same path.Match-style globs as
// kube.match[].when.namespace; empty values mean "any".
func filterDiscoveryRows(rows []store.DiscoverySnapshotRow, f templates.DiscoveryFilter) []store.DiscoverySnapshotRow {
	if f.Namespace == "" && f.Status == "" {
		return rows
	}
	out := rows[:0:0]
	for _, r := range rows {
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		if f.Namespace != "" && !matchNamespaceGlob(f.Namespace, r.Namespace) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// matchNamespaceGlob mirrors the merger.matchGlob semantics so the
// /discovery filter behaves identically to the kube.match[] rules:
// `*` matches any run of non-`/` characters via path.Match.
func matchNamespaceGlob(pattern, value string) bool {
	if pattern == value {
		return true
	}
	ok, err := path.Match(pattern, value)
	if err != nil {
		return false
	}
	return ok
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
	if limit := s.pageSizes.MaxPerPage; limit > 0 && perPage > limit {
		perPage = limit
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
	var cfg *templates.MonitorConfig
	if s.configLookup != nil {
		if c, ok := s.configLookup.ConfigFor(slug); ok {
			cfg = &c
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.MonitorDetail(m, cfg, gatingParents, history).Render(ctx, w)
}

// renderDBUnavailable serves a 503 with the friendly fallback page.
func (s *Server) renderDBUnavailable(ctx context.Context, w http.ResponseWriter, err error) {
	s.log.Warn("db unavailable during UI request", "error", err)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = templates.DBUnavailable().Render(ctx, w)
}

// normalizeSortKey filters the `?sort=` query parameter against the
// store's whitelist. Unknown keys collapse to "" so the listing falls
// back to the default order rather than 500-ing or running an
// attacker-supplied ORDER BY.
func normalizeSortKey(raw string) string {
	if raw == "" {
		return ""
	}
	for _, k := range store.ListSortKeys {
		if raw == k {
			return raw
		}
	}
	return ""
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
