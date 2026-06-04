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
const lateResolveBanner = "ℹ️ This alert resolved without an open incident on file — possibly a process restart or first-time delivery of a stale resolve."

// keyLabelOrder defines the canonical, bounded set of labels surfaced
// in the Slack header chip. The full label set lives on the detail
// page; here we cap display cardinality at three so the header stays
// scannable. Order is stable across renders.
var keyLabelOrder = []string{"namespace", "instance", "service", "job", "cluster", "pod"}

const maxKeyLabels = 3

// AMOpenInput carries everything BuildAMOpen needs.
type AMOpenInput struct {
	Alert        Alert    // the AM Alert object
	Mentions     []string // pre-resolved Slack markup (`<!here>`, `<@U…>`)
	DetailURL    string   // toggle-monitor /alert/{id} link; empty → omit button
	BodyMaxChars int      // hard cap on summary length; 0 → DefaultBodyMaxChars
	Receiver     string   // payload envelope field, displayed in footer
	ExternalURL  string   // payload envelope field, displayed in footer (host only)
}

// AMResolveInput carries everything the resolve-edit and resolve-reply
// renderers need. Embeds AMOpenInput so the resolve banner can re-render
// the original header + body inline.
type AMResolveInput struct {
	AMOpenInput
	ResolvedAt time.Time
	Downtime   time.Duration
	Banner     string // optional late-resolve banner; "" for normal resolve-edit
}

// BuildAMOpen builds the parent message for a firing AM alert.
//
// Header:   severity emoji + alertname + [severity] chip + key labels
// CC line:  pre-resolved mention markup (operator notifications)
// Body:     annotations["summary"] (truncated at BodyMaxChars when set)
// Actions:  "View details →" (DetailURL) + "Runbook →" (runbook_url)
// Footer:   Receiver · Via <ExternalURL host> · Firing since <relative>
func BuildAMOpen(in AMOpenInput) slack.Message {
	return slack.Message{
		Blocks: []slack.Block{headerBlock(in, severityEmoji(in.Alert), "")},
		Attachments: []slack.Attachment{{
			Color:  amColor(in.Alert),
			Blocks: amBodyBlocks(in, "" /*banner*/, nil /*extraFooter*/),
		}},
	}
}

// BuildAMResolveEdit edits the parent in place when the alert resolves.
// Same header layout but the severity emoji swaps to ✅, a "· Resolved"
// suffix is appended, and a resolved-at + downtime line is appended to
// the footer. Body, action row, and original footer remain so the
// message stays coherent if viewed standalone.
func BuildAMResolveEdit(in AMResolveInput) slack.Message {
	extraFooter := []string{
		fmt.Sprintf("Resolved at %s · Downtime %s",
			slack.FormatDate(in.ResolvedAt),
			formatDowntime(in.Downtime)),
	}
	return slack.Message{
		Blocks: []slack.Block{headerBlock(in.AMOpenInput, "✅", "· Resolved")},
		Attachments: []slack.Attachment{{
			Color:  slack.ColorResolved,
			Blocks: amBodyBlocks(in.AMOpenInput, in.Banner, extraFooter),
		}},
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
// fingerprint we never saw open (no parent on file). Mirrors
// BuildAMResolveEdit but with a "this alert resolved without an open
// incident" banner. Caller may override Banner; empty falls back to
// the package default.
func BuildAMLateResolve(in AMResolveInput) slack.Message {
	if in.Banner == "" {
		in.Banner = lateResolveBanner
	}
	return BuildAMResolveEdit(in)
}

// AMThrottleNoticeInput carries the per-channel state the throttle
// notice renders. ChannelSlug is operator-visible (it appears in the
// rendered message) so they can connect the warning to the AM-side
// route group_by that's likely overproducing alerts.
type AMThrottleNoticeInput struct {
	ChannelSlug string        // human-readable channel handle
	Dropped     int           // count of drops accumulated since the previous notice
	PerChannel  int           // operator-configured per-window limit
	Window      time.Duration // operator-configured window
}

// BuildAMThrottleNotice renders the "AM throttle engaged" warning the
// handler posts the first time a channel trips the sliding-window
// limiter, and again every noticeEvery while the channel stays engaged.
// It's a plain Slack section block, no attachment / no buttons — the
// goal is a single dim line that operators can spot without it
// pretending to be its own alert.
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

// amColor picks the attachment stripe color based on severity. We
// reuse the monitor palette so probe + AM messages read as kindred.
func amColor(a Alert) string {
	switch a.Labels["severity"] {
	case "warning", "info":
		return slack.ColorSSLWarn // amber
	default:
		return slack.ColorDown // red (critical + unknown)
	}
}

// headerBlock renders the Slack header line:
//
//	<emoji>  <alertname> [<severity>]  ·  <key=v>  <key=v>  <key=v>  <suffix>
//
// suffix is appended verbatim (e.g. "· Resolved" for the edit variant).
func headerBlock(in AMOpenInput, emoji, suffix string) slack.Block {
	parts := []string{emoji}

	name := in.Alert.Labels["alertname"]
	if name == "" {
		name = "(unnamed alert)"
	}
	if sev := in.Alert.Labels["severity"]; sev != "" {
		parts = append(parts, fmt.Sprintf(" %s [%s]", name, sev))
	} else {
		parts = append(parts, " "+name)
	}

	if chips := keyLabelChips(in.Alert.Labels); len(chips) > 0 {
		parts = append(parts, "  ·  "+strings.Join(chips, "  "))
	}

	if suffix != "" {
		parts = append(parts, "  "+suffix)
	}

	// plain_text + emoji:true keeps unicode emoji rendering consistent
	// with bigHeader in internal/slack/blocks.go.
	return slack.Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  strings.Join(parts, ""),
			"emoji": true,
		},
	}
}

// keyLabelChips picks up to maxKeyLabels labels in canonical order. Stable.
func keyLabelChips(labels map[string]string) []string {
	var chips []string
	for _, k := range keyLabelOrder {
		v, ok := labels[k]
		if !ok || v == "" {
			continue
		}
		chips = append(chips, fmt.Sprintf("%s=%s", k, v))
		if len(chips) == maxKeyLabels {
			break
		}
	}
	return chips
}

// amBodyBlocks renders the attachment-wrapped body: mentions (CC), the
// summary body, the action row, and the small context footer.
//
// banner is rendered at the very top when non-empty (resolve-edit /
// late-resolve banners). extraFooter lines are appended as additional
// dim context lines below the main footer (used for the resolve
// "Resolved at … · Downtime …" stamp).
func amBodyBlocks(in AMOpenInput, banner string, extraFooter []string) []slack.Block {
	var blocks []slack.Block

	if banner != "" {
		blocks = append(blocks, contextBlock(banner))
	}

	// Mentions ride at the top of the body, mirroring buildParentBlocks
	// in internal/slack/blocks.go where "*CC:* …" is the first line.
	if len(in.Mentions) > 0 {
		blocks = append(blocks, contextBlock("*CC:* "+strings.Join(in.Mentions, " ")))
	}

	// Body: summary or italic placeholder.
	blocks = append(blocks, sectionBlock(bodyText(in)))

	// Actions row: View details + Runbook. Omit entirely when neither URL set.
	if row := actionsRow(in); row != nil {
		blocks = append(blocks, row)
	}

	// Footer: Receiver · Via · Firing since.
	if footer := footerLine(in); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	for _, extra := range extraFooter {
		if extra != "" {
			blocks = append(blocks, contextBlock(extra))
		}
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

// actionsRow assembles the Slack `actions` block carrying the View
// details + Runbook buttons. Returns nil when neither URL is set so
// callers can skip appending the block altogether.
func actionsRow(in AMOpenInput) slack.Block {
	var buttons []map[string]any
	if in.DetailURL != "" {
		buttons = append(buttons, slack.LinkButton("View details →", in.DetailURL, "primary"))
	}
	if rb := in.Alert.Annotations["runbook_url"]; rb != "" {
		buttons = append(buttons, slack.LinkButton("Runbook →", rb, ""))
	}
	if len(buttons) == 0 {
		return nil
	}
	return slack.ActionsBlock(buttons...)
}

// footerLine renders the small context footer: Receiver · Via · Firing since.
// Any field with a zero value is omitted; an entirely empty footer
// returns "" so the caller can skip the block.
func footerLine(in AMOpenInput) string {
	var parts []string
	if in.Receiver != "" {
		parts = append(parts, "Receiver: `"+in.Receiver+"`")
	}
	if host := externalHost(in.ExternalURL); host != "" {
		parts = append(parts, "Via: `"+host+"`")
	}
	if !in.Alert.StartsAt.IsZero() {
		parts = append(parts, "Firing since: "+relativeAgo(in.Alert.StartsAt))
	}
	return strings.Join(parts, " · ")
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

// relativeAgo renders a compact "Nm ago" / "Nh ago" / "Nd ago" for the
// header footer. Bounded to one unit, never multi-part.
func relativeAgo(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// formatDowntime renders a compact human form like "1d 4h" or "12m"
// or "2h". Mirrors internal/slack/blocks.go's formatDowntime but
// caps display at two units so resolve replies stay terse.
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
	// Drop minutes once we have days, since "1d 4h" reads better than
	// "1d 4h 23m"; the detail page carries the exact figure.
	if mins > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	return strings.Join(parts, " ")
}

// sectionBlock + contextBlock are tiny local mirrors of the unexported
// helpers in internal/slack/blocks.go. They render identical shapes —
// exporting the slack-package versions would force ripple edits across
// every existing caller for no semantic gain. Kept local so this file
// is the only thing that needs to evolve when AM block shapes change.
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
