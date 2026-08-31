// Package coalesce wires the per-channel group state machine
// (internal/group) into the running system: it owns the in-memory live
// groups, drives them from a single central evaluator goroutine, posts
// the digest messages to Slack, and persists every step so a restart
// reattaches to the existing digest instead of re-storming.
//
// The scheduler stages monitor transitions here (Down/Up/Pause) instead
// of posting per-monitor messages; the evaluator turns the group's
// Actions into Slack calls using the digest builders.
package coalesce

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/group"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// GroupStore is the persistence seam (satisfied by *store.Repo).
type GroupStore interface {
	CreateIncidentGroup(ctx context.Context, channelSlug string, openedAt time.Time) (int64, error)
	FindOpenIncidentGroup(ctx context.Context, channelSlug string) (store.IncidentGroupRow, bool, error)
	ListOpenIncidentGroups(ctx context.Context) ([]store.IncidentGroupRow, error)
	SaveIncidentGroup(ctx context.Context, g store.IncidentGroupRow) error
}

// Poster is the Slack seam. Every call addresses Slack by channel *slug*
// so the implementation can resolve the per-channel bot token; the ts
// identifies the digest message within that channel. PostDigest posts
// the parent and returns the resolved channel ID + message ts; Update
// edits the parent; Reply posts a threaded reply. Implemented in
// lifecycle over *slack.Client; faked in tests.
type Poster interface {
	PostDigest(ctx context.Context, channelSlug string, msg slack.ParentMessage) (channelID, ts string, err error)
	UpdateDigest(ctx context.Context, channelSlug, ts string, msg slack.ParentMessage) error
	Reply(ctx context.Context, channelSlug, ts string, blocks []slack.Block) error
}

// MemberInfo is the display data a monitor contributes to the digest.
type MemberInfo struct {
	Slug         string
	FriendlyName string
	Mentions     []string // raw Slack markup
	DetailURL    string
}

// liveGroup pairs an in-memory group with its persisted id and the
// per-member display info (lost across restart; repopulated lazily).
type liveGroup struct {
	id   int64
	g    *group.Group
	info map[string]MemberInfo
}

// OnDemandParentProbe is the hook the dispatcher invokes at
// pendingWait expiry for each "hot" parent — a dependsOn target shared
// by ≥2 pool entries that isn't itself in the pool. The hook is
// expected to (1) fire a bounded probe of the parent, (2) if down,
// drive alert.Apply + persist the EventOpen, and (3) fire
// push-propagation so the parent's children are drained from this
// pool before the flush-vs-promote decision.
//
// nil disables the on-demand probe pass; pool flushes proceed on the
// raw pool contents.
type OnDemandParentProbe func(ctx context.Context, parentSlug string)

// Manager owns the live groups, the per-channel dispatcher state, and
// the central evaluator. Under ADR-0004 the dispatcher decides
// individual-vs-group routing per channel; the legacy "always coalesce"
// path is replaced by the pending-pool decision.
type Manager struct {
	store          GroupStore
	poster         Poster
	sink           Sink // per-monitor individual notifier; see dispatch.go
	parentProbe    OnDemandParentProbe
	cfg            group.Config
	pendingWait    time.Duration
	burstThreshold int
	burstWindow    time.Duration
	groupMention   string // "" | "channel" | "here" | "none"
	log            *slog.Logger
	now            func() time.Time

	mu       sync.Mutex
	groups   map[string]*liveGroup    // open per-channel digests
	channels map[string]*channelState // dispatcher state per channel
}

// Options configures a Manager.
type Options struct {
	Store               GroupStore
	Poster              Poster
	Sink                Sink                // per-monitor individual notifier
	OnDemandParentProbe OnDemandParentProbe // hot-parent probe at pendingWait expiry
	Config              group.Config        // group state-machine intervals
	PendingWait         time.Duration       // dispatcher pending window; <=0 → 30s
	BurstThreshold      int                 // promote at-or-above this many monitors down in BurstWindow; 0 disables group-mode
	BurstWindow         time.Duration       // rolling window the burst count spans; <=0 → 5m, floored at PendingWait
	GroupMention        string              // "channel"|"here"|"none"; "" → no broadcast marker injected
	Logger              *slog.Logger
	Now                 func() time.Time
}

// defaultPendingWait mirrors config.DefaultPendingWait. Duplicating
// the constant keeps internal/coalesce from importing internal/config
// just for a default.
const defaultPendingWait = 30 * time.Second

// defaultBurstWindow mirrors config.DefaultBurstWindow, for the same
// reason as defaultPendingWait.
const defaultBurstWindow = 5 * time.Minute

// New builds a Manager.
func New(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	pw := opts.PendingWait
	if pw <= 0 {
		pw = defaultPendingWait
	}
	bw := opts.BurstWindow
	if bw <= 0 {
		bw = defaultBurstWindow
	}
	if bw < pw {
		bw = pw
	}
	return &Manager{
		store:          opts.Store,
		poster:         opts.Poster,
		sink:           opts.Sink,
		parentProbe:    opts.OnDemandParentProbe,
		cfg:            opts.Config,
		pendingWait:    pw,
		burstThreshold: opts.BurstThreshold,
		burstWindow:    bw,
		groupMention:   opts.GroupMention,
		log:            log,
		now:            now,
		groups:         map[string]*liveGroup{},
		channels:       map[string]*channelState{},
	}
}

// broadcastMarker maps the configured GroupMention policy to a raw
// Slack mention marker. Returns "" when the policy is empty or
// explicitly disabled — the caller skips injection in that case.
func (m *Manager) broadcastMarker() string {
	switch m.groupMention {
	case "channel":
		return "<!channel>"
	case "here":
		return "<!here>"
	}
	return ""
}

// SinkWired reports whether an individual-notification sink is
// configured. lifecycle uses it as a fail-fast boot guard: the real
// daemon must never run without one, because sub-threshold failures
// (the 90% case) flush through the sink and a nil sink discards them
// silently — a past ADR-0004 regression hit exactly this path.
// Tests may legitimately run sink-less; they just don't call this
// guard.
func (m *Manager) SinkWired() bool { return m.sink != nil }

// SetOnDemandParentProbe wires the on-demand parent-probe hook
// post-construction. Used by lifecycle when the hook captures the
// Manager itself (the probe needs to Route the parent's failure back
// through this same dispatcher), creating a chicken-and-egg around
// New(). Safe to call once at startup before the evaluator goroutine
// starts; concurrent calls are not supported.
func (m *Manager) SetOnDemandParentProbe(p OnDemandParentProbe) {
	m.parentProbe = p
}

// Down is a transitional compatibility shim: a synthesized EventOpen
// routed through the new dispatcher (ADR-0004). The scheduler will be
// updated in a follow-up commit to call Route directly with a full
// Entry (carrying Row + alert.Event); until then, lifecycle's
// groupSinkAdapter.Down lands here and the Sink path stays unused.
//
// Deprecated: use Route with an EventOpen Entry.
func (m *Manager) Down(ctx context.Context, channel string, info MemberInfo, at time.Time) {
	m.Route(ctx, channel, Entry{
		Member: info,
		Event:  &alert.Event{Type: alert.EventOpen, At: at},
	})
}

// Up stages a monitor recovery. Routes by channel mode:
//   - group: MarkUp on the live group.
//   - pending: retract from the pool (a failure that recovered before
//     it ever flushed).
//   - individual: no-op (recoveries for individually-notified monitors
//     flow through Route with EventResolve, not this method).
func (m *Manager) Up(ctx context.Context, channel, slug string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.channelStateFor(channel)
	cs.clearDown(slug)
	switch cs.mode {
	case modeGroup:
		if lg := m.groups[channel]; lg != nil {
			lg.g.MarkUp(slug, at)
		}
	case modePending:
		delete(cs.pending, slug)
		if len(cs.pending) == 0 {
			cs.mode = modeIndividual
			cs.pending = nil
			cs.pendingDue = time.Time{}
		}
	}
}

// Pause stages a dependsOn push-propagation pause: a parent's incident
// just opened, so each of its children's failures rolls into the
// parent's narrative. Routes by channel mode:
//   - group: MarkPaused — render as paused in the digest.
//   - pending: drop from the pool — the child never got individually
//     notified and won't (parent's failure is the page).
//   - individual: silent (per ADR-0004 Q11b).
func (m *Manager) Pause(ctx context.Context, channel, slug string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.channelStateFor(channel)
	// A paused child's failure belongs to its parent's incident, so it
	// must not count toward this channel's burst.
	cs.clearDown(slug)
	switch cs.mode {
	case modeGroup:
		if lg := m.groups[channel]; lg != nil {
			lg.g.MarkPaused(slug, at)
		}
	case modePending:
		delete(cs.pending, slug)
		if len(cs.pending) == 0 {
			cs.mode = modeIndividual
			cs.pending = nil
			cs.pendingDue = time.Time{}
		}
	}
}

// ensureGroupLocked returns the live group for a channel, loading a
// persisted open group or creating a new one. Caller holds m.mu.
func (m *Manager) ensureGroupLocked(ctx context.Context, channel string, at time.Time) *liveGroup {
	if lg := m.groups[channel]; lg != nil {
		return lg
	}
	// Reattach a persisted open group if one exists.
	if row, ok, err := m.store.FindOpenIncidentGroup(ctx, channel); err != nil {
		m.log.Warn("find open incident group", "channel", channel, "error", err)
	} else if ok {
		lg := m.fromRow(row)
		m.groups[channel] = lg
		return lg
	}
	id, err := m.store.CreateIncidentGroup(ctx, channel, at)
	if err != nil {
		m.log.Warn("create incident group", "channel", channel, "error", err)
		return nil
	}
	lg := &liveGroup{id: id, g: group.New(channel, at, m.cfg), info: map[string]MemberInfo{}}
	m.groups[channel] = lg
	return lg
}

// Reattach reloads every open group from the store on startup so
// post-restart deltas edit the existing digest. Display info (friendly
// names / mentions) is not persisted; rows fall back to the slug until
// the monitor next transitions and repopulates it.
func (m *Manager) Reattach(ctx context.Context) error {
	rows, err := m.store.ListOpenIncidentGroups(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		m.groups[row.ChannelSlug] = m.fromRow(row)
	}
	if len(rows) > 0 {
		m.log.Info("reattached open incident groups", "count", len(rows))
	}
	return nil
}

// RunEvaluator is the central evaluator goroutine: every `interval` it
// advances each live group and dispatches the resulting Slack actions.
// Blocks until ctx is cancelled.
func (m *Manager) RunEvaluator(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.evaluateAll(ctx)
		}
	}
}

// evaluateAll runs one evaluation pass over every live group AND every
// channel whose pending pool may have expired. The pending pass runs
// first because expirePending can promote a pool to a brand-new group
// that this same tick should then advance.
func (m *Manager) evaluateAll(ctx context.Context) {
	now := m.now()

	// 1. Pending-pool expiry pass.
	m.mu.Lock()
	pendingChannels := make([]string, 0, len(m.channels))
	for ch, cs := range m.channels {
		if cs.mode == modePending && !now.Before(cs.pendingDue) {
			pendingChannels = append(pendingChannels, ch)
		}
	}
	sort.Strings(pendingChannels)
	for _, ch := range pendingChannels {
		cs := m.channels[ch]
		if cs == nil || cs.mode != modePending {
			continue
		}
		m.expirePending(ctx, ch, cs, now)
	}
	m.mu.Unlock()

	// 2. Group evaluator pass.
	m.mu.Lock()
	channels := make([]string, 0, len(m.groups))
	for ch := range m.groups {
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	m.mu.Unlock()

	for _, ch := range channels {
		m.mu.Lock()
		lg := m.groups[ch]
		if lg == nil {
			m.mu.Unlock()
			continue
		}
		actions := lg.g.Evaluate(now)
		retire := m.dispatch(ctx, lg, actions, now)
		if err := m.store.SaveIncidentGroup(ctx, m.toRow(lg)); err != nil {
			m.log.Warn("save incident group", "channel", ch, "error", err)
		}
		if retire {
			delete(m.groups, ch)
			// Group retired: collapse the channel back to individual
			// mode so the next failure starts a fresh pending window.
			m.retireGroupChannel(ch)
		}
		m.mu.Unlock()
	}
}

// dispatch executes a group's actions against Slack and reports whether
// the group should be retired from memory (closed/discarded). Caller
// holds m.mu.
func (m *Manager) dispatch(ctx context.Context, lg *liveGroup, actions []group.Action, now time.Time) bool {
	retire := false
	for _, a := range actions {
		switch a.Kind {
		case group.ActionDiscard:
			retire = true
		case group.ActionPostDigest:
			m.postDigest(ctx, lg, false)
		case group.ActionUpdate:
			m.ensurePosted(ctx, lg)
			m.editDigest(ctx, lg, false)
			if blocks := slack.BuildDigestDelta(m.deltaInput(lg, a.Delta)); blocks != nil {
				m.reply(ctx, lg, blocks)
			}
		case group.ActionRemind:
			m.ensurePosted(ctx, lg)
			sb := lg.g.Scoreboard()
			m.reply(ctx, lg, slack.BuildDigestReminderReply(slack.DigestReminderInput{
				DownCount:    sb.Down,
				DownDuration: now.Sub(lg.g.OpenedAt),
				Mentions:     m.unionMentionsWithBroadcast(lg, lg.g.DownSlugs()),
			}))
		case group.ActionClose:
			m.ensurePosted(ctx, lg)
			m.editDigest(ctx, lg, true)
			sb := lg.g.Scoreboard()
			m.reply(ctx, lg, slack.BuildDigestCloseReply(now.Sub(lg.g.OpenedAt), sb.Recovered))
			retire = true
		}
	}
	return retire
}

// ensurePosted synthesizes the initial digest if a later action fired
// before (or despite) a failed PostDigest — the digest-side analogue of
// the notifier's fresh-parent fallback.
func (m *Manager) ensurePosted(ctx context.Context, lg *liveGroup) {
	if lg.g.DigestTS == "" {
		m.postDigest(ctx, lg, false)
	}
}

func (m *Manager) postDigest(ctx context.Context, lg *liveGroup, closed bool) {
	chID, ts, err := m.poster.PostDigest(ctx, lg.g.Channel, m.buildParent(lg, closed, true))
	if err != nil {
		m.log.Warn("post digest", "channel", lg.g.Channel, "error", err)
		return
	}
	lg.g.DigestChannel = chID
	lg.g.DigestTS = ts
}

func (m *Manager) editDigest(ctx context.Context, lg *liveGroup, closed bool) {
	if lg.g.DigestTS == "" {
		return
	}
	// Edits never re-ping (mentions=false); only open + reminder do.
	// Addressed by channel slug so the poster can resolve the bot token;
	// the digest ts identifies the message within that channel.
	if err := m.poster.UpdateDigest(ctx, lg.g.Channel, lg.g.DigestTS, m.buildParent(lg, closed, false)); err != nil {
		m.log.Warn("edit digest", "channel", lg.g.Channel, "error", err)
	}
}

func (m *Manager) reply(ctx context.Context, lg *liveGroup, blocks []slack.Block) {
	if lg.g.DigestTS == "" || len(blocks) == 0 {
		return
	}
	if err := m.poster.Reply(ctx, lg.g.Channel, lg.g.DigestTS, blocks); err != nil {
		m.log.Warn("digest reply", "channel", lg.g.Channel, "error", err)
	}
}

// buildParent renders the digest parent from current group state.
func (m *Manager) buildParent(lg *liveGroup, closed, withMentions bool) slack.ParentMessage {
	sb := lg.g.Scoreboard()
	rows := make([]slack.DigestRow, 0, len(lg.g.Members))
	for slug, mem := range lg.g.Members {
		rows = append(rows, slack.DigestRow{
			Name:      m.displayName(lg, slug),
			Class:     rowClass(mem.State),
			DetailURL: lg.info[slug].DetailURL,
		})
	}
	in := slack.DigestInput{
		Down:      sb.Down,
		Recovered: sb.Recovered,
		Total:     sb.Total,
		OpenedAt:  lg.g.OpenedAt,
		Rows:      rows,
		Closed:    closed,
	}
	if closed {
		in.Downtime = m.now().Sub(lg.g.OpenedAt)
	}
	if withMentions {
		in.Mentions = m.unionMentionsWithBroadcast(lg, lg.g.DownSlugs())
	}
	return slack.BuildDigestParent(in)
}

// deltaInput maps a group.Delta (slugs) into the rendered DigestDelta
// (friendly names), CC-ing only the newly-down owners per the mention
// policy.
func (m *Manager) deltaInput(lg *liveGroup, d group.Delta) slack.DigestDeltaInput {
	return slack.DigestDeltaInput{
		NewlyDown: m.names(lg, d.NewlyDown),
		Recovered: m.names(lg, d.Recovered),
		Flapped:   m.names(lg, d.Flapped),
		Paused:    m.names(lg, d.Paused),
		Mentions:  m.unionMentions(lg, d.NewlyDown),
	}
}

func (m *Manager) names(lg *liveGroup, slugs []string) []string {
	if len(slugs) == 0 {
		return nil
	}
	out := make([]string, len(slugs))
	for i, s := range slugs {
		out[i] = m.displayName(lg, s)
	}
	return out
}

func (m *Manager) displayName(lg *liveGroup, slug string) string {
	if info, ok := lg.info[slug]; ok && info.FriendlyName != "" {
		return info.FriendlyName
	}
	return slug
}

// unionMentionsWithBroadcast returns the owner-mention union prefixed
// with the configured broadcast marker (<!channel> / <!here>) when
// GroupMention is non-empty. The marker fires on group open and on
// each still-down reminder; edits never re-mention (and call
// unionMentions directly).
func (m *Manager) unionMentionsWithBroadcast(lg *liveGroup, slugs []string) []string {
	owners := m.unionMentions(lg, slugs)
	marker := m.broadcastMarker()
	if marker == "" {
		return owners
	}
	out := make([]string, 0, 1+len(owners))
	out = append(out, marker)
	out = append(out, owners...)
	return out
}

// unionMentions collects the deduplicated mentions of the given slugs,
// in first-seen order.
func (m *Manager) unionMentions(lg *liveGroup, slugs []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range slugs {
		for _, mention := range lg.info[s].Mentions {
			if _, dup := seen[mention]; dup {
				continue
			}
			seen[mention] = struct{}{}
			out = append(out, mention)
		}
	}
	return out
}

func rowClass(s group.MemberState) slack.DigestRowClass {
	switch s {
	case group.MemberRecovered:
		return slack.RowRecovered
	case group.MemberPaused:
		return slack.RowPaused
	default:
		return slack.RowActive
	}
}
