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
	DetailURL    string // empty omits the [View details] button
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

// detailLine is one "*Label:* value" row in the parent's body section.
type detailLine struct {
	Label, Value string
}

// BuildDownParent renders the initial 🔴 parent for an uptime
// incident. Returns a slice of attachments so the message gets a
// red left-edge stripe.
func BuildDownParent(in DownInput) []Attachment {
	return []Attachment{{
		Color:  ColorDown,
		Blocks: buildParentBlocks(in, downHeader(in.FriendlyName), nil),
	}}
}

// BuildResolveEdit renders the attachments the parent should be
// edited to when the monitor recovers. The header swaps to
// :large_green_circle:, the color stripe swaps to green, and a
// "Resolved" line is appended to the detail block.
func BuildResolveEdit(in ResolveInput) []Attachment {
	return []Attachment{{
		Color: ColorResolved,
		Blocks: buildParentBlocks(in.DownInput, resolvedHeader(in.FriendlyName, in.Downtime),
			[]detailLine{{Label: "Resolved", Value: FormatDate(in.ResolveAt)}}),
	}}
}

// BuildReminderReply renders the short scannable thread reply posted
// every reminderInterval while the monitor remains down. No mentions.
// Stays inside a thread, so no color stripe.
func BuildReminderReply(in ReminderInput) []Block {
	text := fmt.Sprintf("⏰ Still down for %s. Last checked: %s.",
		formatDowntime(in.DownDuration), FormatDate(in.LastCheckedAt))
	if in.LastError != "" {
		text += " Last error: " + in.LastError
	}
	return []Block{section(text)}
}

// BuildResolveReply renders the thread reply emitted on resolve.
// Stays inside a thread, so no color stripe.
func BuildResolveReply(in ResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Resolved at %s. Total downtime: %s.",
		FormatDate(in.ResolveAt), formatDowntime(in.Downtime))
	return []Block{section(text)}
}

// buildParentBlocks is the shared parent layout. Extra detail lines
// are appended to the single-section bold-label body (used by
// BuildResolveEdit to append "Resolved").
func buildParentBlocks(in DownInput, header Block, extra []detailLine) []Block {
	blocks := []Block{header}

	// Context line: group · URL.
	blocks = append(blocks, Block{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": in.Group + " · " + in.URL},
		},
	})

	// Mentions block: parent only, never on reminders/resolves.
	if len(in.Mentions) > 0 {
		blocks = append(blocks, section(strings.Join(in.Mentions, " ")))
	}

	// Single mrkdwn section: one *Label:* value per line.
	lines := []string{
		fmt.Sprintf("*Status:* %d %s", in.StatusCode, in.StatusText),
		"*Failure:* " + FormatDate(in.FailureAt),
	}
	if in.LastError != "" {
		lines = append(lines, "*Error:* "+in.LastError)
	}
	for _, e := range extra {
		lines = append(lines, "*"+e.Label+":* "+e.Value)
	}
	blocks = append(blocks, section(strings.Join(lines, "\n")))

	// Optional inline response body.
	if in.ResponseBody != "" && len(in.ResponseBody) <= in.BodyMaxChars {
		blocks = append(blocks, section("```\n"+in.ResponseBody+"\n```"))
	}

	// Optional [View details] button.
	if in.DetailURL != "" {
		blocks = append(blocks, Block{
			"type": "actions",
			"elements": []map[string]any{{
				"type": "button",
				"text": map[string]any{"type": "plain_text", "text": "View details"},
				"url":  in.DetailURL,
			}},
		})
	}

	return blocks
}

func downHeader(name string) Block {
	return Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  fmt.Sprintf(":red_circle: %s is DOWN", name),
			"emoji": true,
		},
	}
}

func resolvedHeader(name string, downtime time.Duration) Block {
	return Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  fmt.Sprintf(":large_green_circle: %s is UP (was down for %s)", name, formatDowntime(downtime)),
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
