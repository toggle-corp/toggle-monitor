package alertmanager

import (
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// Limiter is the per-channel sliding-window flood detector used by the
// AM webhook handler (ADR-0005 §"Rate limit"). It tracks one bucket
// per Slack channel slug; each bucket records the timestamps of recent
// Allow() approvals.
//
// On the (perChannel+1)th approval inside `window`, the bucket flips
// to engaged and Allow() returns (false, _). Engagement persists until
// older entries fall out of the window and the bucket count drops
// below the threshold again.
//
// While engaged, dropped alerts increment a per-channel counter. The
// handler calls NoticeDue(channel) periodically (or before deciding
// whether to post a throttle warning); NoticeDue returns the dropped
// count and resets it if `noticeEvery` has elapsed since the last
// notice — otherwise returns 0 and preserves the count.
//
// The breaker is a flood detector, not a burst smoother: once engaged,
// drops are silent except for the periodic notice. It does not render
// messages, manage threads, or interact with the coalesce pipeline.
type Limiter struct {
	perChannel  int
	window      time.Duration
	noticeEvery time.Duration
	now         func() time.Time
	mu          sync.Mutex
	buckets     map[string]*bucket
}

// bucket is the per-channel state. approvals is append-ordered (each
// entry is the now() at admission), so prune is O(k) where k is the
// number of expired entries — amortised O(1) per Allow.
type bucket struct {
	approvals  []time.Time
	dropped    int
	lastNotice time.Time // zero until first notice fires
	engaged    bool      // derived: true while in-window >= perChannel
}

// Snapshot is a debug-friendly read-only view of one channel's bucket,
// used by /healthz / /metrics surfacing. It never resets counters.
type Snapshot struct {
	InWindow int  // current approvals in window after prune
	Dropped  int  // since last NoticeDue fire
	Engaged  bool // true while InWindow >= perChannel
}

// NewLimiter builds a limiter from the config block. PerChannel == 0
// disables the breaker entirely — Allow always returns (true, false),
// NoticeDue returns 0, Snapshot returns the zero value. Window and
// NoticeEvery are required when PerChannel > 0; the config validator
// already enforces this, but the constructor panics on misuse so a
// programmatic misconfiguration is loud rather than silently broken.
//
// `now` is the time source; pass time.Now in production, a fake in
// tests (matching the convention in internal/scheduler).
func NewLimiter(cfg config.AlertmanagerRateLimit, now func() time.Time) *Limiter {
	if cfg.PerChannel < 0 {
		panic("alertmanager: NewLimiter: PerChannel must be >= 0")
	}
	if cfg.PerChannel > 0 {
		if cfg.Window.AsDuration() <= 0 {
			panic("alertmanager: NewLimiter: Window must be > 0 when PerChannel > 0")
		}
		if cfg.NoticeEvery.AsDuration() <= 0 {
			panic("alertmanager: NewLimiter: NoticeEvery must be > 0 when PerChannel > 0")
		}
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		perChannel:  cfg.PerChannel,
		window:      cfg.Window.AsDuration(),
		noticeEvery: cfg.NoticeEvery.AsDuration(),
		now:         now,
		buckets:     map[string]*bucket{},
	}
}

// Allow consults the bucket for `channel` and either approves the
// alert (returns (true, false)) or drops it (returns (false, true)).
// When dropped, the bucket's dropped counter increments.
//
// The second return value indicates "this was the moment of
// engagement transition" — true only on the first drop after a
// previously-allowing state. Useful for emitting the first throttle
// warning immediately instead of waiting for the first NoticeDue tick.
//
// When PerChannel == 0, Allow always returns (true, false) and does
// not allocate a bucket.
func (l *Limiter) Allow(channel string) (allowed bool, justEngaged bool) {
	if l.perChannel == 0 {
		return true, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[channel]
	if b == nil {
		b = &bucket{}
		l.buckets[channel] = b
	}

	now := l.now()
	b.prune(now, l.window)

	if len(b.approvals) >= l.perChannel {
		// Drop. justEngaged flips true exactly once on the
		// allowing→engaged transition.
		b.dropped++
		if !b.engaged {
			b.engaged = true
			return false, true
		}
		return false, false
	}

	// Allow: record the approval. If we were previously engaged,
	// dropping below threshold during prune flipped us back to
	// "allowing"; no public transition signal — callers detect re-open
	// implicitly via allowed=true.
	b.approvals = append(b.approvals, now)
	b.engaged = false
	return true, false
}

// NoticeDue returns the count of alerts dropped on this channel since
// the last notice fired, but only if `noticeEvery` has elapsed since
// then (or no notice has ever fired). Otherwise returns 0 and
// preserves the counter — drops keep accumulating until a notice
// actually fires.
//
// The handler calls NoticeDue right before deciding to post a throttle
// warning so the count never goes to Slack unaccompanied.
func (l *Limiter) NoticeDue(channel string) int {
	if l.perChannel == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[channel]
	if b == nil || b.dropped == 0 {
		return 0
	}
	now := l.now()
	if !b.lastNotice.IsZero() && now.Sub(b.lastNotice) < l.noticeEvery {
		return 0
	}
	n := b.dropped
	b.dropped = 0
	b.lastNotice = now
	return n
}

// Snapshot returns a read-only view of one channel's bucket. Used by
// diagnostic surfaces (/metrics, /healthz) — does not reset counters
// or mutate state beyond prune-on-read.
func (l *Limiter) Snapshot(channel string) Snapshot {
	if l.perChannel == 0 {
		return Snapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[channel]
	if b == nil {
		return Snapshot{}
	}
	b.prune(l.now(), l.window)
	// Engaged is the derived "currently at-or-above threshold"; we
	// expose the after-prune view so it matches what the next Allow
	// would see.
	engaged := len(b.approvals) >= l.perChannel
	return Snapshot{
		InWindow: len(b.approvals),
		Dropped:  b.dropped,
		Engaged:  engaged,
	}
}

// prune drops approvals older than `window` from the head of the
// slice. Slice is append-ordered (oldest first), so we advance an
// index and reslice — amortised O(1) per Allow.
func (b *bucket) prune(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	i := 0
	for i < len(b.approvals) && !b.approvals[i].After(cutoff) {
		i++
	}
	if i > 0 {
		// Copy down to release references and keep slice short.
		b.approvals = append(b.approvals[:0], b.approvals[i:]...)
	}
}
