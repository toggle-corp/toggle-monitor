package heartbeat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/heartbeat"
)

// fakeSource implements heartbeat.Source with values controllable from
// the test.
type fakeSource struct {
	mu       sync.Mutex
	lastTick time.Time
	open     int
}

func (f *fakeSource) LastTick() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTick
}

func (f *fakeSource) OpenIncidents(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open, nil
}

// deadmanRecorder is a stand-in for healthchecks.io.
type deadmanRecorder struct {
	mu       sync.Mutex
	hits     []deadmanHit
	failHits int
}

type deadmanHit struct {
	Path string
	Body map[string]any
}

func (d *deadmanRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.hits = append(d.hits, deadmanHit{Path: r.URL.Path, Body: body})
		if strings.HasSuffix(r.URL.Path, "/fail") {
			d.failHits++
		}
		d.mu.Unlock()
		w.WriteHeader(200)
	})
}

func TestHeartbeat_postsBodyWithIncidentsAndLastTick(t *testing.T) {
	recorder := &deadmanRecorder{}
	srv := httptest.NewServer(recorder.handler())
	t.Cleanup(srv.Close)

	lastTick := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{lastTick: lastTick, open: 3}

	hb := heartbeat.New(heartbeat.Options{
		URL:                 srv.URL + "/ping",
		Interval:            30 * time.Second,
		FailOnStalledWorker: false,
		Source:              src,
	})
	hb.Beat(context.Background())

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(recorder.hits))
	}
	hit := recorder.hits[0]
	if hit.Path != "/ping" {
		t.Errorf("path: got %q, want /ping", hit.Path)
	}
	if got := hit.Body["openIncidents"]; got != float64(3) {
		t.Errorf("openIncidents: got %v, want 3", got)
	}
	if !strings.HasPrefix(hit.Body["lastTickAt"].(string), "2026-05-21T12:00:00") {
		t.Errorf("lastTickAt: got %v", hit.Body["lastTickAt"])
	}
}

func TestHeartbeat_postsFailWhenStalledAndConfigured(t *testing.T) {
	recorder := &deadmanRecorder{}
	srv := httptest.NewServer(recorder.handler())
	t.Cleanup(srv.Close)

	// LastTick well in the past; threshold is max(2*30s, 6m) = 6m.
	lastTick := time.Now().Add(-20 * time.Minute)
	src := &fakeSource{lastTick: lastTick, open: 0}

	hb := heartbeat.New(heartbeat.Options{
		URL:                 srv.URL + "/ping",
		Interval:            30 * time.Second,
		FailOnStalledWorker: true,
		Source:              src,
	})
	hb.Beat(context.Background())

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.failHits != 1 {
		t.Errorf("/fail hits: got %d, want 1", recorder.failHits)
	}
}

func TestHeartbeat_doesNotFailWhenWorkerHealthy(t *testing.T) {
	recorder := &deadmanRecorder{}
	srv := httptest.NewServer(recorder.handler())
	t.Cleanup(srv.Close)

	src := &fakeSource{lastTick: time.Now(), open: 0}
	hb := heartbeat.New(heartbeat.Options{
		URL:                 srv.URL + "/ping",
		Interval:            30 * time.Second,
		FailOnStalledWorker: true,
		Source:              src,
	})
	hb.Beat(context.Background())

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.failHits != 0 {
		t.Errorf("expected NO /fail hit while worker is healthy, got %d", recorder.failHits)
	}
}

func TestHeartbeat_sendShutdownEmitsShutdownEvent(t *testing.T) {
	recorder := &deadmanRecorder{}
	srv := httptest.NewServer(recorder.handler())
	t.Cleanup(srv.Close)

	hb := heartbeat.New(heartbeat.Options{
		URL:      srv.URL + "/ping",
		Interval: 30 * time.Second,
	})
	hb.SendShutdown(context.Background())

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.hits) != 1 {
		t.Fatalf("hits: got %d, want 1", len(recorder.hits))
	}
	if recorder.hits[0].Body["event"] != "shutdown" {
		t.Errorf("event: got %v, want shutdown", recorder.hits[0].Body["event"])
	}
}

// TestHeartbeat_runStopsOnContextCancel sanity-checks the run loop
// returns promptly when ctx is cancelled.
func TestHeartbeat_runStopsOnContextCancel(t *testing.T) {
	recorder := &deadmanRecorder{}
	srv := httptest.NewServer(recorder.handler())
	t.Cleanup(srv.Close)

	hb := heartbeat.New(heartbeat.Options{
		URL:      srv.URL + "/ping",
		Interval: 30 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var stopped atomic.Bool
	go func() {
		hb.Run(ctx)
		stopped.Store(true)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if !stopped.Load() {
		t.Error("expected Run to return after cancel")
	}
}
