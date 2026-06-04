package alertmanager_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
)

var tStart = time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)

// dump renders block-kit output the way the Slack client will (no HTML
// escaping, so the `<!date^...>` tokens come through literally). Mirrors
// internal/slack/blocks_test.go:dump.
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

// fullAlert is a critical firing AM alert with every field populated;
// the per-test builders shallow-mutate it.
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

func TestBuildAMOpen_criticalFullyPopulated(t *testing.T) {
	s := dump(t, alertmanager.BuildAMOpen(openInput()))
	for _, want := range []string{
		"\U0001F525", // 🔥 critical
		"HighCPU",
		"[critical]",
		"namespace=prod",
		"service=api",
		"instance=pod-1",
		"CPU has been above 95% for 5 minutes",
		"<!here>",
		"<@U123ABC>",
		"View details",
		"https://monitor.internal/alert/inc-1",
		"Runbook",
		"https://runbooks.example.test/cpu",
		"Receiver: `toggle_monitor`",
		"Via: `am.prod.example.test`",
		"Firing since:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// description must NOT render in body — that lives on the detail page.
	if strings.Contains(s, "long-form description") {
		t.Errorf("description should not appear in Slack body; got:\n%s", s)
	}
}

func TestBuildAMOpen_warningNoRunbook(t *testing.T) {
	in := openInput()
	in.Alert.Labels["severity"] = "warning"
	delete(in.Alert.Annotations, "runbook_url")
	s := dump(t, alertmanager.BuildAMOpen(in))

	if !strings.Contains(s, "⚠️") { // ⚠️ warning
		t.Errorf("expected warning emoji in:\n%s", s)
	}
	if !strings.Contains(s, "View details") {
		t.Errorf("expected View details button in:\n%s", s)
	}
	if strings.Contains(s, "Runbook") {
		t.Errorf("Runbook button should be absent when runbook_url unset:\n%s", s)
	}
}

func TestBuildAMOpen_noActionsBlockWhenNoURLs(t *testing.T) {
	in := openInput()
	in.DetailURL = ""
	delete(in.Alert.Annotations, "runbook_url")
	s := dump(t, alertmanager.BuildAMOpen(in))

	if strings.Contains(s, "View details") || strings.Contains(s, "Runbook") {
		t.Errorf("expected no actions block when neither URL is set:\n%s", s)
	}
	if strings.Contains(s, `"type":"actions"`) {
		t.Errorf("expected no `actions` block in:\n%s", s)
	}
}

func TestBuildAMOpen_noSummaryRendersPlaceholder(t *testing.T) {
	in := openInput()
	delete(in.Alert.Annotations, "summary")
	s := dump(t, alertmanager.BuildAMOpen(in))

	if !strings.Contains(s, "_no summary_") {
		t.Errorf("expected `_no summary_` placeholder in:\n%s", s)
	}
}

func TestBuildAMOpen_noSeverityFallbackEmoji(t *testing.T) {
	in := openInput()
	delete(in.Alert.Labels, "severity")
	s := dump(t, alertmanager.BuildAMOpen(in))

	if !strings.Contains(s, "\U0001F6A8") { // 🚨 fallback
		t.Errorf("expected fallback siren emoji in:\n%s", s)
	}
	if strings.Contains(s, "[critical]") || strings.Contains(s, "[warning]") {
		t.Errorf("severity chip should be absent when label missing:\n%s", s)
	}
}

func TestBuildAMOpen_summaryTruncatedAtBodyMaxChars(t *testing.T) {
	in := openInput()
	in.BodyMaxChars = 20
	in.Alert.Annotations["summary"] = strings.Repeat("x", 100)
	s := dump(t, alertmanager.BuildAMOpen(in))

	if !strings.Contains(s, "…") { // …
		t.Errorf("expected truncation ellipsis in:\n%s", s)
	}
	if strings.Contains(s, strings.Repeat("x", 100)) {
		t.Errorf("expected truncated body, got full 100x:\n%s", s)
	}
}

func TestBuildAMOpen_keyLabelsCappedAtThreeStableOrder(t *testing.T) {
	// fullAlert has 6 key labels: namespace, instance, service, job,
	// cluster, pod — the renderer must pick the first three in the
	// declared canonical order: namespace, instance, service.
	s := dump(t, alertmanager.BuildAMOpen(openInput()))

	// Locate header in dump; it lives in a `header` block. Easiest is
	// substring assertions on order: namespace=prod must appear before
	// service=api, and only three of the six key labels render.
	idxNS := strings.Index(s, "namespace=prod")
	idxInst := strings.Index(s, "instance=pod-1")
	idxSvc := strings.Index(s, "service=api")
	if idxNS == -1 || idxInst == -1 || idxSvc == -1 {
		t.Fatalf("expected namespace/instance/service in header, dump:\n%s", s)
	}
	if !(idxNS < idxInst && idxInst < idxSvc) {
		t.Errorf("expected canonical order namespace, instance, service in header, dump:\n%s", s)
	}
	// The remaining three (job, cluster, pod) must NOT render in header.
	for _, unwanted := range []string{"job=node-exporter", "cluster=us-east-1", "pod=api-7b8"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("expected only three key labels; got extra %q in:\n%s", unwanted, s)
		}
	}
}

func TestBuildAMOpen_footerOmitsEmptyFields(t *testing.T) {
	in := openInput()
	in.Receiver = ""
	in.ExternalURL = ""
	s := dump(t, alertmanager.BuildAMOpen(in))

	if strings.Contains(s, "Receiver:") {
		t.Errorf("footer should omit Receiver when empty:\n%s", s)
	}
	if strings.Contains(s, "Via:") {
		t.Errorf("footer should omit Via when ExternalURL empty:\n%s", s)
	}
	// Firing since: still appears (StartsAt always set).
	if !strings.Contains(s, "Firing since:") {
		t.Errorf("Firing since should still render:\n%s", s)
	}
}

func TestBuildAMOpen_externalURLParseFallback(t *testing.T) {
	in := openInput()
	// Garbage URL — parser may succeed but yield empty host; renderer
	// must fall back to the raw string rather than rendering "Via: ``".
	in.ExternalURL = "not a url"
	s := dump(t, alertmanager.BuildAMOpen(in))
	if !strings.Contains(s, "Via: `not a url`") {
		t.Errorf("expected raw fallback for unparseable ExternalURL in:\n%s", s)
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

func TestBuildAMResolveEdit_swapsEmojiAppendsResolved(t *testing.T) {
	s := dump(t, alertmanager.BuildAMResolveEdit(resolveInput()))

	if !strings.Contains(s, "✅") { // ✅
		t.Errorf("expected ✅ emoji in resolve edit:\n%s", s)
	}
	if !strings.Contains(s, "· Resolved") { // · Resolved
		t.Errorf("expected `· Resolved` suffix in header:\n%s", s)
	}
	if strings.Contains(s, "\U0001F525") { // 🔥 must be gone
		t.Errorf("severity emoji should be swapped out on resolve:\n%s", s)
	}
}

func TestBuildAMResolveEdit_bannerRendersWhenSet(t *testing.T) {
	in := resolveInput()
	in.Banner = "stale resolve banner text"
	s := dump(t, alertmanager.BuildAMResolveEdit(in))

	if !strings.Contains(s, "stale resolve banner text") {
		t.Errorf("expected banner in resolve edit:\n%s", s)
	}
}

func TestBuildAMResolveEdit_omitsBannerWhenEmpty(t *testing.T) {
	s := dump(t, alertmanager.BuildAMResolveEdit(resolveInput()))
	if strings.Contains(s, "stale resolve banner text") {
		t.Errorf("expected no banner when unset:\n%s", s)
	}
}

func TestBuildAMResolveEdit_preservesButtonsAndFooter(t *testing.T) {
	s := dump(t, alertmanager.BuildAMResolveEdit(resolveInput()))

	for _, want := range []string{
		"View details",
		"Runbook",
		"Receiver: `toggle_monitor`",
		"Via: `am.prod.example.test`",
		"Resolved at",
		"Downtime",
		"12m",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in resolve edit:\n%s", want, s)
		}
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

func TestBuildAMResolveReply_humanDowntimeFormats(t *testing.T) {
	cases := []struct {
		downtime time.Duration
		want     string
	}{
		{12 * time.Minute, "12m"},
		{2 * time.Hour, "2h"},
		{28*time.Hour + 0*time.Minute, "1d 4h"},
	}
	for _, c := range cases {
		in := resolveInput()
		in.Downtime = c.downtime
		s := dump(t, alertmanager.BuildAMResolveReply(in))
		if !strings.Contains(s, "Resolved after "+c.want) {
			t.Errorf("downtime %v: expected %q in:\n%s", c.downtime, c.want, s)
		}
	}
}

// -- BuildAMLateResolve ----------------------------------------------

func TestBuildAMLateResolve_defaultsBannerWhenEmpty(t *testing.T) {
	s := dump(t, alertmanager.BuildAMLateResolve(resolveInput()))

	// The constant lateResolveBanner is package-private; assert on a
	// stable substring of its known wording instead.
	if !strings.Contains(s, "resolved without an open incident") {
		t.Errorf("expected default late-resolve banner in:\n%s", s)
	}
}

func TestBuildAMLateResolve_operatorBannerOverridesDefault(t *testing.T) {
	in := resolveInput()
	in.Banner = "custom operator banner"
	s := dump(t, alertmanager.BuildAMLateResolve(in))

	if !strings.Contains(s, "custom operator banner") {
		t.Errorf("expected operator-supplied banner in:\n%s", s)
	}
	if strings.Contains(s, "resolved without an open incident") {
		t.Errorf("default banner should not appear when operator supplied one:\n%s", s)
	}
}
