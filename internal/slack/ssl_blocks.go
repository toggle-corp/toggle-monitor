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
	Tags          []string
	URL           string
	Mentions      []string
	ExpiresAt     time.Time
	Issuer        string
	Subject       string
	DaysRemaining int
	DetailURL     string
	DetectedAt    time.Time // when the expiring state was observed; zero omits the footer date
	Banner        string    // optional top-of-body banner (used for fresh-parent fallback); "" omits
}

// BuildSSLParent renders the initial ⚠️ SSL parent as the three-block
// iA2 shape from ADR-0006: title carries name + days-remaining + URL;
// body carries Issuer (and Subject when present); footer carries
// mentions + Detected + View-details. No attachments, no header block.
func BuildSSLParent(in SSLDownInput) ParentMessage {
	title := fmt.Sprintf(":warning: *%s* — SSL expires in %dd (%s)  ·  <%s|%s>",
		in.FriendlyName, in.DaysRemaining, shortDate(in.ExpiresAt), in.URL, in.URL)
	return ParentMessage{
		Blocks: buildSSLParentBlocks(in, title, sslBodyText(in), "Detected", in.DetectedAt),
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

// BuildSSLResolveEdit produces the parent edit emitted when the cert
// is renewed. Three blocks: green title with new expiry days + short
// date, body preserves Issuer (helpful when the CA changed), footer
// prefixed with "Renewed".
func BuildSSLResolveEdit(in SSLResolveInput) ParentMessage {
	days := int(time.Until(in.NewExpiresAt).Hours() / 24)
	if days < 0 {
		days = 0
	}
	title := fmt.Sprintf(":large_green_circle: *%s* — Cert renewed (%dd expiry · %s)  ·  <%s|%s>",
		in.FriendlyName, days, shortDate(in.NewExpiresAt), in.URL, in.URL)
	return ParentMessage{
		Blocks: buildSSLParentBlocks(in.SSLDownInput, title, sslBodyText(in.SSLDownInput),
			"Renewed", in.RenewedAt),
	}
}

// BuildSSLResolveReply is the thread reply emitted on cert renewal.
func BuildSSLResolveReply(in SSLResolveInput) []Block {
	text := fmt.Sprintf(":large_green_circle: Cert renewed. New expiry: %s.",
		FormatDate(in.NewExpiresAt))
	return []Block{section(text)}
}

// buildSSLParentBlocks composes the three-block iA2 SSL parent: title
// section, body section (Issuer/Subject), footer context. Mirrors
// buildParentBlocks but with SSL-specific inputs.
func buildSSLParentBlocks(in SSLDownInput, title, body, footerPrefix string, footerTime time.Time) []Block {
	blocks := []Block{section(title)}

	if in.Banner != "" {
		blocks = append(blocks, contextBlock(in.Banner))
	}
	if body != "" {
		blocks = append(blocks, section(body))
	}
	if footer := parentFooter(in.Mentions, footerPrefix, footerTime, in.DetailURL); footer != "" {
		blocks = append(blocks, contextBlock(footer))
	}
	return blocks
}

// sslBodyText composes the Issuer/Subject body line(s). Issuer is the
// primary signal; Subject folds in when present and short enough to
// keep the body within a single readable line.
func sslBodyText(in SSLDownInput) string {
	var parts []string
	if in.Issuer != "" {
		parts = append(parts, "Issuer `"+in.Issuer+"`")
	}
	if in.Subject != "" {
		parts = append(parts, "Subject `"+in.Subject+"`")
	}
	return strings.Join(parts, "  ·  ")
}

// shortDate renders a Slack `<!date^…>` token with a short-date format
// only, e.g. "Jun 14". The fallback string is the UTC short date.
func shortDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf(`<!date^%d^{date_short_pretty}|%s>`,
		t.Unix(), t.UTC().Format("Jan 2"))
}
