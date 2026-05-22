package slack

import (
	"context"
	"fmt"
	"strings"
)

// RemovedInput carries the data for the non-threaded "monitor removed"
// warning post.
type RemovedInput struct {
	FriendlyName string
	GroupSlug    string
	URL          string
	HTTPMethod   string
	Source       string // "static config" | "k8s ingress (ns/name)"
	Reason       string // e.g. "removed from config", "k8s ingress removed"
	DetailURL    string // empty omits the [View details] button
}

// BuildRemovedWarning renders the non-threaded warning that's posted
// to the monitor's last-known Slack channel when the monitor
// disappears from config or from the cluster. Amber stripe, one
// detail per line. No mentions.
func BuildRemovedWarning(in RemovedInput) []Attachment {
	header := Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  fmt.Sprintf(":warning: Monitor removed: %s", in.FriendlyName),
			"emoji": true,
		},
	}
	lines := []string{
		"*Group:* " + in.GroupSlug,
		"*Method:* " + in.HTTPMethod,
		"*URL:* " + in.URL,
		"*Source:* " + in.Source,
		"*Reason:* " + in.Reason,
	}
	blocks := []Block{header, section(strings.Join(lines, "\n"))}
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
	return []Attachment{{Color: ColorRemoved, Blocks: blocks}}
}

// BuildRemovedClose renders the in-thread reply posted when a removed
// monitor had an open uptime incident. No mentions.
func BuildRemovedClose() []Block {
	return []Block{section("ℹ️ Monitor was removed. Closing incident.")}
}

// BuildRemovedResolveEdit produces the parent-edit attachments for a
// removed monitor's uptime thread. Green stripe + :large_green_circle:
// header, with a "Resolved: monitor removed" detail line.
func BuildRemovedResolveEdit(in DownInput) []Attachment {
	header := Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  fmt.Sprintf(":large_green_circle: %s — Resolved (monitor removed)", in.FriendlyName),
			"emoji": true,
		},
	}
	return []Attachment{{
		Color: ColorResolved,
		Blocks: buildParentBlocks(in, header, []detailLine{
			{Label: "Resolved", Value: "monitor removed"},
		}),
	}}
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
			Group:        in.GroupSlug,
			URL:          in.URL,
			StatusCode:   in.StatusCode,
			StatusText:   in.StatusText,
			FailureAt:    in.OpenedAt,
			LastError:    in.LastError,
			DetailURL:    n.detailURL(in.Slug),
		}
		if err := n.client.UpdateMessage(ctx, ch.Token, UpdateMessageInput{
			ChannelID:   in.UptimeThreadChannel,
			TS:          in.UptimeThreadTS,
			Attachments: BuildRemovedResolveEdit(downIn),
		}); err != nil {
			n.log.Warn("removal: edit parent", "monitor", in.Slug, "error", err)
		}
	}

	// Non-threaded warning, always (so monitor-was-removed is visible
	// even for monitors that were up at the time).
	warning := BuildRemovedWarning(RemovedInput{
		FriendlyName: in.FriendlyName,
		GroupSlug:    in.GroupSlug,
		URL:          in.URL,
		HTTPMethod:   "GET", // populated by the caller once we carry it on MonitorView; safe default for now
		Source:       source,
		Reason:       reason,
		DetailURL:    n.detailURL(in.Slug),
	})
	if _, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Attachments: warning,
	}); err != nil {
		n.log.Warn("removal: post warning", "monitor", in.Slug, "error", err)
	}
}
