package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// amListingDefaultPerPage caps the listing's default per-page size; the
// handler honors ?per_page= up to PageSizes.MaxPerPage as everywhere
// else in the UI.
const amListingDefaultPerPage = 50

// handleAMAlertsListing renders /alerts — filterable, paginated list of
// AM incidents. Filter parsing mirrors the store-side AMListFilter
// vocabulary; unknown values silently collapse to the all-rows default.
func (s *Server) handleAMAlertsListing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	page, perPage := s.pagination(r, amListingDefaultPerPage)
	rawFilter := templates.AMListingFilter{
		Status:    normalizeAMStatus(q.Get("status")),
		Severity:  strings.TrimSpace(q.Get("severity")),
		Alertname: strings.TrimSpace(q.Get("alertname")),
		Channel:   strings.TrimSpace(q.Get("channel")),
		Receiver:  strings.TrimSpace(q.Get("receiver")),
		From:      strings.TrimSpace(q.Get("from")),
		To:        strings.TrimSpace(q.Get("to")),
	}
	from, to := parseAMTimeRange(rawFilter.From, rawFilter.To)

	// Ask for perPage+1 rows so we can tell the operator whether the
	// next page would carry anything without paying for a COUNT(*) on
	// every render.
	storeFilter := store.AMListFilter{
		Status:      rawFilter.Status,
		Severity:    rawFilter.Severity,
		Alertname:   rawFilter.Alertname,
		ChannelSlug: rawFilter.Channel,
		Receiver:    rawFilter.Receiver,
		From:        from,
		To:          to,
		Limit:       perPage + 1,
		Offset:      (page - 1) * perPage,
	}
	rows, err := s.repo.ListAMIncidents(ctx, storeFilter)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}

	hasNext := len(rows) > perPage
	if hasNext {
		rows = rows[:perPage]
	}

	view := templates.AMListingView{
		Filter:  rawFilter,
		Page:    page,
		PerPage: perPage,
		HasNext: hasNext,
	}
	for _, inc := range rows {
		view.Incidents = append(view.Incidents, templates.ToAMIncidentRow(inc))
	}

	// Empty distinguishes "no AM alerts have ever fired" from "filters
	// narrowed to zero". A separate any-row probe keeps the empty-state
	// honest under filters; we only run it when the listing query
	// returned nothing and the user hasn't supplied filters (the cheap
	// case is also the common one — DB empty == fast).
	if len(view.Incidents) == 0 && rawFilter.IsEmpty() && page == 1 {
		view.Empty = true
	} else if len(view.Incidents) == 0 {
		// Filters present and empty — see if the unfiltered table has
		// any row at all. When it doesn't, render the soft empty state
		// so an operator who filtered prematurely doesn't get told off.
		any, err := s.repo.ListAMIncidents(ctx, store.AMListFilter{Limit: 1})
		if err != nil {
			s.renderDBUnavailable(ctx, w, err)
			return
		}
		if len(any) == 0 {
			view.Empty = true
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.AMAlertsListing(view).Render(ctx, w)
}

// handleAMAlertDetail renders /alert/{id}. Unknown / non-numeric id =>
// 404 (no DB hit). DB outage => the shared 503 fallback the rest of the
// UI uses.
func (s *Server) handleAMAlertDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	inc, err := s.repo.GetAMIncident(ctx, id)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	if inc == nil {
		http.NotFound(w, r)
		return
	}

	historyRows, err := s.repo.ListAMIncidentsByFingerprint(ctx, inc.Fingerprint, 10)
	if err != nil {
		s.renderDBUnavailable(ctx, w, err)
		return
	}
	history := make([]templates.AMIncidentRow, 0, len(historyRows))
	for _, h := range historyRows {
		if h.ID == inc.ID {
			continue
		}
		history = append(history, templates.ToAMIncidentRow(h))
	}

	rawPayload, err := s.repo.GetLatestAMEventPayload(ctx, inc.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.log.Warn("get latest am event payload", "incident_id", inc.ID, "error", err)
		// Soft-fail: render the page without the raw payload rather
		// than 5xx-ing on a cosmetic detail.
		rawPayload = nil
	}

	view := templates.AMDetailView{
		Incident:   templates.ToAMIncidentDetail(*inc, time.Now()),
		History:    history,
		RawPayload: prettyJSON(rawPayload),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = templates.AMAlertDetail(view).Render(ctx, w)
}

// normalizeAMStatus filters the ?status= query parameter against the
// store's whitelist. Unknown values become "" so the all-rows default
// fires (rather than a 4xx for a typo).
func normalizeAMStatus(raw string) string {
	switch raw {
	case "firing", "resolved":
		return raw
	default:
		return ""
	}
}

// parseAMTimeRange accepts the two formats the datetime-local input
// emits ("2006-01-02T15:04" and the RFC3339 variant a paste can produce)
// and returns optional *time.Time pointers suitable for AMListFilter.
// Unparseable values are silently dropped — the filter form's whole
// vibe is "type something approximate, get something useful back."
func parseAMTimeRange(fromRaw, toRaw string) (*time.Time, *time.Time) {
	parse := func(s string) *time.Time {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		// datetime-local input value: "2006-01-02T15:04".
		if t, err := time.Parse("2006-01-02T15:04", s); err == nil {
			return &t
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return &t
		}
		return nil
	}
	return parse(fromRaw), parse(toRaw)
}

// prettyJSON re-indents a JSONB payload for the detail page's <pre>.
// Invalid JSON falls back to the verbatim bytes so an operator looking
// at a broken payload still sees what arrived; nil returns "".
func prettyJSON(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, payload, "", "  "); err == nil {
		return buf.String()
	}
	return string(payload)
}

