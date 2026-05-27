package slack

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultDigestMaxRows caps how many monitor rows the digest body lists
// before collapsing the remainder into a "…and N more" tail, keeping
// the message under Slack's per-block text limit during a large outage.
const DefaultDigestMaxRows = 40

// DigestRowClass is how one monitor renders in the digest body.
type DigestRowClass string

const (
	// RowActive — currently down (or recovering): un-struck.
	RowActive DigestRowClass = "active"
	// RowRecovered — recovered: struck through, kept visible.
	RowRecovered DigestRowClass = "recovered"
	// RowPaused — pulled out by dependsOn push-propagation.
	RowPaused DigestRowClass = "paused"
)

// DigestRow is one monitor line in the digest body.
type DigestRow struct {
	Name      string // friendly name
	Class     DigestRowClass
	DetailURL string // empty omits the link
}

// DigestInput carries everything the per-channel digest parent needs.
// It is the coalesced analogue of DownInput: one message standing in for
// many monitors.
type DigestInput struct {
	Down      int
	Recovered int
	Total     int
	OpenedAt  time.Time
	Rows      []DigestRow
	Mentions  []string      // raw markup; pass on open + reminder, omit on edits
	Note      string        // e.g. cascading-effect line; "" omits
	Banner    string        // late-notice banner; "" omits
	Closed    bool          // render the all-clear/green final state
	Downtime  time.Duration // set when Closed: incident span
	MaxRows   int           // 0 → DefaultDigestMaxRows
}

// BuildDigestParent renders the living per-channel digest message: a
// scoreboard header plus one row per monitor (struck-through once
// recovered, kept visible until the group closes). The header sits
// outside the color attachment, matching BuildDownParent; the stripe is
// red while anything is down and green once the incident closes.
func BuildDigestParent(in DigestInput) ParentMessage {
	color := ColorDown
	header := fmt.Sprintf(":rotating_light: %d down · %d recovered (of %d)",
		in.Down, in.Recovered, in.Total)
	footerPrefix := "Opened"
	footerTime := in.OpenedAt
	if in.Closed {
		color = ColorResolved
		header = fmt.Sprintf(":large_green_circle: All clear — %d recovered (was down for %s)",
			in.Recovered, formatDowntime(in.Downtime))
		footerPrefix = "Resolved"
		footerTime = in.OpenedAt.Add(in.Downtime)
	}
	return ParentMessage{
		Blocks: []Block{bigHeader(header)},
		Attachments: []Attachment{{
			Color:  color,
			Blocks: buildDigestBlocks(in, footerPrefix, footerTime),
		}},
	}
}

func buildDigestBlocks(in DigestInput, footerPrefix string, footerTime time.Time) []Block {
	var blocks []Block

	if in.Banner != "" {
		blocks = append(blocks, contextBlock(in.Banner))
	}
	if len(in.Mentions) > 0 {
		blocks = append(blocks, contextBlock("*CC:* "+strings.Join(in.Mentions, " ")))
	}

	if body := renderDigestRows(in.Rows, in.MaxRows); body != "" {
		blocks = append(blocks, section(body))
	}
	if in.Note != "" {
		blocks = append(blocks, contextBlock(in.Note))
	}
	if footer := footerLine(footerPrefix, footerTime, ""); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	return blocks
}

// renderDigestRows renders the per-monitor lines, capped at maxRows with
// a "…and N more" tail. Active rows sort first (what's still broken),
// then recovered, then paused; within a class, alphabetical.
func renderDigestRows(rows []DigestRow, maxRows int) string {
	if len(rows) == 0 {
		return ""
	}
	if maxRows <= 0 {
		maxRows = DefaultDigestMaxRows
	}
	classRank := map[DigestRowClass]int{RowActive: 0, RowRecovered: 1, RowPaused: 2}
	sorted := append([]DigestRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if classRank[sorted[i].Class] != classRank[sorted[j].Class] {
			return classRank[sorted[i].Class] < classRank[sorted[j].Class]
		}
		return sorted[i].Name < sorted[j].Name
	})

	shown := sorted
	extra := 0
	if len(sorted) > maxRows {
		shown = sorted[:maxRows]
		extra = len(sorted) - maxRows
	}
	lines := make([]string, 0, len(shown)+1)
	for _, r := range shown {
		lines = append(lines, digestRowLine(r))
	}
	if extra > 0 {
		lines = append(lines, fmt.Sprintf("_…and %d more_", extra))
	}
	return strings.Join(lines, "\n")
}

func digestRowLine(r DigestRow) string {
	name := r.Name
	if r.DetailURL != "" {
		name = fmt.Sprintf("<%s|%s>", r.DetailURL, r.Name)
	}
	switch r.Class {
	case RowRecovered:
		return "✅ ~" + name + "~"
	case RowPaused:
		return "⏸ " + name + " _(paused — upstream incident)_"
	default:
		return "🔴 " + name
	}
}

// DigestDeltaInput carries the batched membership changes for one
// heartbeat's threaded reply. Each slice is a list of friendly names;
// Mentions are the (deduplicated) owners of the *newly down* monitors
// only — recoveries/flaps of already-known monitors don't re-ping.
type DigestDeltaInput struct {
	NewlyDown []string
	Recovered []string
	Flapped   []string
	Paused    []string
	Mentions  []string
}

// BuildDigestDelta renders the heartbeat thread reply summarizing what
// changed since the last flush — one reply, never one per event.
func BuildDigestDelta(in DigestDeltaInput) []Block {
	var lines []string
	if len(in.Mentions) > 0 {
		lines = append(lines, "*CC:* "+strings.Join(in.Mentions, " "))
	}
	if len(in.NewlyDown) > 0 {
		lines = append(lines, fmt.Sprintf("📉 *+%d down:* %s", len(in.NewlyDown), backticked(in.NewlyDown)))
	}
	if len(in.Recovered) > 0 {
		lines = append(lines, fmt.Sprintf("✅ *%d recovered:* %s", len(in.Recovered), backticked(in.Recovered)))
	}
	if len(in.Flapped) > 0 {
		lines = append(lines, fmt.Sprintf("🔁 *%d flapped:* %s", len(in.Flapped), backticked(in.Flapped)))
	}
	if len(in.Paused) > 0 {
		lines = append(lines, fmt.Sprintf("⏸ *%d paused* (rolled into upstream incident): %s",
			len(in.Paused), backticked(in.Paused)))
	}
	if len(lines) == 0 {
		return nil
	}
	return []Block{section(strings.Join(lines, "\n"))}
}

// DigestReminderInput carries the still-down nag content.
type DigestReminderInput struct {
	DownCount    int
	DownDuration time.Duration // since the group opened
	Mentions     []string      // re-ping the union of down owners
}

// BuildDigestReminderReply renders the repeat_interval "still down"
// reminder as a thread reply, re-pinging the union of down owners.
func BuildDigestReminderReply(in DigestReminderInput) []Block {
	var lines []string
	if len(in.Mentions) > 0 {
		lines = append(lines, "*CC:* "+strings.Join(in.Mentions, " "))
	}
	lines = append(lines, fmt.Sprintf("⏰ *Still down after %s:* %d service(s)",
		formatDowntime(in.DownDuration), in.DownCount))
	return []Block{section(strings.Join(lines, "\n"))}
}

// BuildDigestCloseReply renders the thread reply posted when the group
// closes (every member recovered).
func BuildDigestCloseReply(downtime time.Duration, recovered int) []Block {
	return []Block{section(fmt.Sprintf(
		":large_green_circle: All clear — %d service(s) recovered. Incident lasted `%s`.",
		recovered, formatDowntime(downtime)))}
}

func backticked(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "`" + n + "`"
	}
	return strings.Join(parts, ", ")
}
