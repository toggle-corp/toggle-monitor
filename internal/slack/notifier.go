package slack

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// ChannelInfo describes one resolved Slack destination.
type ChannelInfo struct {
	ID    string              // C0123…
	Token secret.SecretString // bot token resolved from tokenEnv
}

// ThreadStore is the slim seam the notifier uses to persist the
// uptime thread ref after posting the parent down message. Production
// wires this to store.Repo.SetUptimeThread; tests inject a fake.
type ThreadStore interface {
	SetUptimeThread(ctx context.Context, monitorSlug, channelID, ts string) error
	SetSSLThread(ctx context.Context, monitorSlug, channelID, ts string) error
}

// Notifier turns alert events into Slack API calls.
type Notifier struct {
	client       *Client
	store        ThreadStore
	channels     func(slug string) (ChannelInfo, bool)
	bodyMaxChars int
	publicBase   string
	log          *slog.Logger
}

// NotifierOptions configures a Notifier.
type NotifierOptions struct {
	Client       *Client
	Store        ThreadStore
	Channels     func(slug string) (ChannelInfo, bool) // slug → ChannelInfo
	BodyMaxChars int
	PublicBase   string // empty → omit [View details] buttons
	Logger       *slog.Logger
}

// NewNotifier builds a Notifier from the resolved channel set.
func NewNotifier(opts NotifierOptions) *Notifier {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		client:       opts.Client,
		store:        opts.Store,
		channels:     opts.Channels,
		bodyMaxChars: opts.BodyMaxChars,
		publicBase:   opts.PublicBase,
		log:          log,
	}
}

// MonitorView is the slim shape the notifier needs about a monitor at
// the moment an alert event fires. Built by the scheduler from
// store.MonitorRow + alert.Event.
type MonitorView struct {
	Slug         string
	FriendlyName string
	GroupSlug    string
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

	switch ev.Type {
	case alert.EventOpen:
		return n.notifyOpen(ctx, ch, mentions, m, ev)
	case alert.EventReminder:
		return n.notifyReminder(ctx, ch, m, ev)
	case alert.EventResolve:
		return n.notifyResolve(ctx, ch, mentions, m, ev)
	}
	return nil
}

func (n *Notifier) notifyOpen(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event) error {
	in := DownInput{
		FriendlyName: m.FriendlyName,
		Group:        m.GroupSlug,
		URL:          m.URL,
		Mentions:     mentions,
		StatusCode:   ev.StatusCode,
		StatusText:   m.StatusText,
		FailureAt:    ev.At,
		LastError:    ev.Error,
		ResponseBody: m.ResponseBody,
		BodyMaxChars: n.bodyMaxChars,
		DetailURL:    n.detailURL(m.Slug),
	}
	res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
		ChannelID:   ch.ID,
		Attachments: BuildDownParent(in),
	})
	if err != nil {
		return err
	}
	if err := n.store.SetUptimeThread(ctx, m.Slug, res.Channel, res.TS); err != nil {
		n.log.Warn("persist uptime thread ref", "monitor", m.Slug, "error", err)
		// don't return: the Slack message went out; we just won't be
		// able to update it later. Issue 11+ will refine this.
	}
	return nil
}

func (n *Notifier) notifyReminder(ctx context.Context, ch ChannelInfo, m MonitorView, ev *alert.Event) error {
	if m.UptimeThreadTS == "" {
		// No parent recorded — design says retry-then-fresh-parent.
		// For Issue 3 we surface the gap in logs and skip; Issue 16
		// owns the full retry policy.
		n.log.Warn("reminder skipped: no parent thread ref", "monitor", m.Slug)
		return nil
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

func (n *Notifier) notifyResolve(ctx context.Context, ch ChannelInfo, mentions []string, m MonitorView, ev *alert.Event) error {
	if m.UptimeThreadTS == "" {
		// Without a parent ref we can't preserve the original message;
		// fall back to a thread-less resolved post so operators still
		// see the recovery (full retry policy lands in Issue 16).
		n.log.Warn("resolve thread ref missing: posting standalone resolve", "monitor", m.Slug)
		_, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID: ch.ID,
			Blocks: BuildResolveReply(ResolveInput{
				DownInput: DownInput{FriendlyName: m.FriendlyName},
				ResolveAt: ev.At,
				Downtime:  ev.Downtime,
			}),
		})
		return err
	}

	resolveIn := ResolveInput{
		DownInput: DownInput{
			FriendlyName: m.FriendlyName,
			Group:        m.GroupSlug,
			URL:          m.URL,
			Mentions:     mentions,
			StatusCode:   m.StatusCode,
			StatusText:   m.StatusText,
			FailureAt:    m.OpenedAt,
			LastError:    m.LastError,
			ResponseBody: m.ResponseBody,
			BodyMaxChars: n.bodyMaxChars,
			DetailURL:    n.detailURL(m.Slug),
		},
		ResolveAt: ev.At,
		Downtime:  ev.Downtime,
	}

	if err := n.client.UpdateMessage(ctx, ch.Token, UpdateMessageInput{
		ChannelID:   m.UptimeThreadChannel,
		TS:          m.UptimeThreadTS,
		Attachments: BuildResolveEdit(resolveIn),
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
	daysRem := int(ssl.ExpiresAt.Sub(ev.At).Hours() / 24)
	in := SSLDownInput{
		FriendlyName:  m.FriendlyName,
		Group:         m.GroupSlug,
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
		res, err := n.client.PostMessage(ctx, ch.Token, PostMessageInput{
			ChannelID:   ch.ID,
			Attachments: BuildSSLParent(in),
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
			n.log.Warn("ssl reminder skipped: no parent ref", "monitor", m.Slug)
			return nil
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
		if err := n.client.UpdateMessage(ctx, ch.Token, UpdateMessageInput{
			ChannelID:   m.SSLThreadChannel,
			TS:          m.SSLThreadTS,
			Attachments: BuildSSLResolveEdit(resolveIn),
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
