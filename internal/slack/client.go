package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// DefaultBaseURL is the production Slack Web API base URL.
const DefaultBaseURL = "https://slack.com/api"

// DefaultRetryBudget is the wall-clock budget the client spends across
// all retry attempts for one logical call. 10s is well under the
// shortest reasonable monitor interval (30s) and the per-call HTTP
// timeout (15s), so retries never block the per-monitor goroutine
// long enough to delay its next tick.
const DefaultRetryBudget = 10 * time.Second

// Observer is the slim interface the client uses to emit retry-loop
// observability. The notifier owns the higher-level
// success/fail-by-reason counter; this is the per-retry-outcome
// counter, scoped to the client.
type Observer interface {
	SlackRetry(outcome, code string)
}

// Client is a thin wrapper over the few Slack Web API methods
// toggle-monitor uses. Token is supplied per call so callers don't
// need to hold per-token clients; the workspace check still groups by
// token via the caller's logic.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	retryBudget time.Duration
	metrics     Observer
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the Slack API base. Tests inject an httptest
// server URL here.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the underlying *http.Client (for timeouts
// or custom transports).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithRetryBudget overrides the wall-clock budget the client spends
// retrying transient errors. Zero disables retries entirely (the
// initial attempt's classification still runs). Negative values are
// treated as zero.
func WithRetryBudget(d time.Duration) Option {
	return func(c *Client) {
		if d < 0 {
			d = 0
		}
		c.retryBudget = d
	}
}

// WithObserver wires a metrics sink for retry outcomes. nil disables
// the counters.
func WithObserver(o Observer) Option { return func(c *Client) { c.metrics = o } }

// NewClient builds a Slack client with sensible defaults.
func NewClient(opts ...Option) *Client {
	c := &Client{
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		baseURL:     DefaultBaseURL,
		retryBudget: DefaultRetryBudget,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// AuthTestResult is the slim subset of slack.auth.test we use.
type AuthTestResult struct {
	OK     bool   `json:"ok"`
	TeamID string `json:"team_id"`
	Team   string `json:"team"`
	URL    string `json:"url"`
	Error  string `json:"error"`
}

// AuthTest calls slack.auth.test for the given token. Returns the
// workspace's team_id if ok, or an error otherwise.
func (c *Client) AuthTest(ctx context.Context, token secret.SecretString) (AuthTestResult, error) {
	var out AuthTestResult
	if err := c.do(ctx, "auth.test", token, nil, &out); err != nil {
		return AuthTestResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("slack auth.test failed: %s", out.Error)
	}
	return out, nil
}

// Attachment is the slim shape of a legacy chat.postMessage attachment.
// Slack still supports `color` (the left-edge stripe) when blocks are
// nested inside an attachment, even though attachments themselves are
// labelled legacy. We use this exclusively for color theming the
// parent + edited-parent messages.
type Attachment struct {
	Color  string  `json:"color,omitempty"`
	Blocks []Block `json:"blocks,omitempty"`
}

// PostMessageInput is the slim shape of chat.postMessage we send.
// Callers populate either Blocks (top-level, no color stripe) or
// Attachments (one attachment per color stripe).
type PostMessageInput struct {
	ChannelID   string       `json:"channel"`
	Blocks      []Block      `json:"blocks,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	ThreadTS    string       `json:"thread_ts,omitempty"` // empty = new parent
}

// PostMessageResult mirrors the slim shape of chat.postMessage's
// response. TS is the message timestamp; persist it as the thread ref.
type PostMessageResult struct {
	OK      bool   `json:"ok"`
	TS      string `json:"ts"`
	Channel string `json:"channel"`
	Error   string `json:"error"`
}

// PostMessage calls slack.chat.postMessage. If ThreadTS is set, the
// message is posted as a thread reply; otherwise it's a new parent.
func (c *Client) PostMessage(ctx context.Context, token secret.SecretString, in PostMessageInput) (PostMessageResult, error) {
	var out PostMessageResult
	if err := c.do(ctx, "chat.postMessage", token, in, &out); err != nil {
		return PostMessageResult{}, err
	}
	if !out.OK {
		return out, fmt.Errorf("slack chat.postMessage failed: %s", out.Error)
	}
	return out, nil
}

// DeleteMessageInput is the slim shape of chat.delete we send. Used
// by the `slack test --cleanup` flow to wipe simulated messages after
// the workflow renders.
type DeleteMessageInput struct {
	ChannelID string `json:"channel"`
	TS        string `json:"ts"`
}

// DeleteMessage calls slack.chat.delete on a single message TS in a
// channel. Bots can only delete messages they themselves posted.
func (c *Client) DeleteMessage(ctx context.Context, token secret.SecretString, in DeleteMessageInput) error {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.do(ctx, "chat.delete", token, in, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.delete failed: %s", out.Error)
	}
	return nil
}

// UpdateMessageInput is the slim shape of chat.update we send.
// Either Blocks or Attachments may be populated; see PostMessageInput.
type UpdateMessageInput struct {
	ChannelID   string       `json:"channel"`
	TS          string       `json:"ts"`
	Blocks      []Block      `json:"blocks,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// UpdateMessageResult mirrors the slim shape of chat.update's response.
type UpdateMessageResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// UsersInfoResult is the slim shape of slack.users.info we use.
type UsersInfoResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	User  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
}

// UsersInfo calls slack.users.info for the given user ID. Returns
// (result, nil) on a Slack-level error (ok=false) so callers can
// classify it as "unknown user" vs "transport failure".
func (c *Client) UsersInfo(ctx context.Context, token secret.SecretString, userID string) (UsersInfoResult, error) {
	var out UsersInfoResult
	// users.info accepts the user via either query string or form body;
	// we send it as a form body for symmetry with the other POSTs.
	if err := c.doForm(ctx, "users.info", token, map[string]string{"user": userID}, &out); err != nil {
		return UsersInfoResult{}, err
	}
	return out, nil
}

// UsergroupsListResult is the slim shape of slack.usergroups.list.
type UsergroupsListResult struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Usergroups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"usergroups"`
}

// UsergroupsList calls slack.usergroups.list. Returns the slim list
// of subteam IDs visible to the bot.
func (c *Client) UsergroupsList(ctx context.Context, token secret.SecretString) (UsergroupsListResult, error) {
	var out UsergroupsListResult
	if err := c.doForm(ctx, "usergroups.list", token, nil, &out); err != nil {
		return UsergroupsListResult{}, err
	}
	return out, nil
}

// UpdateMessage edits an existing message in place (used for the
// resolve transition's parent-edit).
func (c *Client) UpdateMessage(ctx context.Context, token secret.SecretString, in UpdateMessageInput) error {
	var out UpdateMessageResult
	if err := c.do(ctx, "chat.update", token, in, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.update failed: %s", out.Error)
	}
	return nil
}

// do is the shared request helper. Empty body → POST with no JSON
// (auth.test accepts that). Non-nil body → JSON-encoded with HTML
// escaping disabled (preserves Slack <!date^...> and mention markup
// literally). Transport + HTTP + Slack-level errors come back wrapped
// in a *SlackError so callers can route via errors.As.
func (c *Client) do(ctx context.Context, method string, token secret.SecretString, body, out any) error {
	if token.Reveal() == "" {
		return errors.New("slack: empty token")
	}

	var rawBody []byte
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return fmt.Errorf("encode %s body: %w", method, err)
		}
		rawBody = buf.Bytes()
	}

	build := func() (*http.Request, error) {
		var reqBody io.Reader
		if rawBody != nil {
			reqBody = bytes.NewReader(rawBody)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
		return req, nil
	}

	return c.retryingDo(ctx, method, build, out)
}

// doForm is do() but sends application/x-www-form-urlencoded.
// users.info / usergroups.list accept both — the form variant matches
// what the Slack Go SDK does and keeps the wire layout simple.
func (c *Client) doForm(ctx context.Context, method string, token secret.SecretString, params map[string]string, out any) error {
	if token.Reveal() == "" {
		return errors.New("slack: empty token")
	}
	form := ""
	for k, v := range params {
		if form != "" {
			form += "&"
		}
		form += k + "=" + v
	}
	rawBody := []byte(form)
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(rawBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		return req, nil
	}
	return c.retryingDo(ctx, method, build, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// retryingDo runs build/send-and-parse with the configured retry
// budget. Returns either nil on success or *SlackError on failure
// (Method/Code/Kind populated). Each retry rebuilds the request from
// scratch (build()) so the request body Reader is fresh.
//
// Idempotency stance: a transient error is only retried when the
// classifier reports safeRetryable=true (i.e., the request provably
// didn't reach Slack — DNS/dial/connect-refused). Read/EOF after a
// request was sent are surfaced as transient-unsafe; the loop returns
// them without retry so we don't risk double-delivery.
func (c *Client) retryingDo(ctx context.Context, method string, build func() (*http.Request, error), out any) error {
	start := time.Now()
	budget := c.retryBudget
	if budget < 0 {
		budget = 0
	}
	deadline := start.Add(budget)

	var lastErr *SlackError
	for attempt := 1; ; attempt++ {
		// Context cancelled (e.g. SIGTERM) — short-circuit, don't
		// classify as a failure.
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := build()
		if err != nil {
			return &SlackError{
				Method:   method,
				Code:     "build_request",
				Kind:     KindPermanentBug,
				Attempts: attempt,
				Elapsed:  time.Since(start),
				Cause:    err,
			}
		}
		se := c.attempt(req, method, out)
		if se == nil {
			if attempt > 1 && c.metrics != nil {
				c.metrics.SlackRetry("recovered", lastErr.codeOrEmpty())
			}
			return nil
		}
		se.Attempts = attempt
		se.Elapsed = time.Since(start)

		// Context cancellation surfaces as a transport error here.
		// Treat it as a clean cancel rather than a retryable failure.
		if errors.Is(se.Cause, context.Canceled) || errors.Is(se.Cause, context.DeadlineExceeded) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}

		// Not transient → no retry, no metric (the notifier will
		// emit a single fail counter at its level).
		if se.Kind != KindTransient {
			return se
		}
		// Transient but transport-unsafe (mid-flight read/EOF) → no
		// retry to avoid double-delivery.
		if se.HTTPStatus == 0 && !se.transportSafeRetry {
			return se
		}
		// Budget exhausted → give up, mark for the WARN path.
		nextWait := nextBackoff(attempt, se.RetryAfter)
		if time.Now().Add(nextWait).After(deadline) || budget == 0 {
			se.ExhaustedRetries = attempt > 1
			if c.metrics != nil && se.ExhaustedRetries {
				c.metrics.SlackRetry("exhausted", se.Code)
			}
			return se
		}

		lastErr = se
		// Wait, honoring context.
		t := time.NewTimer(nextWait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// attempt issues one HTTP request and parses the response. nil return
// means success (response decoded into out). Non-nil is a SlackError
// with Method/Code/Kind/HTTPStatus/RetryAfter populated; Attempts/
// Elapsed/ExhaustedRetries are filled in by the caller.
func (c *Client) attempt(req *http.Request, method string, out any) *SlackError {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		kind, code, safe := classifyTransport(err)
		se := &SlackError{
			Method:             method,
			Code:               code,
			Kind:               kind,
			Cause:              err,
			transportSafeRetry: safe,
		}
		return se
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return &SlackError{
			Method:             method,
			Code:               httpStatusCode(resp.StatusCode),
			Kind:               classifyHTTPStatus(resp.StatusCode),
			HTTPStatus:         resp.StatusCode,
			RetryAfter:         retryAfter,
			Cause:              fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200)),
			transportSafeRetry: true, // HTTP-level retries are safe (Slack didn't process)
		}
	}

	// Decode into a sniffing wrapper that lets us inspect `ok` + `error`
	// before handing back to the caller's struct.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &SlackError{
			Method: method,
			Code:   "decode",
			Kind:   KindTransient,
			Cause:  fmt.Errorf("read %s body: %w", method, err),
		}
	}
	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	// Tolerate empty body (some endpoints return nothing; not our case but cheap).
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &head)
	}
	if !head.OK && head.Error != "" {
		kind := classifySlackCode(head.Error)
		if kind == KindUnknown {
			// Unknown slack-level error: treat as persistent so we
			// don't spin retries against an error code we haven't
			// classified. If it turns out to be transient, the next
			// state change recovers.
			kind = KindPersistent
		}
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return &SlackError{
			Method:             method,
			Code:               head.Error,
			Kind:               kind,
			HTTPStatus:         resp.StatusCode,
			RetryAfter:         retryAfter,
			Cause:              fmt.Errorf("slack ok=false error=%s", head.Error),
			transportSafeRetry: true,
		}
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return &SlackError{
				Method: method,
				Code:   "decode",
				Kind:   KindTransient,
				Cause:  fmt.Errorf("decode %s response: %w", method, err),
			}
		}
	}
	return nil
}

// httpStatusCode renders the "http_NNN" label used in metrics + log
// fields when an HTTP non-2xx is returned without a slack-level error.
func httpStatusCode(status int) string {
	return fmt.Sprintf("http_%d", status)
}

// parseRetryAfter accepts the seconds-form Retry-After (RFC 7231).
// Slack uses seconds for rate limits. HTTP-date form is rare here and
// not parsed; we return 0 and fall back to exp backoff.
func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	secs, err := time.ParseDuration(s + "s")
	if err != nil || secs < 0 {
		return 0
	}
	return secs
}

// nextBackoff computes the wait before attempt+1. Uses 500ms / 1s / 2s
// schedule with ±25% jitter. Retry-After (when set) overrides the
// schedule, clamped to a sane max (60s).
func nextBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > 60*time.Second {
			retryAfter = 60 * time.Second
		}
		return retryAfter
	}
	base := time.Duration(0)
	switch attempt {
	case 1:
		base = 500 * time.Millisecond
	case 2:
		base = 1 * time.Second
	default:
		base = 2 * time.Second
	}
	jitter := time.Duration(rand.Int64N(int64(base/2))) - base/4
	return base + jitter
}

// codeOrEmpty is a small SlackError helper used by the retry metrics.
func (e *SlackError) codeOrEmpty() string {
	if e == nil {
		return ""
	}
	return e.Code
}
