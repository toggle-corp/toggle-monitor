package coalesce

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/group"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// --- fakes -----------------------------------------------------------

type fakeStore struct {
	mu     sync.Mutex
	nextID int64
	rows   map[int64]*store.IncidentGroupRow // by id
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[int64]*store.IncidentGroupRow{}} }

func (f *fakeStore) CreateIncidentGroup(_ context.Context, channel string, at time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.rows[f.nextID] = &store.IncidentGroupRow{ID: f.nextID, ChannelSlug: channel, OpenedAt: at}
	return f.nextID, nil
}

func (f *fakeStore) FindOpenIncidentGroup(_ context.Context, channel string) (store.IncidentGroupRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ChannelSlug == channel && !r.Closed {
			return *r, true, nil
		}
	}
	return store.IncidentGroupRow{}, false, nil
}

func (f *fakeStore) ListOpenIncidentGroups(context.Context) ([]store.IncidentGroupRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.IncidentGroupRow
	for _, r := range f.rows {
		if !r.Closed {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) SaveIncidentGroup(_ context.Context, g store.IncidentGroupRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := g
	f.rows[g.ID] = &cp
	return nil
}

type fakePoster struct {
	posts    int
	updates  int
	replies  int
	lastPost slack.ParentMessage
}

func (p *fakePoster) PostDigest(_ context.Context, _ string, msg slack.ParentMessage) (string, string, error) {
	p.posts++
	p.lastPost = msg
	return "C1", "ts1", nil
}
func (p *fakePoster) UpdateDigest(context.Context, string, string, slack.ParentMessage) error {
	p.updates++
	return nil
}
func (p *fakePoster) Reply(context.Context, string, string, []slack.Block) error {
	p.replies++
	return nil
}

// --- helpers ---------------------------------------------------------

// newManager wires a Manager that promotes any 2-monitor burst into a
// group, so the original "always-coalesce" behavior under test reads
// the same way in the ADR-0004 world. PendingWait stays at 30s
// (the dispatcher's wait, not the group's legacy GroupWait).
func newManager(t *testing.T, clock *time.Time) (*Manager, *fakeStore, *fakePoster) {
	t.Helper()
	fs := newFakeStore()
	fp := &fakePoster{}
	m := New(Options{
		Store:          fs,
		Poster:         fp,
		Config:         group.Config{GroupInterval: 5 * time.Minute, RepeatInterval: 30 * time.Minute},
		PendingWait:    30 * time.Second,
		BurstThreshold: 2,
		Now:            func() time.Time { return *clock },
	})
	return m, fs, fp
}

var base = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

// --- tests -----------------------------------------------------------

func TestManagerPostsOneDigestForBurst(t *testing.T) {
	clock := base
	m, _, fp := newManager(t, &clock)
	ctx := context.Background()

	m.Down(ctx, "ops", MemberInfo{Slug: "a", FriendlyName: "API", Mentions: []string{"<!here>"}}, base)
	m.Down(ctx, "ops", MemberInfo{Slug: "b", FriendlyName: "Web"}, base)

	// Before group_wait: no post.
	clock = base.Add(10 * time.Second)
	m.evaluateAll(ctx)
	if fp.posts != 0 {
		t.Fatalf("posted during group_wait: %d", fp.posts)
	}

	// After group_wait: exactly one digest for both monitors.
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if fp.posts != 1 {
		t.Fatalf("want 1 digest post, got %d", fp.posts)
	}
}

func TestManagerClosesAfterRecovery(t *testing.T) {
	clock := base
	m, fs, fp := newManager(t, &clock)
	ctx := context.Background()

	// Burst of 2 = threshold; promotes to group at pendingWait expiry.
	m.Down(ctx, "ops", MemberInfo{Slug: "a", FriendlyName: "API"}, base)
	m.Down(ctx, "ops", MemberInfo{Slug: "b", FriendlyName: "DB"}, base)
	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx) // promote + post digest

	m.Up(ctx, "ops", "a", base.Add(1*time.Minute))
	m.Up(ctx, "ops", "b", base.Add(1*time.Minute))

	// Before debounce ends (1m + 5m = 6m): stays open.
	clock = base.Add(3 * time.Minute)
	m.evaluateAll(ctx)
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 1 {
		t.Fatalf("group closed prematurely")
	}

	// After debounce: close (edit green + close reply), retired from memory.
	clock = base.Add(7 * time.Minute)
	m.evaluateAll(ctx)
	if fp.updates < 1 || fp.replies < 1 {
		t.Fatalf("close should edit + reply: updates=%d replies=%d", fp.updates, fp.replies)
	}
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("closed group should not be open in store: %d", len(open))
	}
	m.mu.Lock()
	_, stillLive := m.groups["ops"]
	m.mu.Unlock()
	if stillLive {
		t.Fatal("closed group should be retired from memory")
	}
}

func TestManagerReattachLoadsOpenGroups(t *testing.T) {
	clock := base
	m, fs, _ := newManager(t, &clock)
	ctx := context.Background()

	// Pre-seed an open group as if a prior process had posted it.
	id, _ := fs.CreateIncidentGroup(ctx, "ops", base)
	_ = fs.SaveIncidentGroup(ctx, store.IncidentGroupRow{
		ID: id, ChannelSlug: "ops", OpenedAt: base, Posted: true,
		DigestChannel: "C1", DigestTS: "ts1",
		Members: []store.IncidentGroupMemberRow{
			{MonitorSlug: "a", State: "down", JoinedAt: base, ChangedAt: base, Rendered: "active"},
		},
	})

	if err := m.Reattach(ctx); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	m.mu.Lock()
	lg := m.groups["ops"]
	m.mu.Unlock()
	if lg == nil || !lg.g.Posted || lg.g.DigestTS != "ts1" {
		t.Fatalf("reattached group missing/incomplete: %+v", lg)
	}
	if got := lg.g.Scoreboard(); got.Down != 1 {
		t.Fatalf("reattached scoreboard = %+v", got)
	}
}

func TestManagerDiscardsBlip(t *testing.T) {
	clock := base
	m, fs, fp := newManager(t, &clock)
	ctx := context.Background()

	m.Down(ctx, "ops", MemberInfo{Slug: "a"}, base)
	m.Up(ctx, "ops", "a", base.Add(5*time.Second)) // recovered before group_wait

	clock = base.Add(31 * time.Second)
	m.evaluateAll(ctx)
	if fp.posts != 0 {
		t.Fatalf("blip should not post a digest, got %d", fp.posts)
	}
	if open, _ := fs.ListOpenIncidentGroups(ctx); len(open) != 0 {
		t.Fatalf("blip group should be closed in store")
	}
}
