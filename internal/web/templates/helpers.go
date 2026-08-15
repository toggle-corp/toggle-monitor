package templates

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// nowFunc is overridable for tests.
var nowFunc = time.Now

// humanInterval renders a positive duration as "30m", "1h", "2d" —
// no "ago" / "in" prefix. Used for things like reconcile cadence
// that aren't tied to a specific timestamp.
func humanInterval(d time.Duration) string {
	if d <= 0 {
		return "n/a"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// humanDuration renders a compact "5m ago" / "in 3d" / "just now"
// token suitable for sitting beside an RFC3339 timestamp. Returns ""
// for a zero time so callers can guard their layout cheaply.
func humanDuration(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := nowFunc().Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var token string
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		token = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		token = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		token = fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		token = fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		token = fmt.Sprintf("%dmo", int(d.Hours()/(24*30)))
	}
	if future {
		return "in " + token
	}
	return token + " ago"
}

// SSLCellState maps a persisted SSL status + optional expiry into the
// short label used in compact table cells. "ssl-expiring" splits into
// "expiring" (future expiry) or "expired" (past expiry) so an
// already-broken cert reads with red/down severity instead of the same
// amber as one with 25 days left. nil status → "" so callers can fall
// back to an em-dash.
func SSLCellState(status string, expiresAt *time.Time) string {
	switch status {
	case "":
		return ""
	case "ok":
		return "ok"
	case "ssl-skipped":
		return "skipped"
	case "ssl-expiring":
		if expiresAt != nil && nowFunc().After(*expiresAt) {
			return "expired"
		}
		return "expiring"
	default:
		return status
	}
}

func summary(rows []store.DiscoverySnapshotRow) string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Status]++
	}
	return fmt.Sprintf("%d total · %d added · %d kube-paused · %d kube-invalid · %d kube-ignored",
		len(rows), counts["added"], counts["kube-paused"], counts["kube-invalid"], counts["kube-ignored"])
}

func pageCount(total, perPage int) int {
	if perPage <= 0 {
		return 1
	}
	n := total / perPage
	if total%perPage != 0 {
		n++
	}
	if n == 0 {
		return 1
	}
	return n
}

func paginatorLink(base string, extra url.Values, page int) templ.SafeURL {
	v := url.Values{}
	for k, vals := range extra {
		for _, val := range vals {
			if val != "" {
				v.Add(k, val)
			}
		}
	}
	v.Set("page", strconv.Itoa(page))
	return templ.URL(base + "?" + v.Encode())
}

// sortHref builds the link for a clickable column header. Clicking
// the column that's already active flips direction; clicking a
// different column starts at ascending. All other filter params are
// preserved so sort doesn't blow away the user's search/filters.
func sortHref(key string, f MonitorsFilter) string {
	v := paramsFromFilter(f)
	v.Del("sort")
	v.Del("dir")
	v.Del("page") // jump back to page 1 on a fresh sort
	v.Set("sort", key)
	if f.Sort == key && !f.SortDesc {
		// Currently asc on this column → switch to desc.
		v.Set("dir", "desc")
	}
	q := v.Encode()
	if q == "" {
		return "/monitors"
	}
	return "/monitors?" + q
}

func paramsFromFilter(f MonitorsFilter) url.Values {
	v := url.Values{}
	if f.Search != "" {
		v.Set("q", f.Search)
	}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if f.SSL != "" {
		v.Set("ssl", f.SSL)
	}
	if f.Page != "" {
		v.Set("status_page", f.Page)
	}
	if f.Page != "" && f.Section >= 0 {
		v.Set("section", strconv.Itoa(f.Section))
	}
	if f.Sort != "" {
		v.Set("sort", f.Sort)
		if f.SortDesc {
			v.Set("dir", "desc")
		}
	}
	if f.Archived != "" && f.Archived != "active" {
		v.Set("archived", f.Archived)
	}
	for _, t := range f.Tags {
		if t != "" {
			v.Add("tag", t)
		}
	}
	return v
}

// kubeNamePrefix returns the namespace from a kube friendly-name
// produced by merger.formatFriendlyName ("(my-team) api" → "my-team").
// Returns "" for static-monitor names (operator-chosen, no parens
// convention) and any name that doesn't start with "(…)".
func kubeNamePrefix(name string) string {
	if len(name) < 2 || name[0] != '(' {
		return ""
	}
	close := strings.Index(name, ")")
	if close < 0 {
		return ""
	}
	return name[1:close]
}

// kubeNameRest returns the text after the "(namespace) " prefix.
// When no prefix is present, returns the full name so callers can
// fall back to a single-span render.
func kubeNameRest(name string) string {
	if len(name) < 2 || name[0] != '(' {
		return name
	}
	close := strings.Index(name, ")")
	if close < 0 {
		return name
	}
	return strings.TrimSpace(name[close+1:])
}

// boolYesNo renders a bool as "yes"/"no" for compact dl rows.
func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
