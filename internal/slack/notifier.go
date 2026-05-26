package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
// thread refs (uptime + SSL) after posting the parent message, and to
// look up which monitors depend on a given one (so the parent message
// can call out "this alert will pause X, Y"). Production wires this
// to *store.Repo; tests inject a fake.
type ThreadStore interface {
	SetUptimeThread(ctx context.Context, monitorSlug, channelID, ts string) error
	SetSSLThread(ctx context.Context, monitorSlug, channelID, ts string) error
	ListChildrenOf(ctx context.Context, parentSlug string) ([]string, error)
}

// DefaultDependentsNoteMax is the fallback cap when the config leaves
// slack.dependentsNoteMax at zero. Keeps the dependents line readable
// when a parent monitor has many children.
const DefaultDependentsNoteMax = 5

// freshParentBanner is the late-notice banner rendered at the top of
// the body when the notifier had to synthesize a parent because the
// initial Open delivery failed.
const freshParentBanner = "⚠️ Initial notification delivery failed. This alert is current."

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
//   - EventOpen:     post a new parent, persist the thread ref.
//   - EventReminder: post a thread reply.
//   - EventResolve:  edit the parent and post a thread reply.
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

	var err error
	switch ev.Type {
	case alert.EventOpen:
		err = n.notifyOpen(ctx, ch, mentions, m, ev)
	case alert.EventReminder:
		err = n.notifyReminder(ctx, ch, mentions, m, ev)
	case alert.EventResolve:
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

func (n *Notifier) notifyOpen(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event) error {
	in := DownInput{
		FriendlyName: m.FriendlyName,
		Tags:         m.Tags,
		URL:          m.URL,
		Mentions:     mentions,
		StatusCode:   ev.StatusCode,
		StatusText:   m.StatusText,
		FailureAt:    ev.At,
		LastError:    ev.Error,
		ResponseBody: m.ResponseBody,
		BodyMaxChars: n.bodyMaxChars,
		DetailURL:    n.detailURL(m.Slug),
		Note:         n.dependentsNote(ctx, m.Slug, "⏸ Pauses dependents"),
	}
	msg := BuildDownParent(in)
	res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return err
	}
	if err := n.store.SetUptimeThread(ctx, m.Slug, res.Channel, res.TS); err != nil {
		n.log.Warn("persist uptime thread ref", "monitor", m.Slug, "error", err)
		// don't return: the Slack message went out; we just won't be
		// able to update it later. The reminder fresh-parent fallback
		// will synthesize a replacement on the next tick.
	}
	return nil
}

func (n *Notifier) notifyReminder(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event) error {
	if m.UptimeThreadTS == "" {
		// No parent recorded — the initial Open delivery failed. Post
		// a fresh Down parent with a late-notice banner, persist its
		// ts, and let subsequent reminders thread onto it.
		return n.postFreshUptimeParent(ctx, ch, mentions, m)
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
// fires but no parent ts is on file (because the original Open delivery
// failed). Persists the new ts so subsequent reminders thread normally.
func (n *Notifier) postFreshUptimeParent(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView) error {
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
		DetailURL:    n.detailURL(m.Slug),
		Note:         n.dependentsNote(ctx, m.Slug, "⏸ Pauses dependents"),
		Banner:       freshParentBanner,
	}
	msg := BuildDownParent(in)
	res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		return err
	}
	n.observeFreshParent("uptime")
	if err := n.store.SetUptimeThread(ctx, m.Slug, res.Channel, res.TS); err != nil {
		// Best-effort: the message went out. Future reminders will
		// re-synthesize on the next tick if persistence still fails.
		n.log.Warn("persist fresh uptime thread ref", "monitor", m.Slug, "error", err)
	}
	return nil
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
				DetailURL:    n.detailURL(m.Slug),
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
			DetailURL:    n.detailURL(m.Slug),
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

func (n *Notifier) detailURL(monitorSlug string) string {
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
	if err := n.store.SetSSLThread(ctx, slug, res.Channel, res.TS); err != nil {
		n.log.Warn("persist fresh ssl thread ref", "monitor", slug, "error", err)
	}
	return nil
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
		DetailURL:     n.detailURL(m.Slug),
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
		if err := n.store.SetSSLThread(ctx, m.Slug, res.Channel, res.TS); err != nil {
			n.log.Warn("persist ssl thread ref", "monitor", m.Slug, "error", err)
		}
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
