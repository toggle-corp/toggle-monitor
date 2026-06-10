package alertmanager

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// DefaultBodyMaxChars caps the rendered `annotations.summary` length
// in Slack. The full text — and the longer `description` annotation —
// remain accessible on the /alert/{id} detail page.
const DefaultBodyMaxChars = 1500

// lateResolveBanner is the default banner rendered atop a standalone
// resolve message when the receiver has no record of the alert ever
// firing. Mirrors the freshParentBanner convention in the probe path.
const lateResolveBanner = "ℹ️ Resolved without an open incident on file — possibly a process restart or first-time delivery of a stale resolve."

// keyLabelOrder defines the canonical, bounded set of labels surfaced
// in the title chip row. The full label set lives on the detail page;
// here we cap display cardinality at three so the title stays
// scannable. Order is stable across renders.
var keyLabelOrder = []string{"namespace", "instance", "service", "job", "cluster", "pod"}

const maxKeyLabels = 3

// AMOpenInput carries everything BuildAMOpen needs.
type AMOpenInput struct {
	Alert        Alert    // the AM Alert object
	Mentions     []string // pre-resolved Slack markup (`<!here>`, `<@U…>`)
	DetailURL    string   // toggle-monitor /alert/{id} link; empty → omit View-details
	BodyMaxChars int      // hard cap on summary length; 0 → DefaultBodyMaxChars
	Receiver     string   // payload envelope field, displayed in footer
	ExternalURL  string   // payload envelope field, displayed in footer (host only)
}

// AMResolveInput carries everything the resolve-edit and resolve-reply
// renderers need. Embeds AMOpenInput so the resolve banner can re-render
// the original title + body inline.
type AMResolveInput struct {
	AMOpenInput
	ResolvedAt time.Time
	Downtime   time.Duration
	Banner     string // optional late-resolve banner; "" for normal resolve-edit
}

// BuildAMOpen builds the parent message for a firing AM alert using
// the three-block iA2 shape from ADR-0006: title section (emoji +
// alertname + severity chip + key=`value` chips), body section
// (annotations.summary), footer context (mentions + Receiver + Via +
// Firing-ago + View-details + Runbook as inline mrkdwn links). No
// attachments, no header block, no actions block.
func BuildAMOpen(in AMOpenInput) slack.Message {
	title := amTitle(in, severityEmoji(in.Alert), "")
	footer := amFooter(in, "" /*resolveStamp*/)
	return slack.Message{
		Blocks: amBlocks(title, "" /*banner*/, bodyText(in), footer),
	}
}

// BuildAMResolveEdit edits the parent in place when the alert resolves.
// The emoji swaps to ✅ and a `_Resolved_` suffix is appended to the
// title; the footer replaces the "Firing N ago" stamp with a
// "Resolved <date> · down around N" stamp.
func BuildAMResolveEdit(in AMResolveInput) slack.Message {
	title := amTitle(in.AMOpenInput, "✅", "  ·  _Resolved_")
	stamp := fmt.Sprintf("_Resolved %s · down around %s_",
		slack.FormatDate(in.ResolvedAt),
		formatDowntime(in.Downtime))
	footer := amFooter(in.AMOpenInput, stamp)
	return slack.Message{
		Blocks: amBlocks(title, in.Banner, bodyText(in.AMOpenInput), footer),
	}
}

// BuildAMResolveReply renders the short in-thread reply emitted on
// resolve. Operators glance, they don't read.
func BuildAMResolveReply(in AMResolveInput) []slack.Block {
	return []slack.Block{
		sectionBlock(fmt.Sprintf("✅ Resolved after %s", formatDowntime(in.Downtime))),
	}
}

// BuildAMLateResolve renders a standalone resolve message for a
// fingerprint we never saw open (no parent on file). Same shape as
// BuildAMResolveEdit but with the late-resolve banner inserted as a
// context block between title and body.
func BuildAMLateResolve(in AMResolveInput) slack.Message {
	if in.Banner == "" {
		in.Banner = lateResolveBanner
	}
	return BuildAMResolveEdit(in)
}

// AMThrottleNoticeInput carries the per-channel state the throttle
// notice renders.
type AMThrottleNoticeInput struct {
	ChannelSlug string        // human-readable channel handle
	Dropped     int           // count of drops accumulated since the previous notice
	PerChannel  int           // operator-configured per-window limit
	Window      time.Duration // operator-configured window
}

// BuildAMThrottleNotice renders the "AM throttle engaged" warning the
// handler posts when a channel trips the sliding-window limiter. A
// single section block, no footer / no buttons — operators should
// spot it without it pretending to be its own alert.
func BuildAMThrottleNotice(in AMThrottleNoticeInput) slack.Message {
	msg := fmt.Sprintf(
		":warning: Alertmanager throttle engaged in `#%s` — dropped %d alert%s "+
			"(limit: %d per %s). Likely misconfigured upstream — check your AM `group_by`.",
		in.ChannelSlug,
		in.Dropped,
		plural(in.Dropped),
		in.PerChannel,
		in.Window,
	)
	return slack.Message{
		Blocks: []slack.Block{sectionBlock(msg)},
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// -- internals --------------------------------------------------------

// severityEmoji picks the lead emoji from labels["severity"]. Unknown
// or missing severity falls back to the siren.
func severityEmoji(a Alert) string {
	switch a.Labels["severity"] {
	case "critical":
		return "🔥"
	case "warning":
		return "⚠️"
	case "info":
		return "ℹ️"
	default:
		return "🚨"
	}
}

// amTitle renders the title-section mrkdwn:
//
//	<emoji> *<alertname>*  `<severity>`  ·  <k>=`<v>`  ·  <k>=`<v>`  <suffix>
//
// severity chip is omitted when the label is absent. suffix is
// appended verbatim (e.g. " · _Resolved_" for the resolve variant).
func amTitle(in AMOpenInput, emoji, suffix string) string {
	name := in.Alert.Labels["alertname"]
	if name == "" {
		name = "(unnamed alert)"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%s *%s*", emoji, name))
	if sev := in.Alert.Labels["severity"]; sev != "" {
		parts = append(parts, "`"+sev+"`")
	}
	parts = append(parts, keyLabelChips(in.Alert.Labels)...)
	out := strings.Join(parts, "  ·  ")
	if suffix != "" {
		out += suffix
	}
	return out
}

// keyLabelChips picks up to maxKeyLabels labels in canonical order and
// returns them as `key=\`value\“ mrkdwn chips.
func keyLabelChips(labels map[string]string) []string {
	var chips []string
	for _, k := range keyLabelOrder {
		v, ok := labels[k]
		if !ok || v == "" {
			continue
		}
		chips = append(chips, fmt.Sprintf("%s=`%s`", k, v))
		if len(chips) == maxKeyLabels {
			break
		}
	}
	return chips
}

// amBlocks composes the AM parent's blocks-only payload: title, then
// optional banner (between title and body), then body, then a single
// footer context block.
func amBlocks(title, banner, body, footer string) []slack.Block {
	blocks := []slack.Block{sectionBlock(title)}

	if banner != "" {
		blocks = append(blocks, contextBlock(banner))
	}

	blocks = append(blocks, sectionBlock(body))

	if footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	return blocks
}

// bodyText returns the rendered body string (summary or placeholder),
// truncated to BodyMaxChars when the summary is too long.
func bodyText(in AMOpenInput) string {
	summary := in.Alert.Annotations["summary"]
	if summary == "" {
		return "_no summary_"
	}
	cap := in.BodyMaxChars
	if cap <= 0 {
		cap = DefaultBodyMaxChars
	}
	if len(summary) > cap {
		summary = summary[:cap] + "…"
	}
	return summary
}

// amFooter renders the AM parent footer as a single mrkdwn string:
//
//	<mentions>  ·  Receiver `X`  ·  Via `host`  ·  _Firing Nm ago_  ·  <det|View details>  ·  <runbook|Runbook>
//
// When resolveStamp is non-empty it replaces the "Firing N ago"
// segment with the caller-supplied stamp (used by the resolve edit
// for "_Resolved <date> · down around N_"). Any segment whose input
// is empty is skipped; an entirely empty footer returns "" so the
// caller can skip the block.
func amFooter(in AMOpenInput, resolveStamp string) string {
	var parts []string
	if len(in.Mentions) > 0 {
		parts = append(parts, strings.Join(in.Mentions, " "))
	}
	if in.Receiver != "" {
		parts = append(parts, "Receiver `"+in.Receiver+"`")
	}
	if host := externalHost(in.ExternalURL); host != "" {
		parts = append(parts, "Via `"+host+"`")
	}
	switch {
	case resolveStamp != "":
		parts = append(parts, resolveStamp)
	case !in.Alert.StartsAt.IsZero():
		parts = append(parts, fmt.Sprintf("_Firing %s ago_", relativeAgo(in.Alert.StartsAt)))
	}
	if in.DetailURL != "" {
		parts = append(parts, fmt.Sprintf("<%s|View details>", in.DetailURL))
	}
	if rb := in.Alert.Annotations["runbook_url"]; rb != "" {
		parts = append(parts, fmt.Sprintf("<%s|Runbook>", rb))
	}
	return strings.Join(parts, "  ·  ")
}

// externalHost extracts the host from a URL; on parse failure (or
// when url.Parse returns empty Host, e.g. for raw strings) the raw
// input is returned so the footer still says something useful.
func externalHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// relativeAgo returns a compact "Ns" / "Nm" / "Nh" / "Nd" suffix for
// the "Firing N ago" footer line. Bounded to one unit.
func relativeAgo(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
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

// formatDowntime renders a compact human form like "1d 4h" or "12m"
// or "2h". Caps display at two units so resolve replies stay terse.
func formatDowntime(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int(d / time.Second)

	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	// Drop minutes once we have days, since "1d 4h" reads better.
	if mins > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	return strings.Join(parts, " ")
}

// sectionBlock + contextBlock are tiny local mirrors of the unexported
// helpers in internal/slack/blocks.go. Kept local so this file is the
// only thing that needs to evolve when AM block shapes change.
func sectionBlock(mrkdwn string) slack.Block {
	return slack.Block{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": mrkdwn},
	}
}

func contextBlock(mrkdwn string) slack.Block {
	return slack.Block{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": mrkdwn},
		},
	}
}
