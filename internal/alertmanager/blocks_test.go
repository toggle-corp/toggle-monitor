package alertmanager_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

var tStart = time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

// dump renders block-kit output the way the Slack client will (no HTML
// escaping, so the `<!date^...>` tokens come through literally).
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

// blockTexts walks a slack.Message's Blocks and returns the rendered
// text of every block matching wantType.
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

func hasBlockType(blocks []slack.Block, ty string) bool {
	for _, b := range blocks {
		if s, _ := b["type"].(string); s == ty {
			return true
		}
	}
	return false
}

// fullAlert is a critical firing AM alert with every field populated.
func fullAlert() alertmanager.Alert {
	return alertmanager.Alert{
		Status: alertmanager.AlertStatusFiring,
		Labels: map[string]string{
			"alertname": "HighCPU",
			"severity":  "critical",
			"namespace": "prod",
			"service":   "api",
			"instance":  "pod-1",
			"job":       "node-exporter",
			"cluster":   "us-east-1",
			"pod":       "api-7b8",
		},
		Annotations: map[string]string{
			"summary":     "CPU has been above 95% for 5 minutes",
			"runbook_url": "https://runbooks.example.test/cpu",
			"description": "long-form description that should NOT render",
		},
		StartsAt:    tStart,
		Fingerprint: "fp-abc123",
	}
}

func openInput() alertmanager.AMOpenInput {
	return alertmanager.AMOpenInput{
		Alert:       fullAlert(),
		Mentions:    []string{"<!here>", "<@U123ABC>"},
		DetailURL:   "https://monitor.internal/alert/inc-1",
		Receiver:    "toggle_monitor",
		ExternalURL: "https://am.prod.example.test/",
	}
}

// -- BuildAMOpen ------------------------------------------------------

func TestBuildAMOpen_threeBlockShape(t *testing.T) {
	out := alertmanager.BuildAMOpen(openInput())

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	if hasBlockType(out.Blocks, "header") {
		t.Error("must not emit a header block (blocks-only contract)")
	}
	if hasBlockType(out.Blocks, "actions") {
		t.Error("must not emit an actions block (footer carries inline links)")
	}

	sections := blockTexts(t, out.Blocks, "section")
	if len(sections) < 2 {
		t.Fatalf("expected at least 2 section blocks (title + body), got %d", len(sections))
	}
	title := sections[0]
	body := sections[1]

	// Title — severity emoji, bold alertname, inline severity chip,
	// key=`value` chips after middle dots.
	for _, want := range []string{
		"\U0001F525", // 🔥 critical
		"*HighCPU*",
		"`critical`",
		"namespace=`prod`",
		"service=`api`",
		"instance=`pod-1`",
	} {
		if !strings.Contains(title, want) {
			t.Errorf("title missing %q in %q", want, title)
		}
	}

	// Body — annotations.summary as-is, no labels.
	if !strings.Contains(body, "CPU has been above 95% for 5 minutes") {
		t.Errorf("body missing summary: %q", body)
	}

	// Footer — mentions, Receiver, Via, Firing ago, View-details + Runbook inline.
	contexts := blockTexts(t, out.Blocks, "context")
	if len(contexts) == 0 {
		t.Fatal("expected footer context block")
	}
	footer := contexts[len(contexts)-1]
	for _, want := range []string{
		"<!here>",
		"<@U123ABC>",
		"Receiver `toggle_monitor`",
		"Via `am.prod.example.test`",
		"Firing ",
		" ago_",
		"<https://monitor.internal/alert/inc-1|View details>",
		"<https://runbooks.example.test/cpu|Runbook>",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q in %q", want, footer)
		}
	}

	// Description must NOT render — that lives on the detail page.
	if strings.Contains(dump(t, out), "long-form description") {
		t.Error("description should not appear in Slack body")
	}
}

func TestBuildAMOpen_warningNoRunbook(t *testing.T) {
	in := openInput()
	in.Alert.Labels["severity"] = "warning"
	delete(in.Alert.Annotations, "runbook_url")
	out := alertmanager.BuildAMOpen(in)

	sections := blockTexts(t, out.Blocks, "section")
	if !strings.Contains(sections[0], "⚠️") {
		t.Errorf("expected warning emoji in title: %q", sections[0])
	}
	contexts := blockTexts(t, out.Blocks, "context")
	footer := contexts[len(contexts)-1]
	if !strings.Contains(footer, "|View details>") {
		t.Errorf("expected View-details link in footer: %q", footer)
	}
	if strings.Contains(footer, "Runbook") {
		t.Errorf("Runbook link should be absent when runbook_url unset: %q", footer)
	}
}

func TestBuildAMOpen_omitsViewDetailsWhenNoURLs(t *testing.T) {
	in := openInput()
	in.DetailURL = ""
	delete(in.Alert.Annotations, "runbook_url")
	out := alertmanager.BuildAMOpen(in)
	s := dump(t, out)

	if strings.Contains(s, "View details") || strings.Contains(s, "Runbook") {
		t.Errorf("expected no inline View-details / Runbook links:\n%s", s)
	}
	if strings.Contains(s, `"type":"actions"`) {
		t.Errorf("expected no actions block:\n%s", s)
	}
}

func TestBuildAMOpen_noSummaryRendersPlaceholder(t *testing.T) {
	in := openInput()
	delete(in.Alert.Annotations, "summary")
	out := alertmanager.BuildAMOpen(in)

	if !strings.Contains(dump(t, out), "_no summary_") {
		t.Errorf("expected `_no summary_` placeholder in:\n%s", dump(t, out))
	}
}

func TestBuildAMOpen_noSeverityFallbackEmoji(t *testing.T) {
	in := openInput()
	delete(in.Alert.Labels, "severity")
	out := alertmanager.BuildAMOpen(in)
	sections := blockTexts(t, out.Blocks, "section")

	if !strings.Contains(sections[0], "\U0001F6A8") { // 🚨 fallback
		t.Errorf("expected fallback siren emoji in title: %q", sections[0])
	}
	if strings.Contains(sections[0], "`critical`") ||
		strings.Contains(sections[0], "`warning`") {
		t.Errorf("severity chip should be absent when label missing: %q", sections[0])
	}
}

func TestBuildAMOpen_summaryTruncatedAtBodyMaxChars(t *testing.T) {
	in := openInput()
	in.BodyMaxChars = 20
	in.Alert.Annotations["summary"] = strings.Repeat("x", 100)
	s := dump(t, alertmanager.BuildAMOpen(in))

	if !strings.Contains(s, "…") {
		t.Errorf("expected truncation ellipsis in:\n%s", s)
	}
	if strings.Contains(s, strings.Repeat("x", 100)) {
		t.Errorf("expected truncated body, got full 100x:\n%s", s)
	}
}

func TestBuildAMOpen_keyLabelsCappedAtThreeStableOrder(t *testing.T) {
	out := alertmanager.BuildAMOpen(openInput())
	sections := blockTexts(t, out.Blocks, "section")
	title := sections[0]

	idxNS := strings.Index(title, "namespace=`prod`")
	idxInst := strings.Index(title, "instance=`pod-1`")
	idxSvc := strings.Index(title, "service=`api`")
	if idxNS == -1 || idxInst == -1 || idxSvc == -1 {
		t.Fatalf("expected namespace/instance/service chips in title: %q", title)
	}
	if !(idxNS < idxInst && idxInst < idxSvc) {
		t.Errorf("expected canonical order namespace, instance, service: %q", title)
	}
	for _, unwanted := range []string{"job=", "cluster=", "pod=`api-7b8`"} {
		if strings.Contains(title, unwanted) {
			t.Errorf("expected only three key labels; got extra %q in title: %q", unwanted, title)
		}
	}
}

func TestBuildAMOpen_footerOmitsEmptyFields(t *testing.T) {
	in := openInput()
	in.Receiver = ""
	in.ExternalURL = ""
	out := alertmanager.BuildAMOpen(in)
	contexts := blockTexts(t, out.Blocks, "context")
	footer := contexts[len(contexts)-1]

	if strings.Contains(footer, "Receiver `") {
		t.Errorf("footer should omit Receiver when empty: %q", footer)
	}
	if strings.Contains(footer, "Via `") {
		t.Errorf("footer should omit Via when ExternalURL empty: %q", footer)
	}
	if !strings.Contains(footer, "Firing ") {
		t.Errorf("Firing-ago should still render: %q", footer)
	}
}

func TestBuildAMOpen_externalURLParseFallback(t *testing.T) {
	in := openInput()
	in.ExternalURL = "not a url"
	out := alertmanager.BuildAMOpen(in)
	contexts := blockTexts(t, out.Blocks, "context")
	footer := contexts[len(contexts)-1]
	if !strings.Contains(footer, "Via `not a url`") {
		t.Errorf("expected raw fallback for unparseable ExternalURL in: %q", footer)
	}
}

// -- BuildAMResolveEdit -----------------------------------------------

func resolveInput() alertmanager.AMResolveInput {
	return alertmanager.AMResolveInput{
		AMOpenInput: openInput(),
		ResolvedAt:  tStart.Add(12 * time.Minute),
		Downtime:    12 * time.Minute,
	}
}

func TestBuildAMResolveEdit_titleResolvedSuffix(t *testing.T) {
	out := alertmanager.BuildAMResolveEdit(resolveInput())

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	sections := blockTexts(t, out.Blocks, "section")
	title := sections[0]
	if !strings.Contains(title, "✅") {
		t.Errorf("expected ✅ emoji in resolve title: %q", title)
	}
	if !strings.Contains(title, "_Resolved_") {
		t.Errorf("expected `_Resolved_` suffix in title: %q", title)
	}
	if strings.Contains(title, "\U0001F525") {
		t.Errorf("severity emoji should be swapped out on resolve: %q", title)
	}
}

func TestBuildAMResolveEdit_footerStampHasResolvedAtAndDowntime(t *testing.T) {
	out := alertmanager.BuildAMResolveEdit(resolveInput())
	contexts := blockTexts(t, out.Blocks, "context")
	footer := contexts[len(contexts)-1]

	for _, want := range []string{
		"Resolved ",
		"<!date^",
		"down around 12m",
		"<https://monitor.internal/alert/inc-1|View details>",
		"<https://runbooks.example.test/cpu|Runbook>",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q in %q", want, footer)
		}
	}
}

func TestBuildAMResolveEdit_omitsBannerWhenEmpty(t *testing.T) {
	out := alertmanager.BuildAMResolveEdit(resolveInput())
	if strings.Contains(dump(t, out), "Resolved without an open incident") {
		t.Errorf("expected no banner when unset")
	}
}

// -- BuildAMLateResolve ----------------------------------------------

func TestBuildAMLateResolve_bannerIsFirstContextBlockAfterTitle(t *testing.T) {
	out := alertmanager.BuildAMLateResolve(resolveInput())

	if len(out.Blocks) < 3 {
		t.Fatalf("expected at least 3 blocks (title + banner + body), got %d", len(out.Blocks))
	}
	// blocks[0] = title section, blocks[1] = banner context.
	if ty, _ := out.Blocks[0]["type"].(string); ty != "section" {
		t.Errorf("blocks[0] type=%q, want section", ty)
	}
	if ty, _ := out.Blocks[1]["type"].(string); ty != "context" {
		t.Errorf("blocks[1] type=%q, want context (banner)", ty)
	}
	banner := blockTexts(t, []slack.Block{out.Blocks[1]}, "context")
	if len(banner) == 0 || !strings.Contains(banner[0], "Resolved without an open incident") {
		t.Errorf("expected late-resolve banner text in blocks[1]: %v", banner)
	}
}

func TestBuildAMLateResolve_operatorBannerOverridesDefault(t *testing.T) {
	in := resolveInput()
	in.Banner = "custom operator banner"
	s := dump(t, alertmanager.BuildAMLateResolve(in))

	if !strings.Contains(s, "custom operator banner") {
		t.Errorf("expected operator-supplied banner in:\n%s", s)
	}
	if strings.Contains(s, "Resolved without an open incident") {
		t.Errorf("default banner should not appear when operator supplied one")
	}
}

// -- BuildAMResolveReply ---------------------------------------------

func TestBuildAMResolveReply_shortAndCarriesDowntime(t *testing.T) {
	s := dump(t, alertmanager.BuildAMResolveReply(resolveInput()))

	if !strings.Contains(s, "✅ Resolved after 12m") {
		t.Errorf("expected `✅ Resolved after 12m` in thread reply:\n%s", s)
	}
	if strings.Contains(s, "<!here>") || strings.Contains(s, "<@U") {
		t.Errorf("thread reply must not echo mentions:\n%s", s)
	}
}

// -- BuildAMThrottleNotice (unchanged) -------------------------------

func TestBuildAMThrottleNotice_singleSectionBlock(t *testing.T) {
	out := alertmanager.BuildAMThrottleNotice(alertmanager.AMThrottleNoticeInput{
		ChannelSlug: "ops-noisy",
		Dropped:     42,
		PerChannel:  20,
		Window:      5 * time.Minute,
	})

	if out.Attachments != nil {
		t.Errorf("attachments must be nil, got %+v", out.Attachments)
	}
	if len(out.Blocks) != 1 {
		t.Errorf("expected exactly 1 block, got %d", len(out.Blocks))
	}
	if ty, _ := out.Blocks[0]["type"].(string); ty != "section" {
		t.Errorf("blocks[0] type=%q, want section", ty)
	}
	s := dump(t, out)
	if !strings.Contains(s, "Alertmanager throttle engaged in `#ops-noisy`") {
		t.Errorf("expected throttle warning text in:\n%s", s)
	}
}
