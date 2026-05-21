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
	"strings"
	"sync/atomic"

	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// StaticAssets holds the embedded static asset tree (CSS and any other
// small static files). Tailwind compiles into static/css/app.css.
//
//go:embed all:static
var StaticAssets embed.FS

// Server wires the HTTP surface for the read-only UI plus the
// k8s probe endpoints.
type Server struct {
	repo    *store.Repo
	log     *slog.Logger
	metrics http.Handler // /metrics handler; nil → endpoint is omitted
	ready   atomic.Bool
}

// New constructs a Server. Call MarkReady once the DB is connected and
// the config has loaded so /readyz starts returning 200.
func New(repo *store.Repo, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{repo: repo, log: log}
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
	mux.HandleFunc("GET /monitor/{slug}", s.handleMonitorDetail)

	return mux
}

func (s *Server) handleHomepage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.repo.HomepageStats(ctx)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	monitors, err := s.repo.ListActiveMonitors(ctx)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.Homepage(stats, monitors).Render(ctx, w)
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

