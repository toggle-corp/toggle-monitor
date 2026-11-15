package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// ChannelInfo describes one resolved Slack destination.
type ChannelInfo struct {
	ID    string              // C0123…
	Token secret.SecretString // bot token resolved from tokenEnv
}

// ThreadStore is the slim seam the notifier uses to persist Slack
// thread refs (uptime + SSL) after posting the parent message, to mark
// an uptime parent whose post failed, and to look up which monitors
// depend on a given one (so the parent message can call out "this
// alert will pause X, Y"). Production wires this to *store.Repo; tests
// inject a fake.
type ThreadStore interface {
	SetUptimeThread(ctx context.Context, monitorSlug string, openedAt time.Time, channelID, ts string) error
	MarkUptimeOpenFailed(ctx context.Context, monitorSlug string, openedAt time.Time) error
	SetSSLThread(ctx context.Context, monitorSlug, channelID, ts string) error
	ListChildrenOf(ctx context.Context, parentSlug string) ([]string, error)
}

// DefaultDependentsNoteMax is the fallback cap when the config leaves
// slack.dependentsNoteMax at zero. Keeps the dependents line readable
// when a parent monitor has many children.
const DefaultDependentsNoteMax = 5

// freshParentBanner is the late-notice banner rendered at the top of
// the body when the notifier had to synthesize a parent because the
// initial Open delivery failed. A parent posted for a monitor whose
// Open was never sent carries no banner: it is the first announcement.
const freshParentBanner = "⚠️ Initial notification delivery failed. This alert is current."

// OrphanResolvesBanner is the late-notice banner on the grouped
// recovery message the dispatcher posts when several monitors recover
// whose initial Open delivery failed.
const OrphanResolvesBanner = "⚠️ Initial notification delivery failed. These monitors are back up."

// NotifierObserver is the slim metrics seam used by the notifier. Each
// Notify call increments SlackPost once (worst outcome wins). A
// fresh-parent fallback additionally increments SlackFreshParent.
type NotifierObserver interface {
	SlackPost(result, reason string)
	SlackFreshParent(kind string)
}

// Notifier turns alert events into Slack API calls.
type Notifier struct {
	client            *Client
	store             ThreadStore
	channels          func(slug string) (ChannelInfo, bool)
	bodyMaxChars      int
	dependentsNoteMax int
	publicBase        string
	log               *slog.Logger
	observer          NotifierObserver

	mu       sync.Mutex
	monitors map[string]*monitorDelivery // slug → uptime delivery state
}

// monitorDelivery serializes one monitor's uptime deliveries and
// remembers what became of its latest incident's parent message.
//
// Deliveries for one monitor reach the notifier from several goroutines:
// the monitor's own scheduler tick, the dispatcher's evaluator flushing a
// pending pool, the on-demand parent probe and the self-health commit.
// Each sink call carries a row snapshot read before its own transition
// committed, so a recovery can carry a snapshot taken while its Open was
// still being posted: no thread ts and no failed-delivery mark, although
// a DOWN is about to land. Holding the lock across a delivery makes a
// recovery wait for an Open in flight, and parent records what that Open
// produced.
//
// A record lives in Notifier.monitors until its monitor is removed. It
// leaves the map only while its lock is held, and is marked retired at
// the same time, so a record that is locked and not retired is the
// map's entry for its slug.
type monitorDelivery struct {
	sync.Mutex
	parent  uptimeParent
	retired bool // dropped from Notifier.monitors; lockDelivery skips it
}

// uptimeParent is what this process knows about one incident's parent
// message, beyond the row snapshot a delivery is handed.
type uptimeParent struct {
	incident time.Time // OpenedAt of the incident; zero when nothing is recorded
	channel  string    // parent message posted for it, when one was
	ts       string
	failed   bool // a parent post for it failed
	resolved bool // its recovery was delivered
}

// sameIncident reports whether the record describes the incident that
// opened at `at`. A row's opened_at round-trips through Postgres at
// microsecond precision, hence the tolerance.
func (p uptimeParent) sameIncident(at time.Time) bool {
	if p.incident.IsZero() || at.IsZero() {
		return false
	}
	d := p.incident.Sub(at)
	return d < time.Millisecond && d > -time.Millisecond
}

// incidentStart identifies the incident an uptime event belongs to: the
// row's OpenedAt, or for an Open from an up monitor (whose row has none
// yet) the Open's own time, which becomes OpenedAt when it commits.
func incidentStart(m MonitorView, ev *alert.Event) time.Time {
	if !m.OpenedAt.IsZero() {
		return m.OpenedAt
	}
	return ev.At
}

// delivery returns the monitor's delivery state, creating it on first use.
func (n *Notifier) delivery(slug string) *monitorDelivery {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.monitors == nil {
		n.monitors = map[string]*monitorDelivery{}
	}
	d, ok := n.monitors[slug]
	if !ok {
		d = &monitorDelivery{}
		n.monitors[slug] = d
	}
	return d
}

// lockDelivery returns the monitor's delivery state, locked. A record
// retired while the caller waited for its lock is passed over for the
// one that replaced it, so one monitor's deliveries never run on two
// records at once.
func (n *Notifier) lockDelivery(slug string) *monitorDelivery {
	for {
		d := n.delivery(slug)
		d.Lock()
		if !d.retired {
			return d
		}
		d.Unlock()
	}
}

// retireDelivery drops the monitor's delivery record d, which the
// caller locked through lockDelivery, and unlocks it. A delivery for
// the slug that comes after starts from a fresh record.
func (n *Notifier) retireDelivery(slug string, d *monitorDelivery) {
	n.mu.Lock()
	delete(n.monitors, slug)
	n.mu.Unlock()
	d.retired = true
	d.Unlock()
}

// UptimeParentPosted reports whether this process posted an uptime
// parent for the monitor's incident that opened at openedAt and holds
// its thread ref. The failed-delivery mark covers both a failed post
// and a delivered parent whose ref could not be stored; the dispatcher
// asks this to tell them apart, and sends the second kind's recovery
// here, where the parent gets its edit. It waits for a delivery of the
// monitor still in flight.
func (n *Notifier) UptimeParentPosted(slug string, openedAt time.Time) bool {
	d := n.lockDelivery(slug)
	defer d.Unlock()
	return d.parent.sameIncident(openedAt) && d.parent.ts != ""
}

// NotifierOptions configures a Notifier.
type NotifierOptions struct {
	Client            *Client
	Store             ThreadStore
	Channels          func(slug string) (ChannelInfo, bool) // slug → ChannelInfo
	BodyMaxChars      int
	DependentsNoteMax int    // 0 → DefaultDependentsNoteMax
	PublicBase        string // empty → omit [View details] buttons
	Logger            *slog.Logger
	// Observer receives slack_post_total + slack_fresh_parent_total
	// increments. nil disables emission (tests that don't care can pass
	// nil; production wires *observability.Metrics).
	Observer NotifierObserver
}

// NewNotifier builds a Notifier from the resolved channel set.
func NewNotifier(opts NotifierOptions) *Notifier {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	max := opts.DependentsNoteMax
	if max <= 0 {
		max = DefaultDependentsNoteMax
	}
	return &Notifier{
		client:            opts.Client,
		store:             opts.Store,
		channels:          opts.Channels,
		bodyMaxChars:      opts.BodyMaxChars,
		dependentsNoteMax: max,
		publicBase:        opts.PublicBase,
		log:               log,
		observer:          opts.Observer,
	}
}

// observePost emits the per-Notify outcome counter. err==nil → success.
// err *SlackError → fail with the matching Kind.String() reason. Other
// errors → fail with reason="unknown".
func (n *Notifier) observePost(err error) {
	if n.observer == nil {
		return
	}
	if err == nil {
		n.observer.SlackPost("success", "ok")
		return
	}
	var se *SlackError
	if errors.As(err, &se) {
		n.observer.SlackPost("fail", se.Kind.String())
		return
	}
	n.observer.SlackPost("fail", "unknown")
}

// observeFreshParent emits the fresh-parent counter. kind = "uptime" or "ssl".
func (n *Notifier) observeFreshParent(kind string) {
	if n.observer == nil {
		return
	}
	n.observer.SlackFreshParent(kind)
}

// MonitorView is the slim shape the notifier needs about a monitor at
// the moment an alert event fires. Built by the scheduler from
// store.MonitorRow + alert.Event.
type MonitorView struct {
	Slug         string
	FriendlyName string
	Tags         []string
	URL          string
	OpenedAt     time.Time
	StatusCode   int
	StatusText   string // e.g. "Service Unavailable"
	LastError    string
	ResponseBody string

	// Set when the monitor is currently down and the parent message
	// has been recorded (used for reminders + resolve).
	UptimeThreadChannel string
	UptimeThreadTS      string
	// UptimeOpenFailed reports that this incident's parent message was
	// sent and the post failed, or that a delivered parent's thread ref
	// could not be stored. With no UptimeThreadTS and no mark, no message
	// about the incident was ever attempted.
	UptimeOpenFailed bool

	// SSL-side counterparts.
	SSLThreadChannel string
	SSLThreadTS      string
	SSLIssuer        string
	SSLSubject       string
}

// SSLView is the slim shape NotifySSL needs about a cert event.
type SSLView struct {
	ExpiresAt time.Time
}

// Notify dispatches the right Slack call(s) for an alert event:
//   - EventOpen:     post a new parent, persist the thread ref; nothing
//     for an incident whose recovery was already delivered, or whose
//     parent this process already posted.
//   - EventReminder: post a thread reply.
//   - EventResolve:  edit the parent and post a thread reply; nothing
//     at all for an incident no message was ever attempted for.
//
// Deliveries for one monitor run one at a time, and the thread ref or
// failed-delivery mark of a parent this process posted fills in a row
// snapshot that predates it (see monitorDelivery).
//
// channelSlug identifies the monitor's configured Slack destination;
// mentions are pre-resolved raw markup (`<!here>`, `<@U…>`, etc).
func (n *Notifier) Notify(ctx context.Context, channelSlug string, mentions []string, m MonitorView, ev *alert.Event) error {
	if ev == nil {
		return nil
	}
	ch, ok := n.channels(channelSlug)
	if !ok {
		return fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}

	d := n.lockDelivery(m.Slug)
	defer d.Unlock()
	incident := incidentStart(m, ev)
	known := d.parent.sameIncident(incident)
	if known && m.UptimeThreadTS == "" {
		if d.parent.ts != "" {
			m.UptimeThreadChannel, m.UptimeThreadTS = d.parent.channel, d.parent.ts
		} else if d.parent.failed {
			m.UptimeOpenFailed = true
		}
	}

	var err error
	switch ev.Type {
	case alert.EventOpen:
		if known && (d.parent.resolved || d.parent.ts != "") {
			// The incident already recovered, or its parent is already
			// up: a DOWN posted now would stand with nothing left to
			// edit it, or duplicate the one posted.
			n.log.Info("open of an incident already announced or resolved: posting nothing", "monitor", m.Slug)
			return nil
		}
		err = n.notifyOpen(ctx, ch, mentions, m, ev, &d.parent)
	case alert.EventReminder:
		err = n.notifyReminder(ctx, ch, mentions, m, ev, &d.parent)
	case alert.EventResolve:
		d.parent = uptimeParent{incident: incident, resolved: true}
		if m.UptimeThreadTS == "" && !m.UptimeOpenFailed {
			// No parent and no failed delivery: no message about this
			// incident was ever attempted, so nobody needs to hear it
			// ended. Nothing is posted, so nothing is counted.
			n.log.Info("resolve of a never-announced incident: posting nothing", "monitor", m.Slug)
			return nil
		}
		err = n.notifyResolve(ctx, ch, mentions, m, ev)
	default:
		return nil
	}
	// Context cancellation is not a delivery failure; skip the metric
	// so a SIGTERM doesn't show up as a "fail" in the dashboards.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	n.observePost(err)
	return err
}

func (n *Notifier) notifyOpen(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event, rec *uptimeParent) error {
	in := DownInput{
		FriendlyName: m.FriendlyName,
		Tags:         m.Tags,
		URL:          m.URL,
		Mentions:     mentions,
		StatusCode:   ev.StatusCode,
		StatusText:   m.StatusText,
		FailureAt:    incidentStart(m, ev),
		LastError:    ev.Error,
		ResponseBody: m.ResponseBody,
		BodyMaxChars: n.bodyMaxChars,
		DetailURL:    n.DetailURL(m.Slug),
		Note:         n.dependentsNote(ctx, m.Slug, "⏸ Pauses dependents"),
	}
	msg := BuildDownParent(in)
	res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	incident := incidentStart(m, ev)
	if err != nil {
		*rec = uptimeParent{incident: incident, failed: true}
		n.markOpenFailed(ctx, m.Slug, incident)
		return err
	}
	*rec = uptimeParent{incident: incident, channel: res.Channel, ts: res.TS}
	n.storeUptimeThread(ctx, m.Slug, incident, res.Channel, res.TS)
	return nil
}

// persistAttempts and persistBackoff bound the retry of a thread-ref
// write after a delivered parent post: three attempts, 100ms then 200ms
// apart.
const (
	persistAttempts = 3
	persistBackoff  = 100 * time.Millisecond
)

// persistThreadRef runs write, retrying a failure briefly. A ref lost
// here leaves the parent with nothing on file that can find it again.
func persistThreadRef(ctx context.Context, write func(context.Context) error) error {
	delay := persistBackoff
	for attempt := 1; ; attempt++ {
		err := write(ctx)
		if err == nil || attempt == persistAttempts {
			return err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		delay *= 2
	}
}

// storeUptimeThread persists the thread ref of a delivered uptime
// parent for the incident that opened at incident. When the write keeps
// failing, the row is marked the way a failed post marks it: the
// recovery then posts the late-notice resolve rather than staying
// silent while the DOWN stands. This process still edits the parent,
// from the monitor's delivery record.
func (n *Notifier) storeUptimeThread(ctx context.Context, slug string, incident time.Time, channel, ts string) {
	err := persistThreadRef(ctx, func(ctx context.Context) error {
		return n.store.SetUptimeThread(ctx, slug, incident, channel, ts)
	})
	if err == nil {
		return
	}
	n.log.Warn("persist uptime thread ref", "monitor", slug, "error", err)
	n.markOpenFailed(ctx, slug, incident)
}

func (n *Notifier) notifyReminder(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event, rec *uptimeParent) error {
	if m.UptimeThreadTS == "" {
		// No parent recorded: either the initial Open delivery failed,
		// or no Open was ever sent (a dependsOn child paused before its
		// Open flushed, now resumed and still down). Post the Down
		// parent, persist its ts, and let subsequent reminders thread
		// onto it.
		return n.postFreshUptimeParent(ctx, ch, mentions, m, rec)
	}
	blocks := BuildReminderReply(ReminderInput{
		DownDuration:  ev.At.Sub(m.OpenedAt),
		LastCheckedAt: ev.At,
		LastError:     ev.Error,
	})
	_, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID: ch.ID,
		ThreadTS:  m.UptimeThreadTS,
		Blocks:    blocks,
	})
	return err
}

// postFreshUptimeParent synthesizes a Down parent when a reminder
// fires but no parent ts is on file. After a failed Open delivery it
// carries the late-notice banner and counts as a fresh-parent fallback;
// for a monitor never announced it is a plain Down parent. Persists the
// new ts so subsequent reminders thread normally, or marks the delivery
// failed when the post fails.
func (n *Notifier) postFreshUptimeParent(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, rec *uptimeParent) error {
	banner := ""
	if m.UptimeOpenFailed {
		banner = freshParentBanner
	}
	in := DownInput{
		FriendlyName: m.FriendlyName,
		Tags:         m.Tags,
		URL:          m.URL,
		Mentions:     mentions,
		StatusCode:   m.StatusCode,
		StatusText:   m.StatusText,
		FailureAt:    m.OpenedAt,
		LastError:    m.LastError,
		ResponseBody: m.ResponseBody,
		BodyMaxChars: n.bodyMaxChars,
		DetailURL:    n.DetailURL(m.Slug),
		Note:         n.dependentsNote(ctx, m.Slug, "⏸ Pauses dependents"),
		Banner:       banner,
	}
	msg := BuildDownParent(in)
	res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		*rec = uptimeParent{incident: m.OpenedAt, failed: true}
		n.markOpenFailed(ctx, m.Slug, m.OpenedAt)
		return err
	}
	*rec = uptimeParent{incident: m.OpenedAt, channel: res.Channel, ts: res.TS}
	if m.UptimeOpenFailed {
		n.observeFreshParent("uptime")
	}
	n.storeUptimeThread(ctx, m.Slug, m.OpenedAt, res.Channel, res.TS)
	return nil
}

// markOpenFailed records that the uptime parent post for the incident
// that opened at incident failed, or that the thread ref of a delivered
// one could not be stored, so its eventual recovery reports the
// incident instead of posting nothing. Best-effort: an unrecorded
// failure leaves that recovery silent. A cancelled context (shutdown
// mid-post) records nothing.
func (n *Notifier) markOpenFailed(ctx context.Context, slug string, incident time.Time) {
	if ctx.Err() != nil {
		return
	}
	if err := n.store.MarkUptimeOpenFailed(ctx, slug, incident); err != nil {
		n.log.Warn("persist uptime open failure", "monitor", slug, "error", err)
	}
}

func (n *Notifier) notifyResolve(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event) error {
	if m.UptimeThreadTS == "" {
		// Without a parent ref we post a standalone resolve message
		// that mirrors the resolve-edit body so operators still get
		// status/downtime/details. Carries the banner so they know
		// why the open went missing.
		n.log.Warn("resolve thread ref missing: posting standalone resolve", "monitor", m.Slug)
		resolveIn := ResolveInput{
			DownInput: DownInput{
				FriendlyName: m.FriendlyName,
				Tags:         m.Tags,
				URL:          m.URL,
				Mentions:     mentions,
				StatusCode:   m.StatusCode,
				StatusText:   m.StatusText,
				FailureAt:    m.OpenedAt,
				LastError:    m.LastError,
				ResponseBody: m.ResponseBody,
				BodyMaxChars: n.bodyMaxChars,
				DetailURL:    n.DetailURL(m.Slug),
				Banner:       freshParentBanner,
			},
			ResolveAt: ev.At,
			Downtime:  ev.Downtime,
		}
		msg := BuildResolveEdit(resolveIn)
		_, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID:   ch.ID,
			Blocks:      msg.Blocks,
			Attachments: msg.Attachments,
		})
		return err
	}

	resolveIn := ResolveInput{
		DownInput: DownInput{
			FriendlyName: m.FriendlyName,
			Tags:         m.Tags,
			URL:          m.URL,
			Mentions:     mentions,
			StatusCode:   m.StatusCode,
			StatusText:   m.StatusText,
			FailureAt:    m.OpenedAt,
			LastError:    m.LastError,
			ResponseBody: m.ResponseBody,
			BodyMaxChars: n.bodyMaxChars,
			DetailURL:    n.DetailURL(m.Slug),
			Note:         n.dependentsNote(ctx, m.Slug, "▶ Resumes dependents"),
		},
		ResolveAt: ev.At,
		Downtime:  ev.Downtime,
	}

	resolveMsg := BuildResolveEdit(resolveIn)
	if err := n.client.UpdateMessage(ctx, ch.Token, UpdateMessageInput{
		ChannelID:   m.UptimeThreadChannel,
		TS:          m.UptimeThreadTS,
		Blocks:      resolveMsg.Blocks,
		Attachments: resolveMsg.Attachments,
	}); err != nil {
		return fmt.Errorf("update parent on resolve: %w", err)
	}
	if _, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID: m.UptimeThreadChannel,
		ThreadTS:  m.UptimeThreadTS,
		Blocks:    BuildResolveReply(resolveIn),
	}); err != nil {
		return fmt.Errorf("post resolve reply: %w", err)
	}
	return nil
}

// DetailURL returns the web UI link for a monitor, or "" when no public
// base URL is configured.
func (n *Notifier) DetailURL(monitorSlug string) string {
	if n.publicBase == "" {
		return ""
	}
	return n.publicBase + "/monitor/" + monitorSlug
}

// dependentsNote looks up the children that depend on this monitor
// and renders the cascading-effect line that the parent message
// surfaces just above its footer. Returns "" when the monitor has
// no dependents (the common case — most monitors are leaves) or when
// the lookup fails (best-effort: a DB hiccup here shouldn't block the
// Slack post). prefix is the verb-bearing lead, e.g.
// "⏸ Pauses dependents" or "▶ Resumes dependents".
//
// Truncates to dependentsNoteMax entries; the remainder collapses
// into a "…and N more" tail so the line stays scannable when a
// parent has many children.
func (n *Notifier) dependentsNote(ctx context.Context, parentSlug, prefix string) string {
	children, err := n.store.ListChildrenOf(ctx, parentSlug)
	if err != nil {
		n.log.Warn("list children of monitor", "monitor", parentSlug, "error", err)
		return ""
	}
	return FormatDependentsNote(prefix, children, n.dependentsNoteMax)
}

// postFreshSSLParent synthesizes an SSL Down parent (banner + same body)
// when a reminder fires with no parent ts on file. Symmetric with
// postFreshUptimeParent.
func (n *Notifier) postFreshSSLParent(ctx context.Context, ch ChannelInfo, in SSLDownInput, slug string) error {
	in.Banner = freshParentBanner
	msg := BuildSSLParent(in)
	res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return err
	}
	n.observeFreshParent("ssl")
	n.storeSSLThread(ctx, slug, res.Channel, res.TS)
	return nil
}

// storeSSLThread persists a delivered SSL parent's thread ref. Unlike
// the uptime side it needs no mark when the write keeps failing: an SSL
// recovery with no ref on file already posts a standalone resolve.
func (n *Notifier) storeSSLThread(ctx context.Context, slug, channel, ts string) {
	err := persistThreadRef(ctx, func(ctx context.Context) error {
		return n.store.SetSSLThread(ctx, slug, channel, ts)
	})
	if err != nil {
		n.log.Warn("persist ssl thread ref", "monitor", slug, "error", err)
	}
}

// FormatDependentsNote renders the "<prefix>: `a`, `b`, …and N more"
// line. Returns "" when slugs is empty. Exported so the slack-test
// CLI can mirror the same truncation logic without duplicating it.
func FormatDependentsNote(prefix string, slugs []string, max int) string {
	if len(slugs) == 0 {
		return ""
	}
	if max <= 0 {
		max = DefaultDependentsNoteMax
	}
	shown := slugs
	extra := 0
	if len(slugs) > max {
		shown = slugs[:max]
		extra = len(slugs) - max
	}
	parts := make([]string, len(shown))
	for i, c := range shown {
		parts[i] = "`" + c + "`"
	}
	line := prefix + ": " + strings.Join(parts, ", ")
	if extra > 0 {
		line += fmt.Sprintf(", …and %d more", extra)
	}
	return line
}

// NotifySSL dispatches the right Slack call(s) for an SSL event:
//   - ssl_open:     post a parent, persist the SSL thread ref.
//   - ssl_reminder: post a thread reply.
//   - ssl_resolve:  edit the parent + post a thread reply.
func (n *Notifier) NotifySSL(ctx context.Context, channelSlug string, mentions []string, m MonitorView, ssl SSLView, ev *alert.SSLEvent) error {
	if ev == nil {
		return nil
	}
	ch, ok := n.channels(channelSlug)
	if !ok {
		return fmt.Errorf("slack channel slug %q is not registered", channelSlug)
	}
	err := n.notifySSL(ctx, ch, mentions, m, ssl, ev)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	n.observePost(err)
	return err
}

func (n *Notifier) notifySSL(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ssl SSLView, ev *alert.SSLEvent) error {
	daysRem := int(ssl.ExpiresAt.Sub(ev.At).Hours() / 24)
	in := SSLDownInput{
		FriendlyName:  m.FriendlyName,
		Tags:          m.Tags,
		URL:           m.URL,
		Mentions:      mentions,
		ExpiresAt:     ssl.ExpiresAt,
		Issuer:        m.SSLIssuer,
		Subject:       m.SSLSubject,
		DaysRemaining: daysRem,
		DetailURL:     n.DetailURL(m.Slug),
		DetectedAt:    ev.At,
	}

	switch ev.Type {
	case alert.EventSSLOpen:
		sslMsg := BuildSSLParent(in)
		res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID:   ch.ID,
			Blocks:      sslMsg.Blocks,
			Attachments: sslMsg.Attachments,
		})
		if err != nil {
			return err
		}
		n.storeSSLThread(ctx, m.Slug, res.Channel, res.TS)
		return nil

	case alert.EventSSLReminder:
		if m.SSLThreadTS == "" {
			// Initial SSL Open never delivered — synthesize a parent
			// (banner + same Down body) and persist the new ts.
			return n.postFreshSSLParent(ctx, ch, in, m.Slug)
		}
		_, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID: ch.ID,
			ThreadTS:  m.SSLThreadTS,
			Blocks:    BuildSSLReminderReply(in),
		})
		return err

	case alert.EventSSLResolve:
		resolveIn := SSLResolveInput{SSLDownInput: in, NewExpiresAt: ssl.ExpiresAt, RenewedAt: ev.At}
		if m.SSLThreadTS == "" {
			// No parent — fall back to a standalone resolve post.
			_, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
				ChannelID: ch.ID,
				Blocks:    BuildSSLResolveReply(resolveIn),
			})
			return err
		}
		sslResolveMsg := BuildSSLResolveEdit(resolveIn)
		if err := n.client.UpdateMessage(ctx, ch.Token, UpdateMessageInput{
			ChannelID:   m.SSLThreadChannel,
			TS:          m.SSLThreadTS,
			Blocks:      sslResolveMsg.Blocks,
			Attachments: sslResolveMsg.Attachments,
		}); err != nil {
			return fmt.Errorf("update ssl parent: %w", err)
		}
		_, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID: m.SSLThreadChannel,
			ThreadTS:  m.SSLThreadTS,
			Blocks:    BuildSSLResolveReply(resolveIn),
		})
		return err
	}
	return nil
}
