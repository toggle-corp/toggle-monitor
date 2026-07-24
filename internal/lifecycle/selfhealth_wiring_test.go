package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/selfhealth"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeShPoster records self-health digest posts and can be made to fail
// PostDigest to exercise the ensurePosted-style self-heal.
type fakeShPoster struct {
	mu       sync.Mutex
	posts    []slack.ParentMessage
	updates  int
	failPost bool
	nextTS   int
	lastTS   string
}

func (p *fakeShPoster) PostDigest(_ context.Context, _ string, msg slack.ParentMessage) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failPost {
		return "", "", errors.New("slack unreachable")
	}
	p.posts = append(p.posts, msg)
	p.nextTS++
	p.lastTS = "ts-" + string(rune('0'+p.nextTS))
	return "C1", p.lastTS, nil
}

func (p *fakeShPoster) UpdateDigest(_ context.Context, _, _ string, _ slack.ParentMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates++
	return nil
}

func (p *fakeShPoster) Reply(context.Context, string, string, []slack.Block) error { return nil }

func (p *fakeShPoster) postCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.posts)
}

// fakeShMetrics records self-health metric transitions.
type fakeShMetrics struct {
	degraded bool
	entries  int
}

func (m *fakeShMetrics) SetSelfDegraded(v bool) { m.degraded = v }
func (m *fakeShMetrics) SelfDegradedEntry()     { m.entries++ }

func shDetector() *selfhealth.Detector {
	return selfhealth.New(selfhealth.Config{Window: 90 * time.Second, MinMonitors: 3})
}

// TestSelfHealthNotifier_entryPostsOnceAndSetsMetric: entering degraded
// mode posts one open notice and flips the gauge + entry counter.
func TestSelfHealthNotifier_entryPostsOnceAndSetsMetric(t *testing.T) {
	d := shDetector()
	poster := &fakeShPoster{}
	met := &fakeShMetrics{}
	n := newSelfHealthNotifier(d, poster, "ops-health", "", met, nil, discardLogger())

	t0 := time.Unix(1000, 0)
	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)

	n.tick(context.Background(), t0)

	if !met.degraded {
		t.Error("expected self_degraded gauge set on entry")
	}
	if met.entries != 1 {
		t.Errorf("entry counter: got %d, want 1", met.entries)
	}
	if poster.postCount() != 1 {
		t.Errorf("expected exactly one open notice, got %d", poster.postCount())
	}
}

// TestSelfHealthNotifier_selfHealsFailedOpenPost: if the open post fails
// (Slack itself needs DNS), the notice is re-posted on the next tick
// once Slack returns — the ensurePosted-style self-heal.
func TestSelfHealthNotifier_selfHealsFailedOpenPost(t *testing.T) {
	d := shDetector()
	poster := &fakeShPoster{failPost: true}
	met := &fakeShMetrics{}
	n := newSelfHealthNotifier(d, poster, "ops-health", "", met, nil, discardLogger())

	t0 := time.Unix(1000, 0)
	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)
	n.tick(context.Background(), t0)

	if poster.postCount() != 0 {
		t.Fatalf("post should have failed, got %d posts", poster.postCount())
	}
	// Slack returns; a later tick (still degraded) re-posts.
	poster.failPost = false
	n.tick(context.Background(), t0.Add(5*time.Second))
	if poster.postCount() != 1 {
		t.Errorf("expected self-heal re-post once Slack returned, got %d", poster.postCount())
	}
}

// TestSelfHealthNotifier_noChannel_metricAndLogOnly: with no channel
// configured, entry sets the metric but posts nothing to Slack.
func TestSelfHealthNotifier_noChannel_metricAndLogOnly(t *testing.T) {
	d := shDetector()
	poster := &fakeShPoster{}
	met := &fakeShMetrics{}
	n := newSelfHealthNotifier(d, poster, "", "", met, nil, discardLogger())

	t0 := time.Unix(1000, 0)
	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)
	n.tick(context.Background(), t0)

	if !met.degraded {
		t.Error("expected gauge set even with no channel")
	}
	if poster.postCount() != 0 {
		t.Errorf("no channel → no Slack post, got %d", poster.postCount())
	}
}

// TestSelfHealthNotifier_exitClosesAndResumes: exiting degraded mode
// clears the gauge and posts the close notice.
func TestSelfHealthNotifier_exitClosesAndResumes(t *testing.T) {
	d := shDetector()
	poster := &fakeShPoster{}
	met := &fakeShMetrics{}
	n := newSelfHealthNotifier(d, poster, "ops-health", "", met, nil, discardLogger())

	t0 := time.Unix(1000, 0)
	d.Report("a", probe.FailKindDNS, false, t0)
	d.Report("b", probe.FailKindDNS, false, t0)
	d.Report("c", probe.FailKindDNS, false, t0)
	n.tick(context.Background(), t0)

	// Recover after a full window.
	tLate := t0.Add(120 * time.Second)
	d.Report("a", probe.FailKindNone, true, tLate)
	n.tick(context.Background(), tLate)

	if met.degraded {
		t.Error("expected gauge cleared on exit")
	}
	// One open + one close = 2 posts (close edits/posts the resolution).
	if poster.postCount() < 1 {
		t.Errorf("expected at least the open post, got %d", poster.postCount())
	}
	if poster.updates == 0 {
		t.Errorf("expected the open notice to be edited closed on exit")
	}
}

// TestSelfHealthNotifier_commitCallbackFiresForIsolatedFailure: an
// isolated (sub-threshold) DNS failure is committed — the notifier
// invokes the commit callback for its slug so the scheduler pages it
// normally, ~W late.
func TestSelfHealthNotifier_commitCallbackFiresForIsolatedFailure(t *testing.T) {
	d := shDetector()
	poster := &fakeShPoster{}
	met := &fakeShMetrics{}
	var committed []string
	commit := func(_ context.Context, slug string) { committed = append(committed, slug) }
	n := newSelfHealthNotifier(d, poster, "ops-health", "", met, commit, discardLogger())

	t0 := time.Unix(1000, 0)
	d.Report("lonely", probe.FailKindDNS, false, t0)
	n.tick(context.Background(), t0)

	if len(committed) != 1 || committed[0] != "lonely" {
		t.Fatalf("commit callback: got %v, want [lonely]", committed)
	}
	if met.degraded {
		t.Error("one DNS failure must not enter degraded mode")
	}
}
