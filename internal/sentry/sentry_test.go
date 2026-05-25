package sentry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sentrygo "github.com/getsentry/sentry-go"
)

// captureTransport implements sentrygo.Transport. It records every
// event the SDK would have shipped so tests can assert against the
// in-memory list instead of standing up an HTTP server.
type captureTransport struct {
	mu     sync.Mutex
	events []*sentrygo.Event
}

func (c *captureTransport) Configure(_ sentrygo.ClientOptions) {}
func (c *captureTransport) SendEvent(e *sentrygo.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}
func (c *captureTransport) Flush(_ time.Duration) bool              { return true }
func (c *captureTransport) FlushWithContext(_ context.Context) bool { return true }
func (c *captureTransport) Close()                                  {}

// withFakeTransport swaps the global Sentry hub for one backed by a
// captureTransport, runs fn, and returns the captured events. The
// hub is restored on cleanup so tests don't bleed into one another.
func withFakeTransport(t *testing.T) *captureTransport {
	t.Helper()
	transport := &captureTransport{}
	client, err := sentrygo.NewClient(sentrygo.ClientOptions{
		Dsn:       "https://public@example.invalid/1",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	prev := sentrygo.CurrentHub()
	hub := sentrygo.NewHub(client, sentrygo.NewScope())
	sentrygo.CurrentHub().BindClient(client)
	_ = prev
	_ = hub
	t.Cleanup(func() {
		sentrygo.CurrentHub().BindClient(nil)
	})
	return transport
}

func TestInit_DisabledIsNoop(t *testing.T) {
	flush, err := Init(Config{Enabled: false}, "v0.0.0")
	if err != nil {
		t.Fatalf("Init disabled: %v", err)
	}
	if flush == nil {
		t.Fatal("flush must not be nil even when disabled")
	}
	flush() // must not panic
}

func TestInit_EnabledButMissingDSN(t *testing.T) {
	_, err := Init(Config{Enabled: true, DSN: ""}, "v0.0.0")
	if err == nil {
		t.Fatal("expected error when DSN is empty")
	}
}

func TestHandler_DropsBelowError(t *testing.T) {
	h := &slogHandler{}
	for _, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if h.Enabled(context.Background(), lvl) {
			t.Errorf("level %s should be dropped", lvl)
		}
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("LevelError should be enabled")
	}
}

func TestHandler_ErrorAttrBecomesException(t *testing.T) {
	transport := withFakeTransport(t)
	logger := slog.New(&slogHandler{})
	logger.Error("db write failed", "monitor", "api-x", "error", errors.New("boom"))

	if got := len(transport.events); got != 1 {
		t.Fatalf("want 1 captured event, got %d", got)
	}
	ev := transport.events[0]
	if ev.Message != "db write failed" {
		t.Errorf("message: got %q want %q", ev.Message, "db write failed")
	}
	if len(ev.Exception) != 1 {
		t.Fatalf("want 1 exception, got %d", len(ev.Exception))
	}
	if ev.Exception[0].Value != "boom" {
		t.Errorf("exception value: got %q want %q", ev.Exception[0].Value, "boom")
	}
	if got, ok := ev.Tags["monitor"]; !ok || got != "api-x" {
		t.Errorf("monitor tag: got %q ok=%v, want %q", got, ok, "api-x")
	}
}

func TestHandler_MessageOnlyWithoutErrorAttr(t *testing.T) {
	transport := withFakeTransport(t)
	logger := slog.New(&slogHandler{})
	logger.Error("invariant violated", "where", "scheduler")

	if got := len(transport.events); got != 1 {
		t.Fatalf("want 1 captured event, got %d", got)
	}
	ev := transport.events[0]
	if len(ev.Exception) != 0 {
		t.Errorf("want no exceptions, got %d", len(ev.Exception))
	}
	slogCtx, ok := ev.Contexts["slog"]
	if !ok {
		t.Fatalf("expected slog context, contexts=%v", ev.Contexts)
	}
	if got := slogCtx["where"]; got != "scheduler" {
		t.Errorf("slog.where: got %v want %q", got, "scheduler")
	}
}

func TestHandler_WithAttrsPreserved(t *testing.T) {
	transport := withFakeTransport(t)
	base := slog.New(&slogHandler{})
	logger := base.With("monitor", "static-api")
	logger.Error("event sink", "error", errors.New("transport"))

	if got := len(transport.events); got != 1 {
		t.Fatalf("want 1 captured event, got %d", got)
	}
	ev := transport.events[0]
	if got := ev.Tags["monitor"]; got != "static-api" {
		t.Errorf("monitor tag from WithAttrs: got %q want %q", got, "static-api")
	}
}

func TestRecoverPanic_CapturesAndDoesNotRepanic(t *testing.T) {
	transport := withFakeTransport(t)
	logger := slog.New(&slogHandler{})
	func() {
		defer RecoverPanic(logger, "test.site")
		panic("kapow")
	}()
	// The slog bridge fires once (log.Error inside RecoverPanic) and
	// hub.Recover fires once. Both events are valid; we assert at
	// least one and that the panic message is present somewhere.
	if len(transport.events) == 0 {
		t.Fatal("want >=1 captured event after panic")
	}
	found := false
	for _, ev := range transport.events {
		if ev.Message == "recovered panic" {
			found = true
		}
		for _, ex := range ev.Exception {
			if ex.Value != "" && (containsAll(ex.Value, "panic in test.site", "kapow") || containsAll(ex.Value, "kapow")) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no captured event mentioned the panic; got %d events", len(transport.events))
	}
}

func TestHTTPMiddleware_CapturesPanicAndRepanics(t *testing.T) {
	transport := withFakeTransport(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("from-handler")
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(HTTPMiddleware(mux))
	t.Cleanup(srv.Close)

	// Repanic: true → middleware reports to Sentry then re-panics.
	// net/http's per-conn recover then kills the connection (no
	// response written), so the client sees an EOF/connection error.
	if _, err := http.Get(srv.URL + "/boom"); err == nil {
		t.Fatal("expected error from panicking handler (Repanic=true)")
	}
	if len(transport.events) == 0 {
		t.Fatal("expected at least one Sentry event from the panicking request")
	}

	// Server is still alive after the panicked goroutine.
	resp, err := http.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("server should still be alive: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post-panic OK request: status=%d", resp.StatusCode)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub == "" {
			continue
		}
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
