package slack

import (
	"fmt"
	"strings"
	"time"
)

// SSLDownInput carries data for the SSL parent message and its
// resolve-edit counterpart.
type SSLDownInput struct {
	FriendlyName  string
	Group         string
	URL           string
	Mentions      []string
	ExpiresAt     time.Time
	Issuer        string
	Subject       string
	DaysRemaining int
	DetailURL     string
}

// BuildSSLParent renders the initial ⚠️ SSL parent. Amber stripe.
func BuildSSLParent(in SSLDownInput) []Attachment {
	return []Attachment{{
		Color:  ColorSSLWarn,
		Blocks: buildSSLParentBlocks(in, sslHeader(in.FriendlyName, in.DaysRemaining), nil),
	}}
}

// BuildSSLReminderReply renders the cadence reminder thread reply.
func BuildSSLReminderReply(in SSLDownInput) []Block {
	text := fmt.Sprintf("⚠️ Still expiring — %d days remaining. Renewal needed.", in.DaysRemaining)
	return []Block{section(text)}
}

// SSLResolveInput carries the data needed for the resolve-edit
// (header swap) and the resolve thread reply.
type SSLResolveInput struct {
	SSLDownInput
	NewExpiresAt time.Time
}

// BuildSSLResolveEdit produces the attachments the SSL parent is
// edited to after the cert is renewed. Green stripe, header swap, and
// a "Renewed" line appended to the detail block.
func BuildSSLResolveEdit(in SSLResolveInput) []Attachment {
	header := Block{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": fmt.Sprintf(":large_green_circle: %s — Certificate renewed (in %d days expiry)",
				in.FriendlyName, int(time.Until(in.NewExpiresAt).Hours()/24)),
			"emoji": true,
		},
	}
	return []Attachment{{
		Color: ColorResolved,
		Blocks: buildSSLParentBlocks(in.SSLDownInput, header, []detailLine{
			{Label: "New expiry", Value: FormatDate(in.NewExpiresAt)},
		}),
	}}
}

// BuildSSLResolveReply is the thread reply emitted on cert renewal.
func BuildSSLResolveReply(in SSLResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Cert renewed. New expiry: %s.",
		FormatDate(in.NewExpiresAt))
	return []Block{section(text)}
}

func sslHeader(name string, daysRemaining int) Block {
	return Block{
		"type": "header",
		"text": map[string]any{
			"type":  "plain_text",
			"text":  fmt.Sprintf(":warning: %s — SSL expiring in %d days", name, daysRemaining),
			"emoji": true,
		},
	}
}

// buildSSLParentBlocks mirrors buildParentBlocks: header + context +
// optional mentions + one bold-label line per detail, plus an
// optional [View details] button.
func buildSSLParentBlocks(in SSLDownInput, header Block, extra []detailLine) []Block {
	blocks := []Block{header}
	blocks = append(blocks, Block{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": in.Group + " · " + in.URL},
		},
	})
	if len(in.Mentions) > 0 {
		blocks = append(blocks, section(strings.Join(in.Mentions, " ")))
	}

	lines := []string{
		"*Expires at:* " + FormatDate(in.ExpiresAt),
		fmt.Sprintf("*Days remaining:* %d", in.DaysRemaining),
	}
	if in.Issuer != "" {
		lines = append(lines, "*Issuer:* "+in.Issuer)
	}
	if in.Subject != "" {
		lines = append(lines, "*Subject:* "+in.Subject)
	}
	for _, e := range extra {
		lines = append(lines, "*"+e.Label+":* "+e.Value)
	}
	blocks = append(blocks, section(strings.Join(lines, "\n")))

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
