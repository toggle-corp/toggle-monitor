package templates

import (
	"strconv"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/slug"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// Discovery outcome constants drive the discovery detail template's
// top-level branch. The handler picks one based on whether the kube
// cascade is reachable (disabled / stale / live) and what
// merger.ResolveWithTrace returned for the live case.
const (
	DiscoveryOutcomeDisabled     = "disabled"     // cfg.Kube == nil; no cascade available
	DiscoveryOutcomeStale        = "stale"        // Ingress not in informer cache (deleted, RBAC, …)
	DiscoveryOutcomeNoMatch      = "no-match"     // cascade walked, zero rules fired (only possible against hand-edited configs)
	DiscoveryOutcomeMaterialized = "materialized" // happy path
	DiscoveryOutcomeIgnored      = "ignored"      // some rule flipped ignore: true
	DiscoveryOutcomeInvalid      = "invalid"      // resolved config failed checkResolved
)

// DiscoveryDetailView bundles everything the discovery detail
// template renders. The Outcome field drives which sub-template
// branch runs; the others are populated to match.
type DiscoveryDetailView struct {
	Row     store.DiscoverySnapshotRow
	Outcome string

	// Live-cascade data, populated when Outcome is no-match,
	// materialized, ignored, or invalid. Empty for disabled / stale.
	Trace    []merger.RuleTrace
	Resolved *config.KubeConfig

	// For Outcome=materialized: the slug the daemon would (or did)
	// materialize as. Matches the discovery snapshot's monitor_slug
	// on the happy path; rendered as a friendly link in the resolved
	// card.
	Slug string

	// For Outcome=invalid: the resolved-validation error message
	// (e.g. "timeout (30s) must be < interval (10s)"). The cascade is
	// still rendered above so the operator can see which rule wrote
	// the offending value.
	InvalidError string

	// InvalidField is a best-effort extraction of the KubeConfig key
	// that the resolved-validation error talks about. Used by the
	// template to highlight the row in the Resolved card. Empty when
	// the error doesn't map cleanly to one key.
	InvalidField string
}

// PopulateCascadeView runs merger.ResolveWithTrace and fills view's
// cascade-side fields. Kept here (rather than in the handler) so the
// template package owns the mapping between merger outcomes and
// DiscoveryDetailView shape — the handler just passes the view to
// the template.
func PopulateCascadeView(view *DiscoveryDetailView, rules []config.KubeMatchRule, ing *networkingv1.Ingress, host string) {
	resolved, traces, _, ignored, matched, resolvedErr := merger.ResolveWithTrace(rules, ing, host)
	view.Trace = traces
	switch {
	case !matched:
		view.Outcome = DiscoveryOutcomeNoMatch
	case ignored:
		view.Outcome = DiscoveryOutcomeIgnored
		// Keep the resolved block so the "would-have-been" panel can
		// render it collapsed.
		r := resolved
		view.Resolved = &r
	case resolvedErr != nil:
		view.Outcome = DiscoveryOutcomeInvalid
		view.InvalidError = resolvedErr.Error()
		view.InvalidField = invalidFieldFromError(resolvedErr.Error())
		r := resolved
		view.Resolved = &r
	default:
		view.Outcome = DiscoveryOutcomeMaterialized
		r := resolved
		view.Resolved = &r
		if s, err := slug.SanitizeKubeDiscovered(ing.Namespace, ing.Name, host); err == nil {
			view.Slug = s
		}
	}
}

// invalidFieldFromError parses the validator's error messages enough
// to highlight the row in the Resolved card. checkResolved produces
// messages that always lead with the KubeConfig YAML key (e.g.
// "interval (10s) must be greater than timeout (30s)"), so a simple
// prefix scan covers every shape.
func invalidFieldFromError(msg string) string {
	for _, key := range []string{
		"interval", "timeout", "path", "httpMethod", "acceptedStatusCodes",
		"retries", "retryBackoff", "reminderInterval",
		"sslAlertThreshold", "sslEscalationThreshold", "sslReminderInterval",
		"slack",
	} {
		if len(msg) >= len(key) && msg[:len(key)] == key {
			return key
		}
	}
	return ""
}

// TraceActionClass maps a merger.TraceAction to a Tailwind chip
// fragment for the action label rendered in the cascade card. Kept
// near the outcome constants so the visual vocabulary stays in one
// place.
func TraceActionClass(a merger.TraceAction) string {
	switch a {
	case merger.TraceSet:
		return "bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300"
	case merger.TraceReplace:
		return "bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300"
	case merger.TraceAdd:
		return "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300"
	case merger.TraceOverride:
		return "bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300"
	default:
		return "bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300"
	}
}

// FormatTraceList re-exports merger's compact list rendering so the
// .templ files don't need a direct merger import in expression
// positions.
func FormatTraceList(values []string) string { return merger.FormatTraceList(values) }

// intsToTraceList renders an []int as []string. Used by the resolved
// card to pipe acceptedStatusCodes through FormatTraceList.
func intsToTraceList(in []int) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.Itoa(v)
	}
	return out
}

// intToString is the .templ-callable form of strconv.Itoa. Kept
// alongside boolYesNo in this package so the rendering helpers stay
// in one place.
func intToString(n int) string { return strconv.Itoa(n) }
