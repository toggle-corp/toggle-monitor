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

// BuildSSLParent renders the initial ⚠️ SSL parent.
func BuildSSLParent(in SSLDownInput) []Block {
	return buildSSLParent(in, sslHeader(in.FriendlyName, in.DaysRemaining))
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

// BuildSSLResolveEdit produces the blocks the SSL parent is edited
// to after the cert is renewed. Preserves the original context line
// and replaces the header.
func BuildSSLResolveEdit(in SSLResolveInput) []Block {
	header := Block{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": fmt.Sprintf("✅ Certificate renewed. New expiry: %s (in %d days).",
				in.NewExpiresAt.UTC().Format("2006-01-02"),
				int(time.Until(in.NewExpiresAt).Hours()/24)),
		},
	}
	return buildSSLParent(in.SSLDownInput, header)
}

// BuildSSLResolveReply is the thread reply emitted on cert renewal.
func BuildSSLResolveReply(in SSLResolveInput) []Block {
	text := fmt.Sprintf("✅ Cert renewed. New expiry: %s.",
		FormatDate(in.NewExpiresAt))
	return []Block{section(text)}
}

func sslHeader(name string, daysRemaining int) Block {
	return Block{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": fmt.Sprintf("⚠️ %s — SSL expiring in %d days", name, daysRemaining),
		},
	}
}

func buildSSLParent(in SSLDownInput, header Block) []Block {
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
	fields := []map[string]any{
		{"type": "mrkdwn", "text": "*Expires at*\n" + FormatDate(in.ExpiresAt)},
		{"type": "mrkdwn", "text": fmt.Sprintf("*Days remaining*\n%d", in.DaysRemaining)},
	}
	if in.Issuer != "" {
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": "*Issuer*\n" + in.Issuer})
	}
	if in.Subject != "" {
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": "*Subject*\n" + in.Subject})
	}
	blocks = append(blocks, Block{"type": "section", "fields": fields})
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
