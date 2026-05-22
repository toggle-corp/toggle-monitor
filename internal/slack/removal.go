package slack

import (
	"context"
	"fmt"
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
// disappears from config or from the cluster. No mentions.
func BuildRemovedWarning(in RemovedInput) []Block {
	header := Block{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": fmt.Sprintf("⚠️ Monitor removed: %s", in.FriendlyName),
		},
	}
	fields := []map[string]any{
		{"type": "mrkdwn", "text": "*Group*\n" + in.GroupSlug},
		{"type": "mrkdwn", "text": "*Method*\n" + in.HTTPMethod},
		{"type": "mrkdwn", "text": "*URL*\n" + in.URL},
		{"type": "mrkdwn", "text": "*Source*\n" + in.Source},
		{"type": "mrkdwn", "text": "*Reason*\n" + in.Reason},
	}
	blocks := []Block{
		header,
		{"type": "section", "fields": fields},
	}
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

// BuildRemovedClose renders the in-thread reply posted when a removed
// monitor had an open uptime incident. No mentions.
func BuildRemovedClose() []Block {
	return []Block{section("ℹ️ Monitor was removed. Closing incident.")}
}

// BuildRemovedResolveEdit produces the parent-edit blocks for a
// removed monitor's uptime thread. Same shape as the normal resolve
// edit but with a "resolved (monitor removed)" header.
func BuildRemovedResolveEdit(in DownInput) []Block {
	header := Block{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": fmt.Sprintf("✅ %s — Resolved (monitor removed)", in.FriendlyName),
		},
	}
	return buildParent(in, header, []map[string]any{
		{"type": "mrkdwn", "text": "*Resolved at*\nmonitor removed"},
	})
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
			ChannelID: in.UptimeThreadChannel,
			TS:        in.UptimeThreadTS,
			Blocks:    BuildRemovedResolveEdit(downIn),
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
		ChannelID: ch.ID,
		Blocks:    warning,
	}); err != nil {
		n.log.Warn("removal: post warning", "monitor", in.Slug, "error", err)
	}
}
