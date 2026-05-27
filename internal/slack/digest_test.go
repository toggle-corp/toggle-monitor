package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

func TestBuildDigestParent_scoreboardAndRows(t *testing.T) {
	out := slack.BuildDigestParent(slack.DigestInput{
		Down:      2,
		Recovered: 1,
		Total:     3,
		OpenedAt:  t0,
		Mentions:  []string{"<!here>"},
		Rows: []slack.DigestRow{
			{Name: "payments", Class: slack.RowRecovered, DetailURL: "https://x/monitor/payments"},
			{Name: "api", Class: slack.RowActive},
			{Name: "web", Class: slack.RowActive},
		},
	})
	got := dump(t, out.Blocks) + dump(t, out.Attachments)

	if !strings.Contains(got, "2 down · 1 recovered (of 3)") {
		t.Fatalf("missing scoreboard header: %s", got)
	}
	// Active rows render un-struck; recovered is struck and keeps its link.
	if !strings.Contains(got, "🔴 api") || !strings.Contains(got, "🔴 web") {
		t.Fatalf("missing active rows: %s", got)
	}
	if !strings.Contains(got, "✅ ~<https://x/monitor/payments|payments>~") {
		t.Fatalf("recovered row should be struck + linked: %s", got)
	}
	if !strings.Contains(got, "<!here>") {
		t.Fatalf("open digest should carry mentions: %s", got)
	}
	if !strings.Contains(got, "#df3617") {
		t.Fatalf("open digest stripe should be red: %s", got)
	}
}

func TestBuildDigestParent_closedIsGreen(t *testing.T) {
	out := slack.BuildDigestParent(slack.DigestInput{
		Recovered: 3,
		Total:     3,
		OpenedAt:  t0,
		Closed:    true,
		Downtime:  41 * time.Minute,
		Rows: []slack.DigestRow{
			{Name: "api", Class: slack.RowRecovered},
		},
	})
	got := dump(t, out.Blocks) + dump(t, out.Attachments)
	if !strings.Contains(got, "All clear — 3 recovered") || !strings.Contains(got, "41m") {
		t.Fatalf("closed header wrong: %s", got)
	}
	if !strings.Contains(got, "#22af64") {
		t.Fatalf("closed digest stripe should be green: %s", got)
	}
}

func TestRenderDigestRows_capsWithTail(t *testing.T) {
	rows := make([]slack.DigestRow, 45)
	for i := range rows {
		rows[i] = slack.DigestRow{Name: string(rune('a'+i%26)) + "-svc", Class: slack.RowActive}
	}
	out := slack.BuildDigestParent(slack.DigestInput{Down: 45, Total: 45, OpenedAt: t0, Rows: rows, MaxRows: 40})
	got := dump(t, out.Attachments)
	if !strings.Contains(got, "…and 5 more") {
		t.Fatalf("expected truncation tail: %s", got)
	}
}

func TestBuildDigestDelta_batchesAllBucketsOneReply(t *testing.T) {
	blocks := slack.BuildDigestDelta(slack.DigestDeltaInput{
		NewlyDown: []string{"a", "b"},
		Recovered: []string{"c"},
		Flapped:   []string{"d"},
		Paused:    []string{"e"},
		Mentions:  []string{"<@U1>"},
	})
	if len(blocks) != 1 {
		t.Fatalf("delta should be a single block, got %d", len(blocks))
	}
	got := dump(t, blocks)
	for _, want := range []string{"+2 down", "1 recovered", "1 flapped", "1 paused", "<@U1>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("delta missing %q: %s", want, got)
		}
	}
}

func TestBuildDigestDelta_emptyIsNil(t *testing.T) {
	if blocks := slack.BuildDigestDelta(slack.DigestDeltaInput{}); blocks != nil {
		t.Fatalf("empty delta should be nil, got %v", blocks)
	}
}

func TestBuildDigestReminderReply_repingsUnion(t *testing.T) {
	blocks := slack.BuildDigestReminderReply(slack.DigestReminderInput{
		DownCount:    47,
		DownDuration: 30 * time.Minute,
		Mentions:     []string{"<!here>"},
	})
	got := dump(t, blocks)
	if !strings.Contains(got, "Still down after 30m") || !strings.Contains(got, "47 service") || !strings.Contains(got, "<!here>") {
		t.Fatalf("reminder reply wrong: %s", got)
	}
}
