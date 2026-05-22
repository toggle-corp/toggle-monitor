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

// DownInput carries everything Block-Kit builders need to render an
// uptime parent (and its eventual resolved form).
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

// BuildDownParent renders the Block Kit blocks for the initial `🔴 …
// is DOWN` parent message.
func BuildDownParent(in DownInput) []Block {
	return buildParent(in, downHeader(in.FriendlyName), nil)
}

// BuildResolveEdit renders the blocks the parent should be edited to
// when the monitor recovers. Preserves the original content (group,
// URL, mentions, fields) and only changes the header + appends a
// "Resolved at" field, per docs/design-decisions.md.
func BuildResolveEdit(in ResolveInput) []Block {
	resolvedAt := map[string]any{
		"type": "mrkdwn",
		"text": fmt.Sprintf("*Resolved at*\n%s", FormatDate(in.ResolveAt)),
	}
	return buildParent(in.DownInput, resolvedHeader(in.FriendlyName, in.Downtime), []map[string]any{resolvedAt})
}

// BuildReminderReply renders the short scannable thread reply posted
// every reminderInterval while the monitor remains down. No mentions.
func BuildReminderReply(in ReminderInput) []Block {
	text := fmt.Sprintf("⏰ Still down for %s. Last checked: %s.",
		formatDowntime(in.DownDuration), FormatDate(in.LastCheckedAt))
	if in.LastError != "" {
		text += " Last error: " + in.LastError
	}
	return []Block{section(text)}
}

// BuildResolveReply renders the thread reply emitted on resolve,
// alongside the edit applied to the parent. No mentions.
func BuildResolveReply(in ResolveInput) []Block {
	text := fmt.Sprintf("✅ Resolved at %s. Total downtime: %s.",
		FormatDate(in.ResolveAt), formatDowntime(in.Downtime))
	return []Block{section(text)}
}

// buildParent is the shared parent layout. Extra optional fields go in
// the right column of the fields section (used by BuildResolveEdit to
// append "Resolved at").
func buildParent(in DownInput, header Block, extraFields []map[string]any) []Block {
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

	// Fields: status, failure time, last error (+ optional resolved-at).
	fields := []map[string]any{
		{"type": "mrkdwn", "text": fmt.Sprintf("*Status*\n%d %s", in.StatusCode, in.StatusText)},
		{"type": "mrkdwn", "text": fmt.Sprintf("*Failure time*\n%s", FormatDate(in.FailureAt))},
	}
	if in.LastError != "" {
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": "*Last error*\n" + in.LastError})
	}
	fields = append(fields, extraFields...)
	blocks = append(blocks, Block{"type": "section", "fields": fields})

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
		"text": map[string]any{"type": "plain_text", "text": fmt.Sprintf("🔴 %s is DOWN", name)},
	}
}

func resolvedHeader(name string, downtime time.Duration) Block {
	return Block{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": fmt.Sprintf("✅ %s is UP (was down for %s)", name, formatDowntime(downtime)),
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
