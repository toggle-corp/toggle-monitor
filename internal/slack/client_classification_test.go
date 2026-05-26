package slack_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// TestClient_PostMessage_wrapsClassifiedError exercises the integration
// path: a Slack-level ok=false response comes back from the fake
// server, and the client surfaces it as a *SlackError with the
// correct Kind/Code so callers can route on it via errors.As.
//
// Retries are disabled so the test asserts a single attempt's
// classification, not retry behavior (which has its own test file).
func TestClient_PostMessage_wrapsClassifiedError(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       string
		wantCode   string
		wantKind   slack.ErrorKind
	}{
		{"invalid_auth", http.StatusOK, `{"ok":false,"error":"invalid_auth"}`, "invalid_auth", slack.KindPersistent},
		{"channel_not_found", http.StatusOK, `{"ok":false,"error":"channel_not_found"}`, "channel_not_found", slack.KindPersistent},
		{"msg_too_long", http.StatusOK, `{"ok":false,"error":"msg_too_long"}`, "msg_too_long", slack.KindPermanentBug},
		{"ratelimited", http.StatusOK, `{"ok":false,"error":"ratelimited"}`, "ratelimited", slack.KindTransient},
		{"http_401", http.StatusUnauthorized, `unauthorized`, "http_401", slack.KindPersistent},
		{"http_500", http.StatusInternalServerError, `boom`, "http_500", slack.KindTransient},
		{"unknown_slack_code", http.StatusOK, `{"ok":false,"error":"banana"}`, "banana", slack.KindPersistent}, // unknown maps to persistent so we don't spin retries on unknowns
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeSlack(t)
			f.respond = func(string) (int, string) {
				return tc.httpStatus, tc.body
			}
			c := slack.NewClient(
				slack.WithBaseURL(srv.URL),
				slack.WithRetryBudget(0),
			)
			_, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{
				ChannelID: "C0",
			})
			if err == nil {
				t.Fatal("expected error")
			}
			var se *slack.SlackError
			if !errors.As(err, &se) {
				t.Fatalf("expected *slack.SlackError, got %T: %v", err, err)
			}
			if se.Code != tc.wantCode {
				t.Errorf("Code: got %q, want %q", se.Code, tc.wantCode)
			}
			if se.Kind != tc.wantKind {
				t.Errorf("Kind: got %v, want %v", se.Kind, tc.wantKind)
			}
			if se.Method != "chat.postMessage" {
				t.Errorf("Method: got %q, want chat.postMessage", se.Method)
			}
		})
	}
}
