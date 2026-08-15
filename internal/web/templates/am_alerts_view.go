package templates

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// KV is a sorted-string pair used by the AM detail page to render
// labels / annotations / key-label-chips on the listing. Kept here
// (rather than reusing a config-side struct) so the templates don't
// reach into other packages for what is, structurally, a key/value
// row.
type KV struct {
	Key, Value string
}

// AMListingView is the data the listing handler hands to the
// AMAlertsListing template. The handler builds this from one
// store.ListAMIncidents call plus the parsed query string; the
// template renders it verbatim, no extra DB work.
type AMListingView struct {
	Incidents []AMIncidentRow
	Filter    AMListingFilter

	// Pagination knobs — the listing page is purely cursor-by-page;
	// HasNext is the cheap signal we use ("we asked for perPage+1 rows
	// and got more than perPage back") so we can paint a next-page
	// link without a separate COUNT(*) query.
	Page    int
	PerPage int
	HasNext bool

	// Empty distinguishes "no AM alerts have ever fired" (database
	// empty, render the soft onboarding empty state) from
	// "filters narrowed to zero" (render the clear-filters card).
	Empty bool
}

// AMIncidentRow is the listing-row projection of one store.AMIncident.
// All formatting decisions (which labels to chip, how to format
// "duration", what to label status as) are made by the handler, so
// the template stays oblivious to time.Time math and label key
// ordering.
type AMIncidentRow struct {
	ID           int64
	Fingerprint  string
	Alertname    string
	Severity     string
	Status       string // "firing" | "resolved"
	StartedAt    time.Time
	EndedAt      *time.Time
	ChannelSlug  string
	Receiver     string
	ExternalURL  string
	KeyLabels    []KV
	SlackChannel string
	SlackTS      string
}

// AMListingFilter mirrors the listing query string so the form can
// repopulate cleanly across submits. All fields are passthroughs;
// no normalization here — that's the handler's job.
type AMListingFilter struct {
	Status    string
	Severity  string
	Alertname string
	Channel   string
	Receiver  string
	From      string
	To        string
}

// AMDetailView is the data the detail handler hands to AMAlertDetail.
// The detail page is read-only — every field is computed once per
// request and rendered verbatim.
type AMDetailView struct {
	Incident   AMIncidentDetail
	History    []AMIncidentRow
	RawPayload string
}

// AMIncidentDetail is the focused row plus the slow-to-format fields
// (sorted labels/annotations, slack permalink, resolved notify list).
type AMIncidentDetail struct {
	AMIncidentRow

	Labels         []KV
	Annotations    []KV
	RuleChain      string
	ResolvedNotify []string
	SlackPermalink string

	// Duration is the time between StartedAt and EndedAt (for resolved
	// incidents) or between StartedAt and now (for firing). Stored on
	// the view so the template doesn't need a now-anchored helper.
	Duration time.Duration
}

// keyLabelOrder is the priority list for picking which labels render
// as chips on the listing row. The detail page renders every label,
// but the listing has to triage; we pick at most the first three
// labels (in this priority order) that the incident actually carries.
var keyLabelOrder = []string{"namespace", "instance", "service", "job", "cluster", "pod"}

// ToAMIncidentRow turns one store.AMIncident into its listing-row
// projection — picks the severity label, formats the status, lifts
// the key labels into chips.
func ToAMIncidentRow(inc store.AMIncident) AMIncidentRow {
	row := AMIncidentRow{
		ID:           inc.ID,
		Fingerprint:  inc.Fingerprint,
		Alertname:    inc.Alertname,
		Severity:     inc.Labels["severity"],
		StartedAt:    inc.StartedAt,
		ChannelSlug:  inc.ChannelSlug,
		Receiver:     inc.Receiver,
		ExternalURL:  inc.ExternalURL,
		SlackChannel: inc.SlackChannel,
		SlackTS:      inc.SlackTS,
	}
	if inc.EndedAt.Valid {
		row.Status = "resolved"
		t := inc.EndedAt.Time
		row.EndedAt = &t
	} else {
		row.Status = "firing"
	}
	for _, k := range keyLabelOrder {
		if v, ok := inc.Labels[k]; ok && v != "" {
			row.KeyLabels = append(row.KeyLabels, KV{Key: k, Value: v})
			if len(row.KeyLabels) >= 3 {
				break
			}
		}
	}
	return row
}

// ToAMIncidentDetail builds the detail-page projection. Labels and
// annotations are sorted by key so the rendered list is stable
// across page reloads (Go map iteration would otherwise jitter the
// order).
func ToAMIncidentDetail(inc store.AMIncident, now time.Time) AMIncidentDetail {
	d := AMIncidentDetail{
		AMIncidentRow:  ToAMIncidentRow(inc),
		Labels:         sortedKV(inc.Labels),
		Annotations:    sortedKV(inc.Annotations),
		RuleChain:      inc.RuleChain,
		ResolvedNotify: inc.ResolvedNotify,
	}
	if inc.EndedAt.Valid {
		d.Duration = inc.EndedAt.Time.Sub(inc.StartedAt)
	} else {
		d.Duration = now.Sub(inc.StartedAt)
	}
	if d.Duration < 0 {
		d.Duration = 0
	}
	return d
}

// sortedKV converts a map into a key-sorted []KV slice.
func sortedKV(m map[string]string) []KV {
	if len(m) == 0 {
		return nil
	}
	out := make([]KV, 0, len(m))
	for k, v := range m {
		out = append(out, KV{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// AnnotationByKey returns the value of a specific annotation key (or
// "") — the detail template uses this for the summary / description /
// runbook fields it carves out at the top before listing the rest.
func (d AMIncidentDetail) AnnotationByKey(key string) string {
	for _, kv := range d.Annotations {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// ExternalHost extracts the bare host from the AM externalURL — the
// detail page renders it as "Via: am.prod.example.test" to keep
// the noisy scheme/path off the chrome.
func (r AMIncidentRow) ExternalHost() string {
	if r.ExternalURL == "" {
		return ""
	}
	if u, err := url.Parse(r.ExternalURL); err == nil && u.Host != "" {
		return u.Host
	}
	return r.ExternalURL
}

// AMListingURL builds /alerts?<query> with the supplied filter,
// preserving per_page so pagination links round-trip cleanly. Empty
// fields are omitted entirely so the URL stays clean for plain
// link-clicks (no "?status=&severity=…" trail).
func AMListingURL(f AMListingFilter, page, perPage int) string {
	v := url.Values{}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if f.Severity != "" {
		v.Set("severity", f.Severity)
	}
	if f.Alertname != "" {
		v.Set("alertname", f.Alertname)
	}
	if f.Channel != "" {
		v.Set("channel", f.Channel)
	}
	if f.Receiver != "" {
		v.Set("receiver", f.Receiver)
	}
	if f.From != "" {
		v.Set("from", f.From)
	}
	if f.To != "" {
		v.Set("to", f.To)
	}
	if perPage > 0 {
		v.Set("per_page", strconv.Itoa(perPage))
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return "/alerts"
	}
	return "/alerts?" + v.Encode()
}

// FilterIsEmpty reports whether every filter field is blank — drives
// the "no alerts have ever fired" vs. "filters narrowed to zero"
// branch in the empty state.
func (f AMListingFilter) IsEmpty() bool {
	return f.Status == "" && f.Severity == "" && f.Alertname == "" &&
		f.Channel == "" && f.Receiver == "" && f.From == "" && f.To == ""
}

// formatDuration renders a duration as the short form used on the
// listing row + detail page header (e.g. "5m", "1h 12m", "2d 3h").
// Negative input collapses to "0s" so the renderer doesn't have to
// guard.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + "m"
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) - days*24
	if hrs == 0 {
		return strconv.Itoa(days) + "d"
	}
	return strconv.Itoa(days) + "d " + strconv.Itoa(hrs) + "h"
}

// amStatusLabel renders the loud status word on the detail page
// header — uppercase for visual weight; the chip remains lowercase
// on the listing.
func amStatusLabel(s string) string {
	switch s {
	case "firing":
		return "FIRING"
	case "resolved":
		return "RESOLVED"
	default:
		return strings.ToUpper(s)
	}
}

// FormatAMDuration is the templ-callable wrapper around formatDuration.
func FormatAMDuration(d time.Duration) string { return formatDuration(d) }

// amRowDurationText is the duration column on the listing + the trailing
// span on each history row: "Xm" / "Xh Ym" for resolved incidents, "Xm
// open" / "Xh open" for firing ones (computed against nowFunc so tests
// can pin the value).
func amRowDurationText(r AMIncidentRow) string {
	if r.EndedAt != nil {
		return formatDuration(r.EndedAt.Sub(r.StartedAt))
	}
	return formatDuration(nowFunc().Sub(r.StartedAt)) + " open"
}

// amDurationLabel renders the "(2h 5m downtime)" / "(open 3m)" trailer
// on the detail page status row. Uses the precomputed Duration on
// AMIncidentDetail so it agrees with whatever clock the handler used.
func amDurationLabel(inc AMIncidentDetail) string {
	if inc.Status == "resolved" {
		return "(" + formatDuration(inc.Duration) + " downtime)"
	}
	return "(open " + formatDuration(inc.Duration) + ")"
}
