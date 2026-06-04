package alertmanager

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// Default knobs for the AM webhook handler. Hardcoded per ADR-0005 §
// "Endpoint, auth, body cap" and §"Multi-alert batching": the operator
// surfaces of v1 are the bearer token, channel routing, and rate limit
// — not these protocol-level guards.
const (
	defaultBodyMaxBytes     = 10 * 1024 * 1024
	defaultSlackTimeout     = 3 * time.Second
	defaultBatchConcurrency = 8
)

// SlackPoster is the slim Slack seam the handler depends on. Production
// passes *slack.Client (which satisfies the interface structurally);
// tests pass either *slack.Client backed by an httptest server or a
// purpose-built fake. Keeping this an interface lets the handler stay
// transport-agnostic and avoids dragging the whole Client surface into
// callers and tests.
type SlackPoster interface {
	PostMessage(ctx context.Context, token secret.SecretString, in slack.PostMessageInput) (slack.PostMessageResult, error)
	UpdateMessage(ctx context.Context, token secret.SecretString, in slack.UpdateMessageInput) error
}

// MentionResolver turns a notify[] slice (slugs and/or raw Slack markup)
// into ready-to-render mention strings. The handler accepts the
// interface so lifecycle wiring (Slice 8) can plug in
// slack.ResolveMentions over the configured userMapping.
type MentionResolver interface {
	Resolve(notify []string) []string
}

// Observer is the metrics seam used by the handler. Each method is
// invoked at most once per logical event (batch / alert / Slack post)
// and the implementation is expected to be safe for concurrent use.
// nil disables every counter.
type Observer interface {
	AMWebhookRequest(result, reason string)
	AMAlertProcessed(result, reason string)
	AMSlackPost(result, reason string)
	AMRateLimitDrop(channel string)
	AMLateResolve()
	AMWebhookLatency(seconds float64)
	AMBatchSize(n int)
}

// Handler is the AM webhook receiver. It does not register itself with
// any mux — lifecycle wiring (Slice 8) plumbs it into web.Server via
// Server.RegisterRoute. Constructed once at process start; safe for
// concurrent use.
type Handler struct {
	cfg              *config.AlertmanagerConfig
	expectedToken    secret.SecretString
	repo             *store.Repo
	slackClient      SlackPoster
	channels         func(slug string) (slack.ChannelInfo, bool)
	mentionResolver  MentionResolver
	limiter          *Limiter
	bodyMaxBytes     int64
	slackTimeout     time.Duration
	batchConcurrency int
	publicBase       string
	now              func() time.Time
	log              *slog.Logger
	observer         Observer
}

// HandlerOptions configures a Handler. Required fields fail loudly via
// NewHandler; optional ones get sensible defaults.
type HandlerOptions struct {
	Config      *config.AlertmanagerConfig
	Repo        *store.Repo
	SlackClient SlackPoster
	Channels    func(slug string) (slack.ChannelInfo, bool)
	Mentions    MentionResolver
	Now         func() time.Time
	Logger      *slog.Logger
	Observer    Observer
	PublicBase  string
}

// NewHandler builds the handler. Resolves the bearer token from the env
// var named in cfg.Endpoint.TokenEnv. Returns an error if the env var
// is missing/empty (fatal at startup; the caller is expected to fail
// loudly).
func NewHandler(opts HandlerOptions) (*Handler, error) {
	if opts.Config == nil {
		return nil, errors.New("alertmanager: NewHandler: Config is required")
	}
	if opts.Repo == nil {
		return nil, errors.New("alertmanager: NewHandler: Repo is required")
	}
	if opts.SlackClient == nil {
		return nil, errors.New("alertmanager: NewHandler: SlackClient is required")
	}
	if opts.Channels == nil {
		return nil, errors.New("alertmanager: NewHandler: Channels is required")
	}
	if opts.Mentions == nil {
		return nil, errors.New("alertmanager: NewHandler: Mentions is required")
	}
	if opts.Config.Endpoint.TokenEnv == "" {
		return nil, errors.New("alertmanager: NewHandler: Config.Endpoint.TokenEnv is empty")
	}
	tok := os.Getenv(opts.Config.Endpoint.TokenEnv)
	if tok == "" {
		return nil, fmt.Errorf("alertmanager: NewHandler: env var %q is empty", opts.Config.Endpoint.TokenEnv)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Handler{
		cfg:              opts.Config,
		expectedToken:    secret.SecretString(tok),
		repo:             opts.Repo,
		slackClient:      opts.SlackClient,
		channels:         opts.Channels,
		mentionResolver:  opts.Mentions,
		limiter:          NewLimiter(opts.Config.RateLimit, now),
		bodyMaxBytes:     defaultBodyMaxBytes,
		slackTimeout:     defaultSlackTimeout,
		batchConcurrency: defaultBatchConcurrency,
		publicBase:       opts.PublicBase,
		now:              now,
		log:              log,
		observer:         opts.Observer,
	}, nil
}

// ServeHTTP implements http.Handler. The body of the method walks the
// ADR-0005 §"Endpoint, auth, body cap" checklist top-to-bottom: method
// gate → auth → body cap → JSON unmarshal → envelope validate → per-
// alert processing. Every failure path emits a request-scoped log line
// and (when configured) an observer counter; the response code mirrors
// the failure class so AM's retry-on-5xx behaviour stays honest.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	w.Header().Set("X-Request-ID", requestID)
	start := h.now()
	defer func() {
		if h.observer != nil {
			h.observer.AMWebhookLatency(h.now().Sub(start).Seconds())
		}
	}()
	log := h.log.With("request_id", requestID)

	// 1) Method gate.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.fail(w, log, http.StatusMethodNotAllowed, "fail", "method",
			"method not allowed", "method", r.Method)
		return
	}

	// 2) Auth.
	const bearerPrefix = "Bearer "
	authHdr := r.Header.Get("Authorization")
	if len(authHdr) <= len(bearerPrefix) || authHdr[:len(bearerPrefix)] != bearerPrefix {
		h.fail(w, log, http.StatusUnauthorized, "fail", "auth",
			"missing or malformed bearer")
		return
	}
	received := authHdr[len(bearerPrefix):]
	if subtle.ConstantTimeCompare([]byte(received), []byte(h.expectedToken.Reveal())) != 1 {
		h.fail(w, log, http.StatusUnauthorized, "fail", "auth",
			"bearer mismatch")
		return
	}

	// 3) Body.
	r.Body = http.MaxBytesReader(w, r.Body, h.bodyMaxBytes)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.fail(w, log, http.StatusRequestEntityTooLarge, "fail", "too_large",
				"body exceeds cap", "limit_bytes", h.bodyMaxBytes)
			return
		}
		h.fail(w, log, http.StatusBadRequest, "fail", "malformed",
			"read body", "error", err.Error())
		return
	}

	// 4) Decode + validate.
	var wh Webhook
	if err := json.Unmarshal(rawBody, &wh); err != nil {
		h.fail(w, log, http.StatusBadRequest, "fail", "malformed",
			"json decode", "error", err.Error())
		return
	}
	if err := wh.Validate(); err != nil {
		h.fail(w, log, http.StatusBadRequest, "fail", "malformed",
			"envelope validate", "error", err.Error())
		return
	}
	if h.observer != nil {
		h.observer.AMBatchSize(len(wh.Alerts))
	}

	envelope := Envelope{Receiver: wh.Receiver, ExternalURL: wh.ExternalURL}

	// 5) Process the batch with bounded concurrency.
	processed, dropped, failed := h.processBatch(r.Context(), wh, envelope, rawBody, log)

	if failed > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "partial_failure",
			"processed": processed,
			"failed":    failed,
		})
		if h.observer != nil {
			h.observer.AMWebhookRequest("fail", "partial_failure")
		}
		log.Warn("am.webhook.batch.partial_failure",
			"receiver", wh.Receiver,
			"externalURL", wh.ExternalURL,
			"alerts", len(wh.Alerts),
			"processed", processed,
			"dropped", dropped,
			"failed", failed,
			"latency_ms", h.now().Sub(start).Milliseconds(),
		)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"processed": processed,
	})
	if h.observer != nil {
		h.observer.AMWebhookRequest("success", "ok")
	}
	log.Info("am.webhook.batch",
		"receiver", wh.Receiver,
		"externalURL", wh.ExternalURL,
		"alerts", len(wh.Alerts),
		"processed", processed,
		"dropped", dropped,
		"failed", failed,
		"latency_ms", h.now().Sub(start).Milliseconds(),
	)
}

// fail centralises the failure response + observer + log path so the
// happy path of ServeHTTP stays readable. result/reason are observer
// counter values; the log line carries any additional structured args.
func (h *Handler) fail(w http.ResponseWriter, log *slog.Logger, code int, result, reason string, msg string, args ...any) {
	if h.observer != nil {
		h.observer.AMWebhookRequest(result, reason)
	}
	logArgs := append([]any{"reason", reason, "status", code}, args...)
	switch {
	case code >= 500:
		log.Error(msg, logArgs...)
	case code >= 400 && code < 500 && code != http.StatusBadRequest && code != http.StatusRequestEntityTooLarge:
		log.Warn(msg, logArgs...)
	default:
		log.Info(msg, logArgs...)
	}
	http.Error(w, http.StatusText(code), code)
}

// processBatch fans the alerts out across batchConcurrency workers and
// collects per-alert outcomes. The returned counts are summed atomically
// inside processAlert; transient errors bubble up via errgroup but we
// translate them to a "failed" tally rather than letting one alert's
// failure roll the whole batch back (each alert is independent).
func (h *Handler) processBatch(ctx context.Context, wh Webhook, env Envelope, rawBody []byte, log *slog.Logger) (processed, dropped, failed int) {
	var (
		mu sync.Mutex
	)
	bump := func(p, d, f int) {
		mu.Lock()
		processed += p
		dropped += d
		failed += f
		mu.Unlock()
	}

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, h.batchConcurrency)
	for i := range wh.Alerts {
		alert := wh.Alerts[i]
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()
			outcome := h.processAlert(gctx, alert, env, rawBody, log)
			bump(outcome.processed, outcome.dropped, outcome.failed)
			return nil
		})
	}
	_ = g.Wait()
	return processed, dropped, failed
}

// alertOutcome captures the per-alert result aggregation. Exactly one
// of processed / dropped / failed is incremented per processAlert call.
type alertOutcome struct {
	processed int
	dropped   int
	failed    int
}

// processAlert is the per-alert state machine: cascade resolve → channel
// lookup → rate-limit check → firing or resolved branch. Returns the
// outcome tally for the batch aggregator.
//
// At-most-twice semantics (ADR-0005): when an AM redelivery hits an
// existing open row whose slack_ts is empty, we re-attempt the post.
// Worst case is one extra Slack message on process crash; AM's
// retry-on-5xx + our partial unique index keep the row count exactly
// one per (fingerprint, incident).
func (h *Handler) processAlert(ctx context.Context, alert Alert, env Envelope, rawBody []byte, log *slog.Logger) alertOutcome {
	resolved := Evaluate(h.cfg.Match, alert, env)
	alog := log.With("fingerprint", alert.Fingerprint, "alertname", alert.Labels["alertname"])

	if resolved.Ignored {
		if h.observer != nil {
			h.observer.AMAlertProcessed("drop", "ignored")
		}
		alog.Info("am.alert.ignored", "rule_chain", resolved.RuleChain)
		return alertOutcome{dropped: 1}
	}

	ch, ok := h.channels(resolved.Channel)
	if !ok {
		// Validator should have caught this at config-load; if we hit
		// it at runtime, something is badly wrong. Roll up to a 5xx so
		// AM retries while the operator fixes the config.
		if h.observer != nil {
			h.observer.AMAlertProcessed("fail", "channel_unknown")
		}
		alog.Error("am.alert.channel_unknown", "channel", resolved.Channel, "rule_chain", resolved.RuleChain)
		return alertOutcome{failed: 1}
	}

	allowed, justEngaged := h.limiter.Allow(resolved.Channel)
	if !allowed {
		if h.observer != nil {
			h.observer.AMRateLimitDrop(resolved.Channel)
			h.observer.AMAlertProcessed("drop", "rate_limited")
		}
		alog.Warn("am.alert.rate_limited", "channel", resolved.Channel)
		if justEngaged {
			// First drop in the engaged state: emit the warning
			// immediately, then consume the accumulated drop counter
			// via NoticeDue so its lastNotice timestamp resets — that's
			// what gates subsequent NoticeDue calls during the
			// noticeEvery window.
			n := h.limiter.NoticeDue(resolved.Channel)
			if n == 0 {
				n = 1
			}
			h.postThrottleNotice(ctx, ch, resolved.Channel, n, alog)
		} else if n := h.limiter.NoticeDue(resolved.Channel); n > 0 {
			h.postThrottleNotice(ctx, ch, resolved.Channel, n, alog)
		}
		return alertOutcome{dropped: 1}
	}

	switch alert.Status {
	case AlertStatusFiring:
		return h.handleFiring(ctx, alert, env, resolved, ch, rawBody, alog)
	case AlertStatusResolved:
		return h.handleResolved(ctx, alert, env, resolved, ch, rawBody, alog)
	default:
		// Should be unreachable — Webhook.Validate enforces the
		// vocabulary. Defensive: treat as a drop with reason "unknown".
		if h.observer != nil {
			h.observer.AMAlertProcessed("drop", "unknown_status")
		}
		return alertOutcome{dropped: 1}
	}
}

// handleFiring inserts (or finds) the open incident, then posts the
// parent Slack message if one isn't already on file. The mid-crash case
// — InsertOpenAMIncident returns inserted=false but the existing row's
// slack_ts is empty — is the at-most-twice delivery edge described in
// ADR-0005: AM redelivered, our process had crashed between DB-INSERT
// and Slack-post the first time around. We re-post.
func (h *Handler) handleFiring(ctx context.Context, alert Alert, env Envelope, resolved Resolved, ch slack.ChannelInfo, rawBody []byte, alog *slog.Logger) alertOutcome {
	in := store.AMIncidentInsert{
		Fingerprint:    alert.Fingerprint,
		Alertname:      alert.Labels["alertname"],
		Labels:         alert.Labels,
		Annotations:    alert.Annotations,
		StartedAt:      alert.StartsAt,
		ChannelSlug:    resolved.Channel,
		RuleChain:      resolved.RuleChain,
		ResolvedNotify: resolved.Notify,
		ExternalURL:    env.ExternalURL,
		Receiver:       env.Receiver,
	}
	incident, inserted, err := h.repo.InsertOpenAMIncident(ctx, in)
	if err != nil {
		if h.observer != nil {
			h.observer.AMAlertProcessed("fail", "db_insert")
		}
		alog.Error("am.alert.db_insert", "error", err)
		return alertOutcome{failed: 1}
	}

	eventType := store.AMEventRepeatFiring
	if inserted {
		eventType = store.AMEventFiring
	}
	if err := h.repo.AppendAMEvent(ctx, incident.ID, eventType, rawAlertOrEmpty(rawBody)); err != nil {
		// Best-effort audit trail: log and keep going.
		alog.Warn("am.alert.event_append", "error", err)
	}

	// Repeat-firing on an already-posted row → no-op for Slack.
	if !inserted && incident.SlackTS != "" {
		if h.observer != nil {
			h.observer.AMAlertProcessed("drop", "duplicate")
		}
		return alertOutcome{dropped: 1}
	}

	// Either fresh insert OR existing row missing slack_ts (mid-crash
	// recovery). Build and post the parent.
	mentions := h.mentionResolver.Resolve(resolved.Notify)
	msg := BuildAMOpen(AMOpenInput{
		Alert:       alert,
		Mentions:    mentions,
		DetailURL:   h.detailURL(incident.ID),
		Receiver:    env.Receiver,
		ExternalURL: env.ExternalURL,
	})

	postCtx, cancel := context.WithTimeout(ctx, h.slackTimeout)
	defer cancel()
	res, err := h.slackClient.PostMessage(postCtx, ch.Token, slack.PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	})
	if err != nil {
		if h.observer != nil {
			h.observer.AMSlackPost("fail", slackFailReason(err))
			h.observer.AMAlertProcessed("fail", "slack_post")
		}
		alog.Error("am.alert.slack_post", "error", err)
		return alertOutcome{failed: 1}
	}
	if err := h.repo.UpdateAMSlackRef(ctx, incident.ID, res.Channel, res.TS); err != nil {
		// Slack message already went out; failing here would have AM
		// retry and double-post. Log + move on; the next inbound for
		// this fingerprint will see the existing row and skip the post.
		alog.Warn("am.alert.update_slack_ref", "error", err)
	}
	if h.observer != nil {
		h.observer.AMSlackPost("success", "ok")
		h.observer.AMAlertProcessed("success", "ok")
	}
	return alertOutcome{processed: 1}
}

// handleResolved closes the open incident and edits the parent +
// thread-replies on resolve. If no open row exists this is a late-
// resolve (resolved arrived before any firing webhook we saw) — we post
// a standalone late-resolve message and skip persistence.
func (h *Handler) handleResolved(ctx context.Context, alert Alert, env Envelope, resolved Resolved, ch slack.ChannelInfo, rawBody []byte, alog *slog.Logger) alertOutcome {
	incident, err := h.repo.MarkAMResolved(ctx, alert.Fingerprint, alert.EndsAt)
	if err != nil {
		if h.observer != nil {
			h.observer.AMAlertProcessed("fail", "db_resolve")
		}
		alog.Error("am.alert.db_resolve", "error", err)
		return alertOutcome{failed: 1}
	}

	if incident == nil {
		// Late-resolve path: no open row on file. Post a standalone
		// late-resolve message and increment the late-resolve counter.
		// We do not persist a row for this (mirrors the "ignored alert"
		// choice — no incident_id, no audit trail).
		mentions := h.mentionResolver.Resolve(resolved.Notify)
		downtime := alert.EndsAt.Sub(alert.StartsAt)
		if downtime < 0 {
			downtime = 0
		}
		msg := BuildAMLateResolve(AMResolveInput{
			AMOpenInput: AMOpenInput{
				Alert:       alert,
				Mentions:    mentions,
				Receiver:    env.Receiver,
				ExternalURL: env.ExternalURL,
			},
			ResolvedAt: alert.EndsAt,
			Downtime:   downtime,
		})
		postCtx, cancel := context.WithTimeout(ctx, h.slackTimeout)
		defer cancel()
		if _, err := h.slackClient.PostMessage(postCtx, ch.Token, slack.PostMessageInput{
			ChannelID:   ch.ID,
			Blocks:      msg.Blocks,
			Attachments: msg.Attachments,
		}); err != nil {
			if h.observer != nil {
				h.observer.AMSlackPost("fail", slackFailReason(err))
				h.observer.AMAlertProcessed("fail", "slack_post")
			}
			alog.Error("am.alert.late_resolve_post", "error", err)
			return alertOutcome{failed: 1}
		}
		if h.observer != nil {
			h.observer.AMSlackPost("success", "ok")
			h.observer.AMLateResolve()
			h.observer.AMAlertProcessed("success", "ok")
		}
		return alertOutcome{processed: 1}
	}

	if err := h.repo.AppendAMEvent(ctx, incident.ID, store.AMEventResolved, rawAlertOrEmpty(rawBody)); err != nil {
		alog.Warn("am.alert.event_append", "error", err)
	}

	mentions := h.mentionResolver.Resolve(resolved.Notify)
	downtime := alert.EndsAt.Sub(incident.StartedAt)
	if downtime < 0 {
		downtime = 0
	}
	resolveIn := AMResolveInput{
		AMOpenInput: AMOpenInput{
			Alert:       alert,
			Mentions:    mentions,
			DetailURL:   h.detailURL(incident.ID),
			Receiver:    env.Receiver,
			ExternalURL: env.ExternalURL,
		},
		ResolvedAt: alert.EndsAt,
		Downtime:   downtime,
	}

	if incident.SlackTS == "" {
		// Open existed but the original Slack post never landed. Post
		// a standalone late-resolve banner so operators see the resolve
		// even though there's no parent to edit. Mirrors the
		// internal/slack notifier's resolve-with-missing-thread-ref path.
		alog.Warn("am.alert.resolve_no_thread", "incident_id", incident.ID)
		msg := BuildAMLateResolve(resolveIn)
		postCtx, cancel := context.WithTimeout(ctx, h.slackTimeout)
		defer cancel()
		if _, err := h.slackClient.PostMessage(postCtx, ch.Token, slack.PostMessageInput{
			ChannelID:   ch.ID,
			Blocks:      msg.Blocks,
			Attachments: msg.Attachments,
		}); err != nil {
			if h.observer != nil {
				h.observer.AMSlackPost("fail", slackFailReason(err))
				h.observer.AMAlertProcessed("fail", "slack_post")
			}
			alog.Error("am.alert.resolve_post", "error", err)
			return alertOutcome{failed: 1}
		}
		if h.observer != nil {
			h.observer.AMSlackPost("success", "ok")
			h.observer.AMAlertProcessed("success", "ok")
		}
		return alertOutcome{processed: 1}
	}

	// Happy resolve: edit parent then post a thread reply.
	editMsg := BuildAMResolveEdit(resolveIn)
	editCtx, cancel := context.WithTimeout(ctx, h.slackTimeout)
	defer cancel()
	if err := h.slackClient.UpdateMessage(editCtx, ch.Token, slack.UpdateMessageInput{
		ChannelID:   incident.SlackChannel,
		TS:          incident.SlackTS,
		Blocks:      editMsg.Blocks,
		Attachments: editMsg.Attachments,
	}); err != nil {
		if h.observer != nil {
			h.observer.AMSlackPost("fail", slackFailReason(err))
			h.observer.AMAlertProcessed("fail", "slack_update")
		}
		alog.Error("am.alert.resolve_edit", "error", err)
		return alertOutcome{failed: 1}
	}
	replyCtx, cancel2 := context.WithTimeout(ctx, h.slackTimeout)
	defer cancel2()
	replyBlocks := BuildAMResolveReply(resolveIn)
	if _, err := h.slackClient.PostMessage(replyCtx, ch.Token, slack.PostMessageInput{
		ChannelID: incident.SlackChannel,
		ThreadTS:  incident.SlackTS,
		Blocks:    replyBlocks,
	}); err != nil {
		if h.observer != nil {
			h.observer.AMSlackPost("fail", slackFailReason(err))
			h.observer.AMAlertProcessed("fail", "slack_reply")
		}
		alog.Error("am.alert.resolve_reply", "error", err)
		return alertOutcome{failed: 1}
	}
	if h.observer != nil {
		h.observer.AMSlackPost("success", "ok")
		h.observer.AMAlertProcessed("success", "ok")
	}
	return alertOutcome{processed: 1}
}

// postThrottleNotice fires the operator-visible "AM throttle engaged"
// message to a channel. Best-effort: a failure here is logged but does
// not propagate to the batch's failure tally — the alert it was
// covering already counted as a drop.
func (h *Handler) postThrottleNotice(ctx context.Context, ch slack.ChannelInfo, channelSlug string, dropped int, alog *slog.Logger) {
	msg := BuildAMThrottleNotice(AMThrottleNoticeInput{
		ChannelSlug: channelSlug,
		Dropped:     dropped,
		PerChannel:  h.cfg.RateLimit.PerChannel,
		Window:      h.cfg.RateLimit.Window.AsDuration(),
	})
	postCtx, cancel := context.WithTimeout(ctx, h.slackTimeout)
	defer cancel()
	if _, err := h.slackClient.PostMessage(postCtx, ch.Token, slack.PostMessageInput{
		ChannelID:   ch.ID,
		Blocks:      msg.Blocks,
		Attachments: msg.Attachments,
	}); err != nil {
		alog.Warn("am.throttle_notice_post", "channel", channelSlug, "error", err)
	}
}

// detailURL builds the /alert/{id} URL for the View details button.
// Returns "" when no PublicBase is configured; callers omit the button.
func (h *Handler) detailURL(incidentID int64) string {
	if h.publicBase == "" {
		return ""
	}
	return fmt.Sprintf("%s/alert/%d", h.publicBase, incidentID)
}

// newRequestID returns a short hex string suitable for log correlation.
// Falls back to a timestamp-based label on the (vanishingly rare)
// crypto/rand failure so logs never lose the field.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// rawAlertOrEmpty returns rawBody if non-empty, or `{}` so the
// am_alert_events JSONB column always receives valid JSON. The full
// envelope is recorded against every alert in a batch — the detail
// page renders it verbatim and the per-alert subset is reconstructed
// at read time.
func rawAlertOrEmpty(rawBody []byte) []byte {
	if len(rawBody) == 0 {
		return []byte(`{}`)
	}
	return rawBody
}

// slackFailReason reduces an error from the Slack client to a short
// observer-friendly reason. Mirrors the notifier's observePost helper
// — kept duplicated here so the AM handler doesn't drag the notifier's
// (monitor-specific) signature into its own seam.
func slackFailReason(err error) string {
	if err == nil {
		return "ok"
	}
	var se *slack.SlackError
	if errors.As(err, &se) {
		return se.Kind.String()
	}
	return "unknown"
}
