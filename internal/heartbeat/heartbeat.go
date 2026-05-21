// Package heartbeat emits periodic POSTs to an external deadman URL
// (healthchecks.io, Better Stack, Dead Man's Snitch, etc.) and
// switches to {url}/fail when the worker has stalled.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Source is the slim seam the heartbeat uses to gather body data and
// detect stalls. Production wires lifecycle.Adapter; tests pass a
// stub.
type Source interface {
	// LastTick returns the time of the most recent completed check
	// (success or failure). Returns the zero time if no tick has run
	// yet — the heartbeat treats that as healthy until 2× interval
	// has elapsed since startup.
	LastTick() time.Time
	// OpenIncidents returns the count of currently-down monitors at
	// call time. Errors are logged but do not block the heartbeat.
	OpenIncidents(ctx context.Context) (int, error)
}

// Heartbeat is the periodic emitter. Owned by lifecycle.RunServe.
type Heartbeat struct {
	url      string
	interval time.Duration
	failMode bool
	source   Source
	http     *http.Client
	log      *slog.Logger
	now      func() time.Time
	started  time.Time
}

// Options configures a Heartbeat.
type Options struct {
	URL                 string
	Interval            time.Duration
	FailOnStalledWorker bool
	Source              Source
	HTTPClient          *http.Client // nil → 10s timeout default
	Logger              *slog.Logger
	Now                 func() time.Time // override for tests; defaults to time.Now
}

// New builds a Heartbeat. URL and Interval must be non-empty / positive.
func New(opts Options) *Heartbeat {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Heartbeat{
		url:      opts.URL,
		interval: opts.Interval,
		failMode: opts.FailOnStalledWorker,
		source:   opts.Source,
		http:     hc,
		log:      log,
		now:      now,
		started:  now(),
	}
}

// Run ticks every interval until ctx is cancelled. On each tick it
// posts a body describing current open incidents and the last tick
// time. When the worker has stalled (no check completion within
// max(2 × interval, 6 min)) and FailOnStalledWorker is true, it posts
// to {url}/fail instead.
func (h *Heartbeat) Run(ctx context.Context) {
	if h.interval <= 0 || h.url == "" {
		return
	}
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.beat(ctx)
		}
	}
}

// Beat performs one emission. Exported for tests that want to drive
// the loop directly without sleeping.
func (h *Heartbeat) Beat(ctx context.Context) { h.beat(ctx) }

func (h *Heartbeat) beat(ctx context.Context) {
	body := struct {
		OpenIncidents int    `json:"openIncidents"`
		LastTickAt    string `json:"lastTickAt"`
	}{}
	if h.source != nil {
		if n, err := h.source.OpenIncidents(ctx); err == nil {
			body.OpenIncidents = n
		} else {
			h.log.Warn("heartbeat: open-incidents lookup failed", "error", err)
		}
		if t := h.source.LastTick(); !t.IsZero() {
			body.LastTickAt = t.UTC().Format(time.RFC3339Nano)
		}
	}

	url := h.url
	if h.failMode && h.isStalled() {
		url = h.url + "/fail"
		h.log.Warn("heartbeat: worker appears stalled, posting to /fail",
			"liveness_threshold", h.livenessThreshold(),
			"last_tick", body.LastTickAt)
	}

	if err := h.post(ctx, url, body); err != nil {
		h.log.Warn("heartbeat: POST failed", "url", url, "error", err)
	}
}

// SendShutdown emits a final {"event": "shutdown"} POST. Called by
// lifecycle on graceful shutdown.
func (h *Heartbeat) SendShutdown(ctx context.Context) {
	if h.url == "" {
		return
	}
	if err := h.post(ctx, h.url, map[string]string{"event": "shutdown"}); err != nil {
		h.log.Warn("heartbeat: shutdown POST failed", "error", err)
	}
}

func (h *Heartbeat) post(ctx context.Context, url string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("non-2xx response: %s", resp.Status)
	}
	return nil
}

// isStalled reports whether the worker last completed a check more
// than max(2 × interval, 6 min) ago. Mirrors the criterion from
// docs/design-decisions.md §"Auth, health probes, heartbeat".
//
// If LastTick is zero, the worker hasn't completed a tick yet — we
// treat that as healthy until the same threshold has elapsed since
// startup, so a slow first tick doesn't cause a spurious /fail.
func (h *Heartbeat) isStalled() bool {
	threshold := h.livenessThreshold()
	now := h.now()
	if h.source == nil {
		return false
	}
	last := h.source.LastTick()
	if last.IsZero() {
		return now.Sub(h.started) > threshold
	}
	return now.Sub(last) > threshold
}

func (h *Heartbeat) livenessThreshold() time.Duration {
	const floor = 6 * time.Minute
	d := 2 * h.interval
	if d < floor {
		d = floor
	}
	return d
}
