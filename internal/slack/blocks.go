package slack

import (
	"fmt"
	"strings"
	"time"
)

// Block is a single Block Kit block; serialized to JSON by the client.
// Using map[string]any keeps the builders concise; tests can navigate
// the result without round-tripping through JSON.
type Block = map[string]any

// Slack attachment left-edge stripe colors used by the parent
// (and edited-parent) messages.
const (
	ColorDown     = "#df3617" // uptime parent
	ColorResolved = "#22af64" // uptime parent on resolve
	ColorSSLWarn  = "#f2b138" // SSL parent (expiring)
	ColorRemoved  = "#f2b138" // monitor-removed warning
)

// DownInput carries everything the parent uptime message needs.
type DownInput struct {
	FriendlyName string
	Tags         []string
	URL          string
	Mentions     []string // raw Slack markup, e.g. "<!here>", "<@U123>"
	StatusCode   int
	StatusText   string // e.g. "Service Unavailable"; empty for transport-level errors
	FailureAt    time.Time
	LastError    string // short summary, never the full body
	ResponseBody string // already truncated by the caller; empty to skip inline body
	BodyMaxChars int    // inline body only when len(ResponseBody) <= this
	DetailURL    string // empty omits the [View details] footer link
	Note         string // small dim line rendered above the footer; "" omits
	Banner       string // optional banner rendered at the very top of the body (used for late-notice fresh-parent fallback). "" omits.
}

// ResolveInput carries DownInput plus the resolved-at moment so the
// edited-parent and the thread reply can render consistent downtime.
type ResolveInput struct {
	DownInput
	ResolveAt time.Time
	Downtime  time.Duration
}

// ReminderInput carries the data needed for an in-thread "still down"
// reminder.
type ReminderInput struct {
	DownDuration  time.Duration
	LastCheckedAt time.Time
	LastError     string
}

// detailLine is one "*Label:* value" row appended to the parent body.
type detailLine struct {
	Label, Value string
}

// ParentMessage is what the parent-message builders return. The
// header rides on the top-level Blocks (no color stripe) so it stays
// at full prominence; Attachments carries the colored stripe wrapping
// only the quieter body / note / footer.
type ParentMessage struct {
	Blocks      []Block
	Attachments []Attachment
}

// Message is the generic block-kit envelope cross-package renderers
// return. It carries the same `{Blocks, Attachments}` shape Slack's
// chat.postMessage accepts, so handlers can post the result without an
// adapter. ParentMessage is the monitor-specific alias preserved for
// existing callers; new renderers (e.g. the Alertmanager receiver in
// internal/alertmanager) use Message.
type Message = ParentMessage

// BuildDownParent renders the initial 🔴 parent for an uptime
// incident. The header sits outside the color attachment so the red
// stripe wraps only the smaller body lines.
func BuildDownParent(in DownInput) ParentMessage {
	return ParentMessage{
		Blocks: []Block{bigHeader(":red_circle: " + in.FriendlyName + " is DOWN")},
		Attachments: []Attachment{{
			Color:  ColorDown,
			Blocks: buildParentBlocks(in, nil, "Detected", in.FailureAt),
		}},
	}
}

// BuildResolveEdit renders the attachments the parent should be
// edited to when the monitor recovers. The header swaps to
// :large_green_circle:, the color stripe swaps to green, a Duration
// line is appended to the body, and the footer rewrites to
// "Resolved <date>".
func BuildResolveEdit(in ResolveInput) ParentMessage {
	return ParentMessage{
		Blocks: []Block{bigHeader(fmt.Sprintf(":large_green_circle: %s is UP (was down for %s)",
			in.FriendlyName, formatDowntime(in.Downtime)))},
		Attachments: []Attachment{{
			Color: ColorResolved,
			Blocks: buildParentBlocks(in.DownInput,
				[]detailLine{{Label: "Duration", Value: "`" + formatDowntime(in.Downtime) + "`"}},
				"Resolved", in.ResolveAt),
		}},
	}
}

// BuildReminderReply renders the short scannable thread reply posted
// every reminderInterval while the monitor remains down. No mentions.
// Stays inside a thread, so no color stripe. One *Label:* value per
// line for consistency with the parent body.
func BuildReminderReply(in ReminderInput) []Block {
	lines := []string{
		"⏰ *Still down for:* `" + formatDowntime(in.DownDuration) + "`",
		"*Last checked:* " + FormatDate(in.LastCheckedAt),
	}
	if in.LastError != "" {
		lines = append(lines, "*Last error:* `"+in.LastError+"`")
	}
	return []Block{section(strings.Join(lines, "\n"))}
}

// BuildResolveReply renders the thread reply emitted on resolve.
// Stays inside a thread, so no color stripe.
func BuildResolveReply(in ResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Resolved at %s. Total downtime: `%s`.",
		FormatDate(in.ResolveAt), formatDowntime(in.Downtime))
	return []Block{section(text)}
}

// buildParentBlocks renders the colored half of the parent message:
// a single context block with the mentions + UR-style fields (no
// header — that's a top-level block outside the attachment), an
// optional response body, the cascading-effect note, and the
// timestamp footer.
//
// `extra` lines are appended to the body (used by BuildResolveEdit
// to surface Duration). footerPrefix + footerTime drive the
// "Detected <date>" / "Resolved <date>" line; either may be empty.
//
// Mentions ride at the top of the body lines rather than in a
// separate section. Slack still fires notifications from mrkdwn in
// context blocks as long as the markup is the canonical `<!here>` /
// `<@U…>` form (which our ResolveMentions helper guarantees).
func buildParentBlocks(in DownInput, extra []detailLine, footerPrefix string, footerTime time.Time) []Block {
	var blocks []Block

	// Optional late-notice banner sits at the very top so operators
	// notice it before scanning the regular body. Used by the
	// fresh-parent fallback when an initial Open failed to deliver.
	if in.Banner != "" {
		blocks = append(blocks, contextBlock(in.Banner))
	}

	// Body in a context block: smaller + dimmer than a section.
	// Mentions sit at the very top, prefixed with "*CC:*" so the row
	// reads as a labeled field consistent with the rest of the body.
	var lines []string
	if len(in.Mentions) > 0 {
		lines = append(lines, "*CC:* "+strings.Join(in.Mentions, " "))
	}
	if in.URL != "" {
		lines = append(lines, "*Monitor URL:* "+in.URL)
	}
	if in.StatusCode != 0 || in.StatusText != "" {
		lines = append(lines, fmt.Sprintf("*Reason:* `%d %s`", in.StatusCode, in.StatusText))
	}
	if in.LastError != "" {
		lines = append(lines, "*Error:* `"+in.LastError+"`")
	}
	if len(in.Tags) > 0 {
		lines = append(lines, "*Tags:* `"+strings.Join(in.Tags, "`, `")+"`")
	}
	for _, e := range extra {
		lines = append(lines, "*"+e.Label+":* "+e.Value)
	}
	if len(lines) > 0 {
		blocks = append(blocks, contextBlock(strings.Join(lines, "\n")))
	}

	// Optional inline response body.
	if in.ResponseBody != "" && len(in.ResponseBody) <= in.BodyMaxChars {
		blocks = append(blocks, section("```\n"+in.ResponseBody+"\n```"))
	}

	// Small dim note (e.g. "⏸ Pauses dependents: `a`, `b`"). Rendered
	// just above the footer so cascading-effect context sits close to
	// the timestamp.
	if in.Note != "" {
		blocks = append(blocks, contextBlock(in.Note))
	}

	// Footer: timestamp + View details link.
	if footer := footerLine(footerPrefix, footerTime, in.DetailURL); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}

	return blocks
}

// footerLine assembles the parent footer mrkdwn:
//
//	_<prefix> <date>_  ·  <DetailURL|View details>
//
// Either half is omitted when its input is empty. Returns "" when
// nothing to render.
func footerLine(prefix string, t time.Time, detailURL string) string {
	var parts []string
	if prefix != "" && !t.IsZero() {
		parts = append(parts, fmt.Sprintf("_%s %s_", prefix, FormatDate(t)))
	}
	if detailURL != "" {
		parts = append(parts, fmt.Sprintf("<%s|View details>", detailURL))
	}
	return strings.Join(parts, " · ")
}

// bigHeader is a Slack `header` block (large, bold). plain_text with
// emoji:true so `:red_circle:` style shortcodes resolve.
func bigHeader(text string) Block {
	return Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  text,
			"emoji": true,
		},
	}
}

func section(mrkdwn string) Block {
	return Block{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": mrkdwn},
	}
}

// contextBlock wraps a single mrkdwn element in a Slack context block.
// Context blocks render at a smaller / dimmer "auxiliary text" size
// than regular sections — used here for the parent footer.
func contextBlock(mrkdwn string) Block {
	return Block{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": mrkdwn},
		},
	}
}

// LinkButton renders a single Block Kit `button` element pointing at
// url. Exported so cross-package renderers (e.g. the Alertmanager
// receiver) can compose an actions row without duplicating the element
// shape. style is one of "" (default), "primary", or "danger".
func LinkButton(text, url, style string) map[string]any {
	el := map[string]any{
		"type": "button",
		"text": map[string]any{"type": "plain_text", "text": text, "emoji": true},
		"url":  url,
	}
	if style != "" {
		el["style"] = style
	}
	return el
}

// ActionsBlock wraps one or more button elements (see LinkButton) into
// a Slack `actions` block. Exported for cross-package use; the monitor
// builders here still render their footer CTAs as inline mrkdwn links
// in the context footer for compactness.
func ActionsBlock(elements ...map[string]any) Block {
	return Block{
		"type":     "actions",
		"elements": elements,
	}
}

// FormatDate renders a Slack <!date^...> token so each viewer sees
// their own local time. Falls back to a fixed UTC string if Slack
// can't resolve it.
func FormatDate(t time.Time) string {
	return fmt.Sprintf(`<!date^%d^{date_short_pretty} at {time}|%s>`,
		t.Unix(), t.UTC().Format("2006-01-02 15:04 UTC"))
}

// formatDowntime returns a compact human form like "1d 4h 23m" or
// "47m" or "2h 15m". Always at least one unit.
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
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}
	return strings.Join(parts, " ")
}
