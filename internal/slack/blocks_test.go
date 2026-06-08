package slack_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

var t0 = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

// dump renders Block Kit output the same way the Slack client will
// (JSON with HTML escaping disabled, so <!date^...> tokens come through
// literally). Used for substring assertions across the message tree.
func dump(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return buf.String()
}

// blockTexts walks a ParentMessage's Blocks and returns the rendered
// text of every block matching wantType. For sections it pulls
// text.text; for context it concatenates each element's text.
func blockTexts(t *testing.T, blocks []slack.Block, wantType string) []string {
	t.Helper()
	var out []string
	for _, b := range blocks {
		ty, _ := b["type"].(string)
		if ty != wantType {
			continue
		}
		if txt, ok := b["text"].(map[string]any); ok {
			if s, ok := txt["text"].(string); ok {
				out = append(out, s)
			}
			continue
		}
		if els, ok := b["elements"].([]map[string]any); ok {
			var parts []string
			for _, el := range els {
				if s, ok := el["text"].(string); ok {
					parts = append(parts, s)
				}
			}
			out = append(out, strings.Join(parts, " "))
		}
	}
	return out
}

// hasBlockType reports whether any block in blocks has type==t.
func hasBlockType(blocks []slack.Block, ty string) bool {
	for _, b := range blocks {
		if s, _ := b["type"].(string); s == ty {
			return true
		}
	}
	return false
}

// -- BuildDownParent -------------------------------------------------

func TestBuildDownParent_threeBlockShape(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API",
		Tags:         []string{"prod"}, // intentionally present; should NOT render on parent
		URL:          "http://api/health",
		Mentions:     []string{"<!here>", "<@U123ABC>"},
		StatusCode:   503,
		StatusText:   "Service Unavailable",
		FailureAt:    t0,
		LastError:    "boom",
		DetailURL:    "https://monitor.internal/monitor/api",
	})

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	if hasBlockType(out.Blocks, "header") {
		t.Error("must not emit a header block (blocks-only contract)")
	}
	if hasBlockType(out.Blocks, "actions") {
		t.Error("must not emit an actions block (blocks-only contract)")
	}
	if got := len(out.Blocks); got < 2 || got > 4 {
		t.Errorf("expected 2-4 blocks, got %d", got)
	}

	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected at least 2 section blocks (title + body), got %d", len(sections))
	}
	title := sections[0]
	body := sections[1]

	// Title shape — emoji, bold name with state, URL after middle dot.
	for _, want := range []string{
		":red_circle:",
		"*API is DOWN*",
		"<http://api/health|http://api/health>",
	} {
		if !strings.Contains(title, want) {
			t.Errorf("title missing %q in %q", want, title)
		}
	}

	// Body shape — fenced status text (ADR-0007: fenced code blocks
	// avoid mobile Slack auto-extracting URLs out of inline-code spans).
	if !strings.Contains(body, "```\n503 Service Unavailable\n```") {
		t.Errorf("body missing fenced status: %q", body)
	}
	// Must not be wrapped in inline code (single backticks around the
	// whole thing).
	if strings.Contains(body, "`503 Service Unavailable`") {
		t.Errorf("body must be fenced, not inline-coded: %q", body)
	}

	// Footer (context) — mentions, Detected, View-details.
	contexts := blockTexts(t, out.Blocks, "context")
	if len(contexts) == 0 {
		t.Fatal("expected at least one context block (footer)")
	}
	footer := contexts[len(contexts)-1]
	for _, want := range []string{
		"<!here>",
		"<@U123ABC>",
		"_Detected ",
		"<!date^",
		"<https://monitor.internal/monitor/api|View details>",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q in %q", want, footer)
		}
	}

	// Labeled-row fields must NOT appear anywhere.
	s := dump(t, out)
	for _, banned := range []string{
		"*Monitor URL:*", "*CC:*", "*Reason:*", "*Error:*", "*Tags:*",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("legacy labeled row %q must not appear: %s", banned, s)
		}
	}
}

func TestBuildDownParent_transportErrorBody(t *testing.T) {
	// StatusCode 0 → body falls back to LastError, fenced (ADR-0007).
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API",
		URL:          "http://api",
		StatusCode:   0,
		LastError:    "dial tcp: i/o timeout",
		FailureAt:    t0,
	})
	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected title + body sections, got %d", len(sections))
	}
	if !strings.Contains(sections[1], "```\ndial tcp: i/o timeout\n```") {
		t.Errorf("body missing fenced transport error: %q", sections[1])
	}
}

// TestBuildDownParent_urlInErrorBodyStaysLiteral guards the ADR-0007
// regression: an error message containing a URL must render the URL as
// literal text inside a fenced block, not as a separately-rendered
// auto-link with empty quotes (the mobile-Slack bug).
func TestBuildDownParent_urlInErrorBodyStaysLiteral(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API",
		URL:          "http://api",
		StatusCode:   0,
		LastError:    `Get "https://api.example.com/health": context deadline exceeded`,
		FailureAt:    t0,
	})
	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected title + body sections, got %d", len(sections))
	}
	body := sections[1]
	want := "```\n" + `Get "https://api.example.com/health": context deadline exceeded` + "\n```"
	if !strings.Contains(body, want) {
		t.Errorf("body must wrap the URL-bearing error in a fenced block; got: %q", body)
	}
}

func TestBuildDownParent_omitsMentionsWhenEmpty(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", URL: "http://api",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
		DetailURL: "https://monitor.internal/monitor/api",
	})
	s := dump(t, out)
	if strings.Contains(s, "<!here>") || strings.Contains(s, "<@U") {
		t.Errorf("unexpected mention markup in:\n%s", s)
	}
	contexts := blockTexts(t, out.Blocks, "context")
	if len(contexts) == 0 {
		t.Fatal("expected footer context block even without mentions")
	}
	// View details still present.
	if !strings.Contains(contexts[len(contexts)-1], "|View details>") {
		t.Errorf("footer missing View-details link: %q", contexts[len(contexts)-1])
	}
}

func TestBuildDownParent_omitsViewDetailsWhenDetailURLEmpty(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", URL: "http://api",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
	})
	if strings.Contains(dump(t, out), "View details") {
		t.Error("expected no [View details] link when DetailURL is empty")
	}
}

func TestBuildDownParent_inlineBodyFencedWhenWithinThreshold(t *testing.T) {
	small := strings.Repeat("x", 50)
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", URL: "u",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
		ResponseBody: small,
		BodyMaxChars: 200,
	})
	sections := blockTexts(t, out.Blocks, "section")
	found := false
	for _, sec := range sections {
		if strings.Contains(sec, "```\n"+small+"\n```") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fenced response body within threshold; got sections:\n%v", sections)
	}
}

func TestBuildDownParent_inlineBodySuppressedOverThreshold(t *testing.T) {
	large := strings.Repeat("x", 500)
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", URL: "u",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
		ResponseBody: large,
		BodyMaxChars: 200,
	})
	if strings.Contains(dump(t, out), large) {
		t.Error("expected NO inline body when response exceeds bodyMaxChars")
	}
}

func TestBuildDownParent_bannerRendersAboveBody(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", URL: "http://api",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
		Banner: "ℹ️ banner text here",
	})
	if !strings.Contains(dump(t, out), "ℹ️ banner text here") {
		t.Error("expected banner to render")
	}
}

// -- BuildResolveEdit -----------------------------------------------

func TestBuildResolveEdit_titleSaysDownAround(t *testing.T) {
	resolveAt := t0.Add(45 * time.Minute)
	in := slack.ResolveInput{
		DownInput: slack.DownInput{
			FriendlyName: "API", URL: "http://api/health",
			Mentions:   []string{"<!here>"},
			StatusCode: 503, StatusText: "Service Unavailable", FailureAt: t0,
			LastError: "boom",
			DetailURL: "https://monitor.internal/monitor/api",
		},
		ResolveAt: resolveAt,
		Downtime:  45 * time.Minute,
	}
	out := slack.BuildResolveEdit(in)

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	if hasBlockType(out.Blocks, "header") {
		t.Error("must not emit a header block")
	}
	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) == 0 {
		t.Fatal("expected at least a title section block")
	}
	title := sections[0]
	if !strings.Contains(title, ":large_green_circle:") {
		t.Errorf("title missing green emoji: %q", title)
	}
	if !strings.Contains(title, "*API is UP*") {
		t.Errorf("title missing bold 'is UP': %q", title)
	}
	if !strings.Contains(title, "down around 45m") {
		t.Errorf("title must say 'down around 45m' (not 'was down for'): %q", title)
	}
	if strings.Contains(title, "was down for") {
		t.Errorf("title must not say 'was down for': %q", title)
	}

	// Footer prefix is "Resolved".
	contexts := blockTexts(t, out.Blocks, "context")
	if len(contexts) == 0 {
		t.Fatal("expected footer context block")
	}
	footer := contexts[len(contexts)-1]
	if !strings.Contains(footer, "_Resolved ") {
		t.Errorf("footer prefix must be 'Resolved': %q", footer)
	}
	if !strings.Contains(footer, "<!here>") {
		t.Errorf("footer must preserve mentions: %q", footer)
	}

	// No separate Duration line on the parent.
	if strings.Contains(dump(t, out), "*Duration:*") {
		t.Error("resolve edit must not emit a *Duration:* line; downtime lives in the title")
	}
}

func TestBuildResolveEdit_bodyKeepsFailureReason(t *testing.T) {
	in := slack.ResolveInput{
		DownInput: slack.DownInput{
			FriendlyName: "API", URL: "http://api/health",
			StatusCode: 503, StatusText: "Service Unavailable", FailureAt: t0,
		},
		ResolveAt: t0.Add(30 * time.Minute),
		Downtime:  30 * time.Minute,
	}
	out := slack.BuildResolveEdit(in)
	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected title + body sections on resolve edit, got %d", len(sections))
	}
	if !strings.Contains(sections[1], "```\n503 Service Unavailable\n```") {
		t.Errorf("body should keep the fenced failure reason: %q", sections[1])
	}
}

// -- Thread replies (unchanged) -------------------------------------

func TestBuildReminderReply_noMentions(t *testing.T) {
	blocks := slack.BuildReminderReply(slack.ReminderInput{
		DownDuration:  3 * 24 * time.Hour,
		LastCheckedAt: t0,
		LastError:     "still 503",
	})

	// ADR-0007: reminder splits into two blocks — labels section, then
	// fenced error section — so a URL in LastError survives mobile.
	if got := len(blocks); got != 2 {
		t.Fatalf("expected 2 blocks (labels + fenced error), got %d", got)
	}
	sections := blockTexts(t, blocks, "section")
	if len(sections) != 2 {
		t.Fatalf("expected 2 section blocks, got %d", len(sections))
	}
	labels := sections[0]
	if !strings.Contains(labels, "*Still down for:* `3d`") {
		t.Errorf("missing 'Still down for: `3d`' in labels block:\n%s", labels)
	}
	if !strings.Contains(labels, "*Last checked:*") {
		t.Errorf("missing 'Last checked:' in labels block:\n%s", labels)
	}
	if strings.Contains(labels, "*Last error:*") {
		t.Errorf("labels block must not carry the *Last error:* label anymore:\n%s", labels)
	}

	errBody := sections[1]
	if !strings.Contains(errBody, "```\nstill 503\n```") {
		t.Errorf("expected fenced error in second block, got:\n%s", errBody)
	}

	s := dump(t, blocks)
	if strings.Contains(s, "<!here>") || strings.Contains(s, "<@U") {
		t.Errorf("reminder should have NO mentions, got:\n%s", s)
	}
}

// TestBuildReminderReply_omitsErrorBlockWhenEmpty: with no LastError
// the reminder collapses back to a single labels-only block.
func TestBuildReminderReply_omitsErrorBlockWhenEmpty(t *testing.T) {
	blocks := slack.BuildReminderReply(slack.ReminderInput{
		DownDuration:  47 * time.Minute,
		LastCheckedAt: t0,
		LastError:     "",
	})
	if got := len(blocks); got != 1 {
		t.Fatalf("expected 1 block when LastError empty, got %d", got)
	}
}

// TestBuildReminderReply_urlInErrorStaysLiteral guards the ADR-0007
// regression: a URL inside LastError must end up inside the fenced
// error block, not as a Slack auto-link.
func TestBuildReminderReply_urlInErrorStaysLiteral(t *testing.T) {
	blocks := slack.BuildReminderReply(slack.ReminderInput{
		DownDuration:  10 * time.Minute,
		LastCheckedAt: t0,
		LastError:     `Get "https://api.example.com/health": context deadline exceeded`,
	})
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	sections := blockTexts(t, blocks, "section")
	want := "```\n" + `Get "https://api.example.com/health": context deadline exceeded` + "\n```"
	if !strings.Contains(sections[1], want) {
		t.Errorf("URL-bearing error must be fenced verbatim in block 2; got: %q", sections[1])
	}
}

func TestBuildResolveReply_noMentionsAndCarriesDowntime(t *testing.T) {
	s := dump(t, slack.BuildResolveReply(slack.ResolveInput{
		DownInput: slack.DownInput{
			FriendlyName: "API",
			Mentions:     []string{"<!here>"},
		},
		ResolveAt: t0,
		Downtime:  2*time.Hour + 15*time.Minute,
	}))
	if !strings.Contains(s, "Total downtime: `2h 15m`") {
		t.Errorf("missing downtime in:\n%s", s)
	}
	if strings.Contains(s, "<!here>") || strings.Contains(s, "<@U") {
		t.Errorf("resolve reply should have NO mentions, got:\n%s", s)
	}
}

func TestFormatDate_emitsSlackDateToken(t *testing.T) {
	s := slack.FormatDate(t0)
	if !strings.HasPrefix(s, "<!date^") {
		t.Errorf("expected Slack date token, got %q", s)
	}
	if !strings.Contains(s, "|2026-05-21 12:00 UTC") {
		t.Errorf("missing fallback string in: %q", s)
	}
}
