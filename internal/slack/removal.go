package slack

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RemovedInput carries the data for the non-threaded "monitor removed"
// warning post.
type RemovedInput struct {
	FriendlyName string
	Tags         []string
	URL          string
	HTTPMethod   string
	Source       string // "static config" | "k8s ingress (ns/name)"
	Reason       string // e.g. "removed from config", "k8s ingress removed"
	DetailURL    string // empty omits the [View details] button
}

// BuildRemovedWarning renders the non-threaded warning that's posted
// to the monitor's last-known Slack channel when the monitor
// disappears from config or from the cluster. Amber stripe wraps the
// body only — the header rides outside it.
func BuildRemovedWarning(in RemovedInput) ParentMessage {
	lines := []string{
		"*Monitor URL:* " + in.URL,
		"*Method:* `" + in.HTTPMethod + "`",
		"*Reason:* `" + in.Reason + "`",
		"*Source:* " + in.Source,
	}
	if len(in.Tags) > 0 {
		lines = append(lines, "*Tags:* `"+strings.Join(in.Tags, "`, `")+"`")
	}
	blocks := []Block{contextBlock(strings.Join(lines, "\n"))}
	if footer := footerLine("", time.Time{}, in.DetailURL); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	return ParentMessage{
		Blocks:      []Block{bigHeader(fmt.Sprintf(":warning: Monitor removed: %s", in.FriendlyName))},
		Attachments: []Attachment{{Color: ColorRemoved, Blocks: blocks}},
	}
}

// BuildRemovedClose renders the in-thread reply posted when a removed
// monitor had an open uptime incident. No mentions.
func BuildRemovedClose() []Block {
	return []Block{section("ℹ️ Monitor was removed. Closing incident.")}
}

// BuildRemovedResolveEdit produces the parent-edit attachments for a
// removed monitor's uptime thread. Green stripe + :large_green_circle:
// header (outside the stripe), with a "Resolved: monitor removed"
// detail line (no timestamp — the removal isn't a real recovery
// moment). This path keeps the legacy attachment-wrapped labeled-row
// shape; ADR-0006 scopes only the live parent renderers and the
// removal flow stays as-is until a future ADR revisits it.
func BuildRemovedResolveEdit(in DownInput) ParentMessage {
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
	lines = append(lines, "*Resolved:* monitor removed")

	blocks := []Block{contextBlock(strings.Join(lines, "\n"))}
	if in.DetailURL != "" {
		blocks = append(blocks, contextBlock(fmt.Sprintf("<%s|View details>", in.DetailURL)))
	}
	return ParentMessage{
		Blocks:      []Block{bigHeader(fmt.Sprintf(":large_green_circle: %s — Resolved (monitor removed)", in.FriendlyName))},
		Attachments: []Attachment{{Color: ColorResolved, Blocks: blocks}},
	}
}

// NotifyRemoved is the public dispatch hook the lifecycle calls per
// soft-deleted monitor. It performs whichever of the three actions
// are applicable:
//  1. Post a thread reply + edit the parent IF the monitor had an
//     open uptime incident (uptime thread refs present).
//  2. Post a standalone "Monitor removed" warning to the channel.
//
// All three sub-calls are best-effort: a Slack outage here logs and
// continues — the DB soft-delete has already committed.
func (n *Notifier) NotifyRemoved(ctx context.Context, channelSlug string, in MonitorView, reason, source string) {
	ch, ok := n.channels(channelSlug)
	if !ok {
		n.log.Warn("removal: channel slug not registered", "monitor", in.Slug, "slug", channelSlug)
		return
	}

	// In-thread closeout if the monitor was currently down.
	if in.UptimeThreadTS != "" {
		closeoutBlocks := BuildRemovedClose()
		if _, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID: in.UptimeThreadChannel,
			ThreadTS:  in.UptimeThreadTS,
			Blocks:    closeoutBlocks,
		}); err != nil {
			n.log.Warn("removal: post in-thread closeout", "monitor", in.Slug, "error", err)
		}
		// Edit the parent to ✅ Resolved (monitor removed).
		downIn := DownInput{
			FriendlyName: in.FriendlyName,
			Tags:         in.Tags,
			URL:          in.URL,
			StatusCode:   in.StatusCode,
			StatusText:   in.StatusText,
			FailureAt:    in.OpenedAt,
			LastError:    in.LastError,
			DetailURL:    n.detailURL(in.Slug),
		}
		resolveMsg := BuildRemovedResolveEdit(downIn)
		if err := n.client.UpdateMessage(ctx, ch.Token, UpdateMessageInput{
			ChannelID:   in.UptimeThreadChannel,
			TS:          in.UptimeThreadTS,
			Blocks:      resolveMsg.Blocks,
			Attachments: resolveMsg.Attachments,
		}); err != nil {
			n.log.Warn("removal: edit parent", "monitor", in.Slug, "error", err)
		}
	}

	// Non-threaded warning, always (so monitor-was-removed is visible
	// even for monitors that were up at the time).
	warning := BuildRemovedWarning(RemovedInput{
		FriendlyName: in.FriendlyName,
		Tags:         in.Tags,
		URL:          in.URL,
		HTTPMethod:   "GET", // populated by the caller once we carry it on MonitorView; safe default for now
		Source:       source,
		Reason:       reason,
		DetailURL:    n.detailURL(in.Slug),
	})
	if _, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      warning.Blocks,
		Attachments: warning.Attachments,
	}); err != nil {
		n.log.Warn("removal: post warning", "monitor", in.Slug, "error", err)
	}
}
