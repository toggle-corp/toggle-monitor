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
	Group        string
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

// BuildDownParent renders the initial 🔴 parent for an uptime
// incident. Returns a slice of attachments so the message gets a
// red left-edge stripe.
func BuildDownParent(in DownInput) []Attachment {
	return []Attachment{{
		Color: ColorDown,
		Blocks: buildParentBlocks(in,
			bigHeader(":red_circle: "+in.FriendlyName+" is DOWN"),
			nil,
			"Detected", in.FailureAt),
	}}
}

// BuildResolveEdit renders the attachments the parent should be
// edited to when the monitor recovers. The header swaps to
// :large_green_circle:, the color stripe swaps to green, a Duration
// line is appended to the body, and the footer rewrites to
// "Resolved <date>".
func BuildResolveEdit(in ResolveInput) []Attachment {
	return []Attachment{{
		Color: ColorResolved,
		Blocks: buildParentBlocks(in.DownInput,
			bigHeader(fmt.Sprintf(":large_green_circle: %s is UP (was down for %s)",
				in.FriendlyName, formatDowntime(in.Downtime))),
			[]detailLine{{Label: "Duration", Value: "`" + formatDowntime(in.Downtime) + "`"}},
			"Resolved", in.ResolveAt),
	}}
}

// BuildReminderReply renders the short scannable thread reply posted
// every reminderInterval while the monitor remains down. No mentions.
// Stays inside a thread, so no color stripe.
func BuildReminderReply(in ReminderInput) []Block {
	text := fmt.Sprintf("⏰ Still down for `%s`. Last checked: %s.",
		formatDowntime(in.DownDuration), FormatDate(in.LastCheckedAt))
	if in.LastError != "" {
		text += " Last error: `" + in.LastError + "`"
	}
	return []Block{section(text)}
}

// BuildResolveReply renders the thread reply emitted on resolve.
// Stays inside a thread, so no color stripe.
func BuildResolveReply(in ResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Resolved at %s. Total downtime: `%s`.",
		FormatDate(in.ResolveAt), formatDowntime(in.Downtime))
	return []Block{section(text)}
}

// buildParentBlocks is the shared parent layout in the Uptime Robot
// style: big header, optional mentions section, single mrkdwn body
// with `*Label:* value` rows, optional inline response body, and a
// small dim context-block footer carrying the timestamp + View
// details link.
//
// `extra` lines are appended to the body (used by BuildResolveEdit
// to surface Duration). footerPrefix + footerTime drive the
// "Detected <date>" / "Resolved <date>" line; either may be empty.
func buildParentBlocks(in DownInput, header Block, extra []detailLine, footerPrefix string, footerTime time.Time) []Block {
	blocks := []Block{header}

	// Mentions block: parent only, never on reminders/resolves. Stays
	// in a section because context-block mentions don't reliably fire
	// notifications.
	if len(in.Mentions) > 0 {
		blocks = append(blocks, section(strings.Join(in.Mentions, " ")))
	}

	// UR-style body: one *Label:* value per line. Group + URL fold in
	// here so the message has a single visual cluster.
	var lines []string
	if in.URL != "" {
		lines = append(lines, "*Monitor URL:* "+in.URL)
	}
	if in.StatusCode != 0 || in.StatusText != "" {
		lines = append(lines, fmt.Sprintf("*Reason:* `%d %s`", in.StatusCode, in.StatusText))
	}
	if in.LastError != "" {
		lines = append(lines, "*Error:* `"+in.LastError+"`")
	}
	if in.Group != "" {
		lines = append(lines, "*Group:* "+in.Group)
	}
	for _, e := range extra {
		lines = append(lines, "*"+e.Label+":* "+e.Value)
	}
	if len(lines) > 0 {
		blocks = append(blocks, section(strings.Join(lines, "\n")))
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

	// Footer: smaller, dimmer (Slack context block). Italics on the
	// timestamp half mirrors Uptime Robot.
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
