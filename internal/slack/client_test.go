package slack_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// fakeSlack is a minimal stand-in for the Slack Web API. It records
// every received request so tests can assert what was sent.
type fakeSlack struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(method string) (statusCode int, body string)
}

type recordedRequest struct {
	Method string
	Auth   string
	Body   map[string]any
}

func newFakeSlack(t *testing.T) (*fakeSlack, *httptest.Server) {
	t.Helper()
	f := &fakeSlack{
		respond: func(method string) (int, string) {
			switch method {
			case "auth.test":
				return 200, `{"ok": true, "team_id": "T123", "team": "TestCorp", "url": "https://test.slack.com/"}`
			case "chat.postMessage":
				return 200, `{"ok": true, "ts": "1700000000.000100", "channel": "C0123ABCD"}`
			case "chat.update":
				return 200, `{"ok": true}`
			}
			return 404, `{"ok": false, "error": "unknown method"}`
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]

		body := map[string]any{}
		if r.Header.Get("Content-Type") != "" {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
		}
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			Method: method,
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		})
		f.mu.Unlock()
		code, resp := f.respond(method)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestClient_AuthTest_returnsTeamID(t *testing.T) {
	_, srv := newFakeSlack(t)
	c := slack.NewClient(slack.WithBaseURL(srv.URL))
	res, err := c.AuthTest(context.Background(), secret.SecretString("xoxb-test"))
	if err != nil {
		t.Fatalf("AuthTest: %v", err)
	}
	if res.TeamID != "T123" {
		t.Errorf("team_id: got %q, want T123", res.TeamID)
	}
}

func TestClient_AuthTest_returnsErrorOnNotOK(t *testing.T) {
	f, srv := newFakeSlack(t)
	f.respond = func(string) (int, string) {
		return 200, `{"ok": false, "error": "invalid_auth"}`
	}
	c := slack.NewClient(slack.WithBaseURL(srv.URL))
	_, err := c.AuthTest(context.Background(), secret.SecretString("xoxb-bad"))
	if err == nil {
		t.Fatal("expected error from auth.test failure")
	}
	if !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("error should mention slack error code, got: %v", err)
	}
}

func TestClient_PostMessage_sendsCorrectBody(t *testing.T) {
	f, srv := newFakeSlack(t)
	c := slack.NewClient(slack.WithBaseURL(srv.URL))

	res, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{
		ChannelID: "C0123ABCD",
		Blocks:    []slack.Block{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "hello <!here>"}}},
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if res.TS != "1700000000.000100" {
		t.Errorf("ts: got %q", res.TS)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("requests recorded: got %d, want 1", len(f.requests))
	}
	r := f.requests[0]
	if r.Method != "chat.postMessage" {
		t.Errorf("method: got %q", r.Method)
	}
	if r.Auth != "Bearer xoxb-tok" {
		t.Errorf("Authorization: got %q", r.Auth)
	}
	if r.Body["channel"] != "C0123ABCD" {
		t.Errorf("channel: got %v", r.Body["channel"])
	}
}

func TestClient_PostMessage_sendsThreadReply(t *testing.T) {
	f, srv := newFakeSlack(t)
	c := slack.NewClient(slack.WithBaseURL(srv.URL))

	_, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{
		ChannelID: "C0123ABCD",
		Blocks:    []slack.Block{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "reminder"}}},
		ThreadTS:  "1700000000.000100",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests[0].Body["thread_ts"] != "1700000000.000100" {
		t.Errorf("thread_ts: got %v", f.requests[0].Body["thread_ts"])
	}
}

func TestClient_UpdateMessage_sendsTSAndBlocks(t *testing.T) {
	f, srv := newFakeSlack(t)
	c := slack.NewClient(slack.WithBaseURL(srv.URL))

	err := c.UpdateMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.UpdateMessageInput{
		ChannelID: "C0123ABCD",
		TS:        "1700000000.000100",
		Blocks:    []slack.Block{{"type": "header", "text": map[string]any{"type": "plain_text", "text": "✅ API is UP"}}},
	})
	if err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests[0].Method != "chat.update" {
		t.Errorf("method: got %q", f.requests[0].Method)
	}
	if f.requests[0].Body["ts"] != "1700000000.000100" {
		t.Errorf("ts: got %v", f.requests[0].Body["ts"])
	}
}

func TestClient_emptyToken_errors(t *testing.T) {
	c := slack.NewClient(slack.WithBaseURL("http://unused"))
	_, err := c.AuthTest(context.Background(), secret.SecretString(""))
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}
