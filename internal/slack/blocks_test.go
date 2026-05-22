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
// (json with HTML escaping disabled, so <!date^...> tokens come
// through literally). Accepts either []slack.Block or
// []slack.Attachment so parent-message tests can assert on the new
// color-stripe shape.
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

func TestBuildDownParent_includesHeaderContextFieldsAndMentions(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API",
		Group:        "prod",
		URL:          "http://api/health",
		Mentions:     []string{"<!here>", "<@U123ABC>"},
		StatusCode:   503,
		StatusText:   "Service Unavailable",
		FailureAt:    t0,
		LastError:    "boom",
		DetailURL:    "https://monitor.internal/monitor/api",
	})
	s := dump(t, out)
	for _, want := range []string{
		":red_circle: API is DOWN",
		"*Monitor URL:* http://api/health",
		"*Group:* `prod`",
		"<!here> <@U123ABC>",
		"*Reason:* `503 Service Unavailable`",
		"*Error:* `boom`",
		"_Detected ",
		"|View details>",
		"https://monitor.internal/monitor/api",
		"<!date^",
		`"color":"#df3617"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestBuildDownParent_omitsButtonWhenDetailURLEmpty(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", Group: "prod", URL: "http://api",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
	})
	if strings.Contains(dump(t, out), "View details") {
		t.Error("expected no [View details] button when DetailURL is empty")
	}
}

func TestBuildDownParent_omitsMentionsWhenEmpty(t *testing.T) {
	out := slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", Group: "prod", URL: "http://api",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
	})
	// No mentions array → no section block with just mention markup.
	s := dump(t, out)
	if strings.Contains(s, "<!here>") || strings.Contains(s, "<@U") {
		t.Errorf("unexpected mention markup in:\n%s", s)
	}
}

func TestBuildDownParent_inlineBodyOnlyWhenWithinThreshold(t *testing.T) {
	small := strings.Repeat("x", 50)
	large := strings.Repeat("x", 500)

	withSmall := dump(t, slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", Group: "g", URL: "u",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
		ResponseBody: small,
		BodyMaxChars: 200,
	}))
	if !strings.Contains(withSmall, small) {
		t.Errorf("expected inline body for small response, missing in:\n%s", withSmall)
	}

	withLarge := dump(t, slack.BuildDownParent(slack.DownInput{
		FriendlyName: "API", Group: "g", URL: "u",
		StatusCode: 500, StatusText: "x", FailureAt: t0,
		ResponseBody: large,
		BodyMaxChars: 200,
	}))
	if strings.Contains(withLarge, large) {
		t.Error("expected NO inline body when response exceeds bodyMaxChars")
	}
}

func TestBuildResolveEdit_preservesContextAndChangesHeader(t *testing.T) {
	resolveAt := t0.Add(45 * time.Minute)
	in := slack.ResolveInput{
		DownInput: slack.DownInput{
			FriendlyName: "API", Group: "prod", URL: "http://api/health",
			Mentions:   []string{"<!here>"},
			StatusCode: 503, StatusText: "Service Unavailable", FailureAt: t0,
			LastError: "boom",
			DetailURL: "https://monitor.internal/monitor/api",
		},
		ResolveAt: resolveAt,
		Downtime:  45 * time.Minute,
	}
	s := dump(t, slack.BuildResolveEdit(in))

	if !strings.Contains(s, ":large_green_circle: API is UP (was down for 45m)") {
		t.Errorf("missing resolved header in:\n%s", s)
	}
	// Body still carries the monitor URL + group.
	if !strings.Contains(s, "*Monitor URL:* http://api/health") {
		t.Errorf("monitor URL line missing in resolve edit:\n%s", s)
	}
	if !strings.Contains(s, "*Group:* `prod`") {
		t.Errorf("group line missing in resolve edit:\n%s", s)
	}
	// Duration line + Resolved-at footer.
	if !strings.Contains(s, "*Duration:* `45m`") {
		t.Errorf("duration line missing in resolve edit:\n%s", s)
	}
	if !strings.Contains(s, "_Resolved ") {
		t.Errorf("resolved-at footer missing in:\n%s", s)
	}
	// Mentions preserved on the edited parent.
	if !strings.Contains(s, "<!here>") {
		t.Errorf("mention markup missing in resolve edit:\n%s", s)
	}
	if !strings.Contains(s, `"color":"#22af64"`) {
		t.Errorf("missing green color stripe in:\n%s", s)
	}
}

func TestBuildReminderReply_noMentions(t *testing.T) {
	s := dump(t, slack.BuildReminderReply(slack.ReminderInput{
		DownDuration:  3 * 24 * time.Hour,
		LastCheckedAt: t0,
		LastError:     "still 503",
	}))
	if !strings.Contains(s, "*Still down for:* `3d`") {
		t.Errorf("missing 'Still down for: `3d`' in:\n%s", s)
	}
	if !strings.Contains(s, "*Last error:* `still 503`") {
		t.Errorf("missing last error line in:\n%s", s)
	}
	if strings.Contains(s, "<!here>") || strings.Contains(s, "<@U") {
		t.Errorf("reminder should have NO mentions, got:\n%s", s)
	}
}

func TestBuildResolveReply_noMentionsAndCarriesDowntime(t *testing.T) {
	s := dump(t, slack.BuildResolveReply(slack.ResolveInput{
		DownInput: slack.DownInput{
			FriendlyName: "API",
			Mentions:     []string{"<!here>"}, // present in input but reply must not echo them
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
