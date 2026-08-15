package templates

import (
	"strconv"
	"strings"
)

// Tone is the design system's status vocabulary. Every colored element
// in the UI resolves to one of these, and each one renders as a colored
// mark *plus* a text label — color never carries meaning on its own, so
// the console stays readable in grayscale and at AA contrast.
//
// ToneNeutral is the "this is a count, not a problem" tone used for
// totals; ToneAccent is chip-only and reserved for the indigo accent.
type Tone string

const (
	ToneUp       Tone = "up"
	ToneDown     Tone = "down"
	TonePaused   Tone = "paused"
	ToneWarn     Tone = "warn"
	ToneSSL      Tone = "ssl"
	ToneArchived Tone = "archived"
	ToneNeutral  Tone = "neutral"
	ToneAccent   Tone = "accent"
)

// toneText is the readable text color for a tone. The role tokens
// behind these lift in lightness on the dark theme so both ends of the
// ramp meet AA.
func toneText(t Tone) string {
	switch t {
	case ToneUp:
		return "text-up"
	case ToneDown:
		return "text-down"
	case TonePaused, ToneWarn, ToneSSL:
		return "text-warn"
	case ToneArchived:
		return "text-idle"
	case ToneAccent:
		return "text-accent"
	default:
		return "text-ink"
	}
}

// toneMark is the saturated dot/square color for a tone. Marks stay
// fully saturated in both themes — only the text tones shift.
func toneMark(t Tone) string {
	switch t {
	case ToneUp:
		return "bg-up-mark"
	case ToneDown:
		return "bg-down-mark"
	case TonePaused, ToneWarn, ToneSSL:
		return "bg-warn-mark"
	case ToneArchived:
		return "bg-dim"
	case ToneAccent:
		return "bg-accent"
	default:
		return "bg-ink"
	}
}

// toneWash is the soft background a tone paints behind content — the
// incident banner's fill. Neutral and archived have none.
func toneWash(t Tone) string {
	switch t {
	case ToneDown:
		return "bg-down-soft"
	case TonePaused, ToneWarn, ToneSSL:
		return "bg-warn-soft"
	case ToneUp:
		return "bg-up-soft"
	default:
		return ""
	}
}

// rowWash is toneWash for table rows, where a healthy row gets nothing.
// Color is reserved for problems: if every row in a listing of fifty is
// tinted, the two that need attention stop standing out.
func rowWash(t Tone) string {
	if t == ToneUp {
		return ""
	}
	return toneWash(t)
}

// toneLabel is the default text label for a tone, used when a caller
// doesn't supply its own.
func toneLabel(t Tone) string {
	switch t {
	case ToneUp:
		return "up"
	case ToneDown:
		return "down"
	case TonePaused:
		return "paused"
	case ToneWarn:
		return "warn"
	case ToneSSL:
		return "ssl exp"
	case ToneArchived:
		return "archived"
	default:
		return "total"
	}
}

// chipTone colors a chip. Unlike a badge, a neutral chip reads as muted
// rather than primary text — a chip is a label on something else, not a
// status in its own right.
func chipTone(t Tone) string {
	if t == ToneNeutral || t == "" {
		return "text-muted"
	}
	return toneText(t)
}

// metricTone colors a metric tile's label and value. Healthy and total
// counts render as primary text unless the caller asks for `vivid` —
// on an overview screen the one green tile is a deliberate choice, not
// a default. A zero count is never colored either, whatever its tone:
// a rollup where nothing is wrong should have no red on it, and the
// tile's dot still carries which state it counts.
func metricTone(t Tone, vivid bool, count int) string {
	if count == 0 {
		return "text-ink"
	}
	if !vivid && (t == ToneUp || t == ToneNeutral || t == ToneArchived) {
		return "text-ink"
	}
	return toneText(t)
}

// MonitorTone maps a persisted monitor status (plus the archived flag,
// which outranks it) onto the status vocabulary.
func MonitorTone(status string, archived bool) Tone {
	if archived {
		return ToneArchived
	}
	switch status {
	case "up":
		return ToneUp
	case "down":
		return ToneDown
	case "temporary-paused":
		return TonePaused
	default:
		return ToneNeutral
	}
}

// statusBadgeLabelFor is the wording a monitor status carries in a
// badge. Known statuses use the design's vocabulary; anything else
// shows its raw value rather than being silently mislabelled.
func statusBadgeLabelFor(status string) string {
	switch status {
	case "up":
		return "up"
	case "down":
		return "down"
	case "temporary-paused":
		return "paused"
	default:
		return status
	}
}

// HTTPCodeTone maps an HTTP status code onto the vocabulary: 2xx reads
// as up, 3xx as a warning worth noticing, 4xx/5xx as down.
func HTTPCodeTone(code int) Tone {
	switch {
	case code >= 200 && code < 300:
		return ToneUp
	case code >= 300 && code < 400:
		return ToneWarn
	case code >= 400:
		return ToneDown
	default:
		return ToneNeutral
	}
}

// EventTone colors an alert-event kind (open/resolve/reminder and their
// ssl_ variants).
func EventTone(t string) Tone {
	switch t {
	case "open", "ssl_open":
		return ToneDown
	case "resolve", "ssl_resolve":
		return ToneUp
	case "reminder", "ssl_reminder":
		return ToneWarn
	default:
		return ToneNeutral
	}
}

// SSLTone colors an SSL status. It accepts both the persisted values
// and the short labels SSLCellState produces.
func SSLTone(s string) Tone {
	switch s {
	case "ok":
		return ToneUp
	case "ssl-expiring", "expiring":
		return ToneWarn
	case "expired":
		return ToneDown
	case "ssl-skipped", "skipped":
		return ToneArchived
	default:
		return ToneNeutral
	}
}

// DiscoveryTone colors a discovery-snapshot row status.
func DiscoveryTone(s string) Tone {
	switch s {
	case "added":
		return ToneUp
	case "kube-paused":
		return ToneArchived
	case "kube-invalid":
		return ToneDown
	case "kube-ignored":
		return ToneWarn
	default:
		return ToneNeutral
	}
}

// statusTone maps the 3-state status-page rollup kind
// (statusBadgeKind / pageBadgeKind) onto the vocabulary. "none" is a
// page whose sections select no monitors — idle, not healthy.
func statusTone(kind string) Tone {
	switch kind {
	case "down":
		return ToneDown
	case "warn":
		return ToneWarn
	case "none":
		return ToneArchived
	default:
		return ToneUp
	}
}

// AMSeverityTone colors an Alertmanager severity label. The vocabulary
// covers AM's de-facto common severities; anything else (or empty)
// falls through to neutral. Matched case-insensitively — the label
// comes from whoever wrote the alerting rule.
func AMSeverityTone(s string) Tone {
	switch strings.ToLower(s) {
	case "critical", "page":
		return ToneDown
	case "warning", "warn":
		return ToneWarn
	case "info":
		return ToneAccent
	default:
		return ToneNeutral
	}
}

// AMStatusTone colors an Alertmanager incident status.
func AMStatusTone(s string) Tone {
	switch s {
	case "firing":
		return ToneDown
	case "resolved":
		return ToneUp
	default:
		return ToneNeutral
	}
}

// px renders an integer as a CSS pixel length, for the few components
// whose geometry is a design constant rather than a spacing token
// (avatar diameters, icon sizes).
func px(n int) string { return strconv.Itoa(n) + "px" }

// gridTemplate builds the inline custom property a Table publishes to
// its rows. Column templates come straight from the design's per-screen
// specs (e.g. "1.9fr .9fr .7fr 1.1fr .8fr" for the monitors listing).
func gridTemplate(cols string) string { return "--tm-cols:" + cols }

// sizeStyle is the inline width/height pair for a fixed-geometry
// element.
func sizeStyle(n int) string { return "width:" + px(n) + ";height:" + px(n) }
