package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// captureLog returns a logger writing JSON records into buf so the
// test can assert the level (and absence of an ERROR record).
func captureLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestLogSinkError_transientSlackErrorIsWARN verifies that an
// exhausted transient *slack.SlackError (DNS race, 5xx, etc.) routes
// to WARN — keeping it off the ERROR/Sentry path.
func TestLogSinkError_transientSlackErrorIsWARN(t *testing.T) {
	var buf bytes.Buffer
	log := captureLog(&buf)

	se := &slack.SlackError{
		Method:           "chat.postMessage",
		Code:             "dns",
		Kind:             slack.KindTransient,
		Attempts:         3,
		ExhaustedRetries: true,
		Cause:            errors.New("dial tcp: lookup slack.com: i/o timeout"),
	}
	logSinkError(log, "event sink", "api", "open", se)

	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("expected WARN level for transient SlackError, got:\n%s", out)
	}
	if strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("transient SlackError must not log at ERROR level (would page Sentry); got:\n%s", out)
	}
	if !strings.Contains(out, `"slack_code":"dns"`) {
		t.Errorf("expected slack_code in structured log fields; got:\n%s", out)
	}
}

// TestLogSinkError_persistentSlackErrorIsERROR verifies that an
// operator-actionable *slack.SlackError (auth/channel/scope) routes
// to ERROR so the slog→Sentry bridge picks it up.
func TestLogSinkError_persistentSlackErrorIsERROR(t *testing.T) {
	var buf bytes.Buffer
	log := captureLog(&buf)

	se := &slack.SlackError{
		Method: "chat.postMessage",
		Code:   "invalid_auth",
		Kind:   slack.KindPersistent,
		Cause:  errors.New("slack ok=false error=invalid_auth"),
	}
	logSinkError(log, "event sink", "api", "open", se)

	out := buf.String()
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("expected ERROR level for persistent SlackError, got:\n%s", out)
	}
	if !strings.Contains(out, `"slack_code":"invalid_auth"`) {
		t.Errorf("expected slack_code field; got:\n%s", out)
	}
}

// TestLogSinkError_contextCanceledIsSilent verifies that
// context.Canceled (SIGTERM mid-call) emits nothing — it's not a
// failure.
func TestLogSinkError_contextCanceledIsSilent(t *testing.T) {
	var buf bytes.Buffer
	log := captureLog(&buf)
	logSinkError(log, "event sink", "api", "open", context.Canceled)
	if buf.Len() != 0 {
		t.Errorf("context.Canceled must not be logged; got:\n%s", buf.String())
	}
}

// TestLogSinkError_nonSlackErrorIsERROR keeps the default behavior
// for arbitrary errors from the sink (anything not *SlackError) — ERROR.
func TestLogSinkError_nonSlackErrorIsERROR(t *testing.T) {
	var buf bytes.Buffer
	log := captureLog(&buf)
	logSinkError(log, "event sink", "api", "open", errors.New("some other failure"))
	if !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("expected ERROR for non-SlackError; got:\n%s", buf.String())
	}
}
