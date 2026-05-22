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
func BuildSSLParent(in SSLDownInput) []Attachment {
	return []Attachment{{
		Color: ColorSSLWarn,
		Blocks: buildSSLParentBlocks(in,
			bigHeader(fmt.Sprintf(":warning: %s — SSL expiring in %d days", in.FriendlyName, in.DaysRemaining)),
			nil,
			"Detected", in.DetectedAt),
	}}
}

// BuildSSLReminderReply renders the cadence reminder thread reply.
func BuildSSLReminderReply(in SSLDownInput) []Block {
	text := fmt.Sprintf("⚠️ Still expiring — `%d` days remaining. Renewal needed.", in.DaysRemaining)
	return []Block{section(text)}
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
func BuildSSLResolveEdit(in SSLResolveInput) []Attachment {
	days := int(time.Until(in.NewExpiresAt).Hours() / 24)
	header := bigHeader(fmt.Sprintf(":large_green_circle: %s — Certificate renewed (in %d days expiry)", in.FriendlyName, days))
	return []Attachment{{
		Color: ColorResolved,
		Blocks: buildSSLParentBlocks(in.SSLDownInput, header,
			[]detailLine{{Label: "New expiry", Value: FormatDate(in.NewExpiresAt)}},
			"Renewed", in.RenewedAt),
	}}
}

// BuildSSLResolveReply is the thread reply emitted on cert renewal.
func BuildSSLResolveReply(in SSLResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Cert renewed. New expiry: %s.",
		FormatDate(in.NewExpiresAt))
	return []Block{section(text)}
}

// buildSSLParentBlocks mirrors buildParentBlocks: big header +
// optional mentions + UR-style body in a section + small dim
// context-block footer. Group + URL fold into the body so the
// message has a single visual cluster.
func buildSSLParentBlocks(in SSLDownInput, header Block, extra []detailLine, footerPrefix string, footerTime time.Time) []Block {
	blocks := []Block{header}
	if len(in.Mentions) > 0 {
		blocks = append(blocks, section(strings.Join(in.Mentions, " ")))
	}

	var lines []string
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
		lines = append(lines, "*Group:* "+in.Group)
	}
	for _, e := range extra {
		lines = append(lines, "*"+e.Label+":* "+e.Value)
	}
	// Body in a context block: smaller + dimmer than a regular section.
	blocks = append(blocks, contextBlock(strings.Join(lines, "\n")))

	if footer := footerLine(footerPrefix, footerTime, in.DetailURL); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	return blocks
}
