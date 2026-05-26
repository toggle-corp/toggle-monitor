package slack_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
)

// retryingServer is a programmable fake Slack that fails the first N
// attempts then succeeds, counting all calls so the test can assert
// retry behavior.
type retryingServer struct {
	failCount  int
	failStatus int
	failBody   string
	calls      atomic.Int32
}

func (r *retryingServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		n := r.calls.Add(1)
		if int(n) <= r.failCount {
			if r.failStatus == 0 {
				r.failStatus = http.StatusInternalServerError
			}
			w.WriteHeader(r.failStatus)
			_, _ = w.Write([]byte(r.failBody))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000100","channel":"C0"}`))
	}
}

// TestClient_PostMessage_retriesTransientHTTP5xx confirms a 500 on the
// first call is retried and recovered.
func TestClient_PostMessage_retriesTransientHTTP5xx(t *testing.T) {
	rs := &retryingServer{failCount: 1, failStatus: 500}
	srv := httptest.NewServer(rs.handler())
	t.Cleanup(srv.Close)

	c := slack.NewClient(
		slack.WithBaseURL(srv.URL),
		slack.WithRetryBudget(2*time.Second),
	)
	res, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{ChannelID: "C0"})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if res.TS != "1700000000.000100" {
		t.Errorf("ts: got %q", res.TS)
	}
	if got := rs.calls.Load(); got != 2 {
		t.Errorf("calls: got %d, want 2 (initial + 1 retry)", got)
	}
}

// TestClient_PostMessage_exhaustsBudget asserts that when retries
// cannot recover, the final error is a *SlackError with
// ExhaustedRetries=true and Attempts > 1.
func TestClient_PostMessage_exhaustsBudget(t *testing.T) {
	rs := &retryingServer{failCount: 99, failStatus: 503} // always fail
	srv := httptest.NewServer(rs.handler())
	t.Cleanup(srv.Close)

	c := slack.NewClient(
		slack.WithBaseURL(srv.URL),
		slack.WithRetryBudget(1500*time.Millisecond),
	)
	_, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{ChannelID: "C0"})
	if err == nil {
		t.Fatal("expected error after exhaustion")
	}
	var se *slack.SlackError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SlackError, got %T", err)
	}
	if !se.ExhaustedRetries {
		t.Errorf("ExhaustedRetries: got false, want true (attempts=%d)", se.Attempts)
	}
	if se.Attempts < 2 {
		t.Errorf("Attempts: got %d, want ≥ 2", se.Attempts)
	}
	if se.Kind != slack.KindTransient {
		t.Errorf("Kind: got %v, want %v", se.Kind, slack.KindTransient)
	}
}

// TestClient_PostMessage_noRetryOnPersistent confirms persistent
// errors (invalid_auth) bail immediately without burning the budget.
func TestClient_PostMessage_noRetryOnPersistent(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	t.Cleanup(srv.Close)

	c := slack.NewClient(
		slack.WithBaseURL(srv.URL),
		slack.WithRetryBudget(5*time.Second),
	)
	start := time.Now()
	_, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{ChannelID: "C0"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls: got %d, want 1 (no retry for persistent)", calls.Load())
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed: %v — should bail fast on persistent error", elapsed)
	}
}

// TestClient_PostMessage_zeroBudgetDisablesRetries asserts that
// WithRetryBudget(0) makes the client one-shot.
func TestClient_PostMessage_zeroBudgetDisablesRetries(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	c := slack.NewClient(
		slack.WithBaseURL(srv.URL),
		slack.WithRetryBudget(0),
	)
	_, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{ChannelID: "C0"})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls: got %d, want 1 (zero budget = no retry)", calls.Load())
	}
	var se *slack.SlackError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SlackError, got %T", err)
	}
	if se.ExhaustedRetries {
		t.Errorf("ExhaustedRetries: got true, want false (no retry attempted)")
	}
}

// TestClient_PostMessage_honorsRetryAfter asserts that a 429 with a
// short Retry-After is honored (the test budget is enough to absorb
// it).
func TestClient_PostMessage_honorsRetryAfter(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"x","channel":"C0"}`))
	}))
	t.Cleanup(srv.Close)

	c := slack.NewClient(
		slack.WithBaseURL(srv.URL),
		slack.WithRetryBudget(3*time.Second),
	)
	start := time.Now()
	_, err := c.PostMessage(context.Background(), secret.SecretString("xoxb-tok"), slack.PostMessageInput{ChannelID: "C0"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed %v — Retry-After: 1 should have caused ~1s wait", elapsed)
	}
}

// TestClient_PostMessage_contextCancelShortCircuits asserts that
// cancelling the context returns ctx.Err() rather than a SlackError.
func TestClient_PostMessage_contextCancelShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	c := slack.NewClient(
		slack.WithBaseURL(srv.URL),
		slack.WithRetryBudget(10*time.Second),
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.PostMessage(ctx, secret.SecretString("xoxb-tok"), slack.PostMessageInput{ChannelID: "C0"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
