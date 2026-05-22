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
	DetectedAt    time.Time // when the expiring state was observed; zero omits the footer date
}

// BuildSSLParent renders the initial ⚠️ SSL parent. Amber stripe.
// Header sits outside the attachment so the stripe wraps only the
// quieter body.
func BuildSSLParent(in SSLDownInput) ParentMessage {
	return ParentMessage{
		Blocks: []Block{bigHeader(fmt.Sprintf(":warning: %s — SSL expiring in %d days",
			in.FriendlyName, in.DaysRemaining))},
		Attachments: []Attachment{{
			Color:  ColorSSLWarn,
			Blocks: buildSSLParentBlocks(in, nil, "Detected", in.DetectedAt),
		}},
	}
}

// BuildSSLReminderReply renders the cadence reminder thread reply.
// One *Label:* value per line for consistency with the parent body.
func BuildSSLReminderReply(in SSLDownInput) []Block {
	lines := []string{
		fmt.Sprintf("⚠️ *Days remaining:* `%d`", in.DaysRemaining),
	}
	if !in.ExpiresAt.IsZero() {
		lines = append(lines, "*Expires:* "+FormatDate(in.ExpiresAt))
	}
	lines = append(lines, "_Renewal needed._")
	return []Block{section(strings.Join(lines, "\n"))}
}

// SSLResolveInput carries the data needed for the resolve-edit
// (header swap) and the resolve thread reply.
type SSLResolveInput struct {
	SSLDownInput
	NewExpiresAt time.Time
	RenewedAt    time.Time // when renewal was observed; zero omits the footer date
}

// BuildSSLResolveEdit produces the attachments the SSL parent is
// edited to after the cert is renewed. Green stripe, header swap, a
// "New expiry" line appended to the body, and the footer rewrites
// to "Renewed <date>".
func BuildSSLResolveEdit(in SSLResolveInput) ParentMessage {
	days := int(time.Until(in.NewExpiresAt).Hours() / 24)
	return ParentMessage{
		Blocks: []Block{bigHeader(fmt.Sprintf(":large_green_circle: %s — Certificate renewed (in %d days expiry)",
			in.FriendlyName, days))},
		Attachments: []Attachment{{
			Color: ColorResolved,
			Blocks: buildSSLParentBlocks(in.SSLDownInput,
				[]detailLine{{Label: "New expiry", Value: FormatDate(in.NewExpiresAt)}},
				"Renewed", in.RenewedAt),
		}},
	}
}

// BuildSSLResolveReply is the thread reply emitted on cert renewal.
func BuildSSLResolveReply(in SSLResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Cert renewed. New expiry: %s.",
		FormatDate(in.NewExpiresAt))
	return []Block{section(text)}
}

// buildSSLParentBlocks mirrors buildParentBlocks: a single context
// block carrying mentions + UR-style fields, then optional footer.
// No header — that's a top-level block outside this attachment.
func buildSSLParentBlocks(in SSLDownInput, extra []detailLine, footerPrefix string, footerTime time.Time) []Block {
	var blocks []Block

	var lines []string
	if len(in.Mentions) > 0 {
		lines = append(lines, "*CC:* "+strings.Join(in.Mentions, " "))
	}
	if in.URL != "" {
		lines = append(lines, "*Monitor URL:* "+in.URL)
	}
	lines = append(lines, "*Expires:* "+FormatDate(in.ExpiresAt))
	lines = append(lines, fmt.Sprintf("*Days remaining:* `%d`", in.DaysRemaining))
	if in.Issuer != "" {
		lines = append(lines, "*Issuer:* `"+in.Issuer+"`")
	}
	if in.Subject != "" {
		lines = append(lines, "*Subject:* `"+in.Subject+"`")
	}
	if in.Group != "" {
		lines = append(lines, "*Group:* `"+in.Group+"`")
	}
	for _, e := range extra {
		lines = append(lines, "*"+e.Label+":* "+e.Value)
	}
	blocks = append(blocks, contextBlock(strings.Join(lines, "\n")))

	if footer := footerLine(footerPrefix, footerTime, in.DetailURL); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	return blocks
}
