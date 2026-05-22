package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

// DefaultBaseURL is the production Slack Web API base URL.
const DefaultBaseURL = "https://slack.com/api"

// Client is a thin wrapper over the few Slack Web API methods
// toggle-monitor uses. Token is supplied per call so callers don't
// need to hold per-token clients; the workspace check still groups by
// token via the caller's logic.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the Slack API base. Tests inject an httptest
// server URL here.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the underlying *http.Client (for timeouts
// or custom transports).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// NewClient builds a Slack client with sensible defaults.
func NewClient(opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    DefaultBaseURL,
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

// PostMessageInput is the slim shape of chat.postMessage we send.
type PostMessageInput struct {
	ChannelID string  `json:"channel"`
	Blocks    []Block `json:"blocks"`
	ThreadTS  string  `json:"thread_ts,omitempty"` // empty = new parent
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

// UpdateMessageInput is the slim shape of chat.update we send.
type UpdateMessageInput struct {
	ChannelID string  `json:"channel"`
	TS        string  `json:"ts"`
	Blocks    []Block `json:"blocks"`
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
// literally).
func (c *Client) do(ctx context.Context, method string, token secret.SecretString, body, out any) error {
	if token.Reveal() == "" {
		return errors.New("slack: empty token")
	}

	var reqBody io.Reader
	if body != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(body); err != nil {
			return fmt.Errorf("encode %s body: %w", method, err)
		}
		reqBody = &buf
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, reqBody)
	if err != nil {
		return fmt.Errorf("new request %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("call %s: http %d: %s", method, resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(out); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
	}
	return nil
}

// doForm is do() but sends application/x-www-form-urlencoded.
// users.info / usergroups.list accept both — the form variant matches
// what the Slack Go SDK does and keeps the wire layout simple.
func (c *Client) doForm(ctx context.Context, method string, token secret.SecretString, params map[string]string, out any) error {
	if token.Reveal() == "" {
		return errors.New("slack: empty token")
	}
	body := ""
	for k, v := range params {
		if body != "" {
			body += "&"
		}
		body += k + "=" + v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader([]byte(body)))
	if err != nil {
		return fmt.Errorf("new request %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("call %s: http %d: %s", method, resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(out); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
