package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

var sslDetected = time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)
var sslExpiry = time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)

func sslOpenInput() slack.SSLDownInput {
	return slack.SSLDownInput{
		FriendlyName:  "api.example.test",
		Tags:          []string{"prod"},
		URL:           "https://api.example.test",
		Mentions:      []string{"<@U123ABC>"},
		ExpiresAt:     sslExpiry,
		Issuer:        "Let's Encrypt",
		DaysRemaining: 7,
		DetailURL:     "https://monitor.internal/monitor/api-ssl",
		DetectedAt:    sslDetected,
	}
}

// -- BuildSSLParent ---------------------------------------------------

func TestBuildSSLParent_threeBlockShape(t *testing.T) {
	out := slack.BuildSSLParent(sslOpenInput())

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	if hasBlockType(out.Blocks, "header") {
		t.Error("must not emit a header block (blocks-only contract)")
	}
	if hasBlockType(out.Blocks, "actions") {
		t.Error("must not emit an actions block")
	}

	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected at least 2 section blocks (title + body), got %d", len(sections))
	}
	title := sections[0]
	body := sections[1]

	// Title — warning emoji, bold friendly name, "SSL expires in 7d",
	// short date in parens, URL after middle dot.
	for _, want := range []string{
		":warning:",
		"*api.example.test*",
		"SSL expires in 7d",
		"<https://api.example.test|https://api.example.test>",
	} {
		if !strings.Contains(title, want) {
			t.Errorf("title missing %q in %q", want, title)
		}
	}

	// Body — Issuer rendered as inline code, no labeled rows.
	if !strings.Contains(body, "Issuer `Let's Encrypt`") {
		t.Errorf("body missing Issuer line: %q", body)
	}

	// Footer — mention, Detected, View-details.
	contexts := blockTexts(t, out.Blocks, "context")
	if len(contexts) == 0 {
		t.Fatal("expected footer context block")
	}
	footer := contexts[len(contexts)-1]
	for _, want := range []string{
		"<@U123ABC>",
		"_Detected ",
		"<https://monitor.internal/monitor/api-ssl|View details>",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q in %q", want, footer)
		}
	}

	// Legacy labeled rows must not appear.
	s := dump(t, out)
	for _, banned := range []string{
		"*Monitor URL:*", "*CC:*", "*Expires:*",
		"*Days remaining:*", "*Issuer:*", "*Subject:*", "*Tags:*",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("legacy labeled row %q must not appear: %s", banned, s)
		}
	}
}

func TestBuildSSLParent_subjectFoldedIntoBodyWhenPresent(t *testing.T) {
	in := sslOpenInput()
	in.Subject = "CN=api.example.test"
	out := slack.BuildSSLParent(in)
	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected title + body sections")
	}
	body := sections[1]
	if !strings.Contains(body, "Issuer `Let's Encrypt`") {
		t.Errorf("body missing Issuer: %q", body)
	}
	if !strings.Contains(body, "Subject `CN=api.example.test`") {
		t.Errorf("body missing Subject: %q", body)
	}
}

// -- BuildSSLResolveEdit ---------------------------------------------

func TestBuildSSLResolveEdit_titleSaysCertRenewed(t *testing.T) {
	newExpiry := sslDetected.Add(89 * 24 * time.Hour)
	in := slack.SSLResolveInput{
		SSLDownInput: sslOpenInput(),
		NewExpiresAt: newExpiry,
		RenewedAt:    sslDetected.Add(2 * time.Hour),
	}
	out := slack.BuildSSLResolveEdit(in)

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	if hasBlockType(out.Blocks, "header") {
		t.Error("must not emit a header block")
	}

	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) == 0 {
		t.Fatal("expected at least title section")
	}
	title := sections[0]
	if !strings.Contains(title, ":large_green_circle:") {
		t.Errorf("title missing green emoji: %q", title)
	}
	if !strings.Contains(title, "Cert renewed") {
		t.Errorf("title must say 'Cert renewed': %q", title)
	}
	if !strings.Contains(title, "<https://api.example.test|https://api.example.test>") {
		t.Errorf("title missing URL: %q", title)
	}

	contexts := blockTexts(t, out.Blocks, "context")
	if len(contexts) == 0 {
		t.Fatal("expected footer context block")
	}
	footer := contexts[len(contexts)-1]
	if !strings.Contains(footer, "_Renewed ") {
		t.Errorf("footer prefix must be 'Renewed': %q", footer)
	}
}

func TestBuildSSLResolveEdit_bodyKeepsIssuer(t *testing.T) {
	in := slack.SSLResolveInput{
		SSLDownInput: sslOpenInput(),
		NewExpiresAt: sslDetected.Add(89 * 24 * time.Hour),
		RenewedAt:    sslDetected,
	}
	out := slack.BuildSSLResolveEdit(in)
	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected title + body sections")
	}
	if !strings.Contains(sections[1], "Issuer `Let's Encrypt`") {
		t.Errorf("body should preserve Issuer on resolve edit: %q", sections[1])
	}
}

// -- SSL thread replies (unchanged) ----------------------------------

func TestBuildSSLReminderReply_carriesDaysRemaining(t *testing.T) {
	s := dump(t, slack.BuildSSLReminderReply(sslOpenInput()))
	if !strings.Contains(s, "*Days remaining:* `7`") {
		t.Errorf("expected days-remaining line in:\n%s", s)
	}
}

func TestBuildSSLResolveReply_carriesNewExpiry(t *testing.T) {
	in := slack.SSLResolveInput{
		SSLDownInput: sslOpenInput(),
		NewExpiresAt: sslDetected.Add(90 * 24 * time.Hour),
		RenewedAt:    sslDetected,
	}
	s := dump(t, slack.BuildSSLResolveReply(in))
	if !strings.Contains(s, "Cert renewed.") {
		t.Errorf("expected 'Cert renewed.' in reply:\n%s", s)
	}
}
