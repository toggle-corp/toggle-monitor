//go:build integration

package lifecycle_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/lifecycle"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

// outageSim is the shared rig for the internet-outage simulations: a
// fake DNS zone whose gate models the outage, an upstream every monitor
// probes through that zone, and a fake Slack reachable only through the
// same zone (so Slack goes dark exactly when the probes do, as it did
// in production).
type outageSim struct {
	dns      *fakeDNS
	slack    *fakeSlackRecorder
	upstream *httptest.Server
	internal *httptest.Server
	slackSrv *httptest.Server
	dbCfg    db.Config

	// netDown models the monitor losing the internet: name resolution
	// dies, so both the probe targets and Slack become unreachable.
	netDown atomic.Bool
	// targetDown models the monitored estate becoming unreachable while
	// the monitor's own resolver and its Slack egress keep working — the
	// shape of a provider/region outage, and of any egress loss where
	// DNS still answers from cache.
	targetDown atomic.Bool

	upstreamPort string
	internalPort string
	slackPort    string
}

// gated wraps a handler so that, while the partition is on, the
// connection is torn down instead of served. Without this a probe can
// keep riding a pooled keep-alive connection and never re-resolve the
// name — an artifact of running the whole simulation on loopback, where
// established sockets survive a DNS outage that would have killed them
// in production.
func gated(down *atomic.Bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
			return
		}
		h.ServeHTTP(w, r)
	})
}

func newOutageSim(t *testing.T) *outageSim {
	t.Helper()

	sim := &outageSim{}

	upstream := httptest.NewServer(gated(&sim.targetDown, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	)))
	t.Cleanup(upstream.Close)

	// The in-cluster upstream is deliberately NOT gated: an egress
	// outage leaves cluster-local services answering normally.
	internalSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	))
	t.Cleanup(internalSrv.Close)

	recorder := &fakeSlackRecorder{}
	slackSrv := httptest.NewServer(gated(&sim.netDown, recorder.handler()))
	t.Cleanup(slackSrv.Close)

	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	dbCfg, err := dbConfigFromDSN(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}

	// The resolver override goes in last so the Postgres container and
	// the two httptest servers are already bound via the real one.
	sim.dns = startFakeDNS(t)
	sim.slack = recorder
	sim.upstream = upstream
	sim.internal = internalSrv
	sim.slackSrv = slackSrv
	sim.dbCfg = dbCfg
	sim.upstreamPort = portOf(t, upstream.URL)
	sim.internalPort = portOf(t, internalSrv.URL)
	sim.slackPort = portOf(t, slackSrv.URL)
	return sim
}

// setOutage flips a full internet outage: DNS dies, so every probe and
// every Slack call fails name resolution. Closing the servers' live
// connections models the partition killing established sockets, which
// forces the next probe to re-resolve and hit the dead zone.
func (s *outageSim) setOutage(down bool) {
	s.netDown.Store(down)
	s.targetDown.Store(down)
	s.dns.setUp(!down)
	s.upstream.CloseClientConnections()
	s.slackSrv.CloseClientConnections()
}

// setTargetsDown takes the monitored estate offline while leaving the
// monitor's resolver and Slack egress intact.
func (s *outageSim) setTargetsDown(down bool) {
	s.targetDown.Store(down)
	s.upstream.CloseClientConnections()
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	trimmed := strings.TrimPrefix(rawURL, "http://")
	_, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("split %q: %v", rawURL, err)
	}
	return port
}

// run boots RunServe against the simulated network and returns a stop
// func. The Slack base URL is addressed by name so Slack delivery dies
// with the rest of the internet when the gate closes.
func (s *outageSim) run(t *testing.T, cfg config.Config) func() {
	t.Helper()
	addrCh := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- lifecycle.RunServe(ctx, lifecycle.ServeOptions{
			Config:       cfg,
			DBConfig:     s.dbCfg,
			ListenAddr:   "127.0.0.1:0",
			SlackBaseURL: "http://slack.outage.test:" + s.slackPort,
			OnReady:      func(a net.Addr) { addrCh <- a },
		})
	}()
	select {
	case <-addrCh:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("RunServe never bound")
	}
	return func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(20 * time.Second):
			t.Error("RunServe did not exit cleanly")
		}
	}
}

// posts returns a compact one-line summary of every chat.postMessage
// the fake Slack has seen, in order — the artifact the assertions and
// the diagnostic dump both read.
func (s *outageSim) posts() []string {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	out := make([]string, 0, len(s.slack.postMessages))
	for _, p := range s.slack.postMessages {
		kind := "parent"
		if _, threaded := p["thread_ts"]; threaded {
			kind = "reply "
		}
		out = append(out, kind+" | "+firstN(flatten(p["blocks"]), 160))
	}
	return out
}

// flatten renders every string nested in a blocks payload into one
// line, so a test failure shows what actually landed in Slack.
func flatten(v any) string {
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			sb.WriteString(strings.ReplaceAll(t, "\n", " ⏎ "))
			sb.WriteString(" ")
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, k := range []string{"text", "elements", "fields"} {
				if e, ok := t[k]; ok {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return strings.TrimSpace(sb.String())
}

func (s *outageSim) dump(t *testing.T, label string) {
	t.Helper()
	posts := s.posts()
	t.Logf("=== %s: %d chat.postMessage call(s) ===", label, len(posts))
	for i, p := range posts {
		t.Logf("  [%02d] %s", i, p)
	}
	s.slack.mu.Lock()
	updates := len(s.slack.updateMessages)
	s.slack.mu.Unlock()
	t.Logf("  (%d chat.update call(s))", updates)
}

// parentPosts counts top-level (non-threaded) messages — the "how many
// notifications did the channel get" number the operator sees.
func (s *outageSim) parentPosts() int {
	n := 0
	for _, p := range s.posts() {
		if strings.HasPrefix(p, "parent") {
			n++
		}
	}
	return n
}

// downParents counts the top-level messages announcing a monitor or a
// digest as DOWN — the ones an operator expects to see edited to a
// resolved form once the incident closes.
func (s *outageSim) downParents() int {
	n := 0
	for _, p := range s.posts() {
		if strings.HasPrefix(p, "parent") && strings.Contains(p, "DOWN") {
			n++
		}
	}
	return n
}

func (s *outageSim) updates() int {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	return len(s.slack.updateMessages)
}

// simConfig renders the YAML for a simulation. external monitors are
// addressed through the fake DNS zone (so they die in the outage);
// internal ones are addressed by IP (so they keep succeeding, modelling
// the in-cluster services that stay reachable when only egress dies).
type simConfig struct {
	external int
	internal int
	selfHeal bool
	// interval is the per-monitor probe interval. It matters: the
	// scheduler jitters each monitor's first tick across a full
	// interval, so the wider it is relative to pendingWait, the more
	// thinly a cluster-wide outage trickles into the dispatcher.
	interval string
	// pendingWait / burstThreshold are the ADR-0004 dispatcher knobs.
	pendingWait    string
	burstThreshold int
}

func (sc simConfig) withDefaults() simConfig {
	if sc.interval == "" {
		sc.interval = "500ms"
	}
	if sc.pendingWait == "" {
		sc.pendingWait = "2s"
	}
	if sc.burstThreshold == 0 {
		sc.burstThreshold = 5
	}
	return sc
}

func (s *outageSim) yaml(t *testing.T, sc simConfig) config.Config {
	t.Helper()
	sc = sc.withDefaults()
	var mons strings.Builder
	monitor := func(slug, host, port string) {
		fmt.Fprintf(&mons, `
  - slug: %s
    friendlyName: %s
    url: http://%s:%s/
    tags: [prod]
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: %s
    timeout: 300ms
    retries: 0
    retryBackoff: 1s
    followRedirects: false
    reminderInterval: 3s
    slack: ops-alerts
`, slug, slug, host, port, sc.interval)
	}
	for i := 1; i <= sc.external; i++ {
		monitor(fmt.Sprintf("ext-%d", i), fmt.Sprintf("svc%d.outage.test", i), s.upstreamPort)
	}
	for i := 1; i <= sc.internal; i++ {
		monitor(fmt.Sprintf("int-%d", i), "127.0.0.1", s.internalPort)
	}

	selfHealth := ""
	if sc.selfHeal {
		selfHealth = `
selfHealth:
  window: 3s
  minMonitors: 3
  channel: ops-alerts
`
	}

	raw := fmt.Sprintf(`
displayTimezone: UTC
publicBaseURL: https://monitor.example.test
dbBodyMaxChars: 4000
database:
  host: %s
  port: %d
  user: %s
  name: %s
  sslMode: %s
  passwordEnv: TOGGLE_DB_PASSWORD
ui:
  pageSize: { homepageAlerts: 20, monitorListing: 50, monitorHistory: 50, discoveryListing: 50 }
  maxPerPage: 200
httpClient: { userAgent: "toggle-monitor/outage-sim" }
slack:
  bodyMaxChars: 200
  channels:
    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: TOGGLE_SLACK_TOKEN }
  coalesce:
    pendingWait: %s
    burstThreshold: %d
    groupInterval: 3s
    repeatInterval: 1h
%s
monitors:%s
`,
		s.dbCfg.Host, s.dbCfg.Port, s.dbCfg.User, s.dbCfg.Name, s.dbCfg.SSLMode,
		sc.pendingWait, sc.burstThreshold, selfHealth, mons.String(),
	)

	cfg, err := config.Load([]byte(raw))
	if err != nil {
		t.Fatalf("config.Load: %v\n%s", err, raw)
	}
	t.Setenv("TOGGLE_DB_PASSWORD", s.dbCfg.Password)
	t.Setenv("TOGGLE_SLACK_TOKEN", "xoxb-test")
	return cfg
}

// TestOutageSimulation_allExternalMonitorsGoBlind is the ADR-0008
// headline case: the monitor loses the internet, every probe fails
// name resolution, Slack is unreachable for the whole outage, and
// connectivity returns. The channel must end up with ONE notification,
// not one per monitor.
func TestOutageSimulation_allExternalMonitorsGoBlind(t *testing.T) {
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{external: 8, selfHeal: true})
	stop := sim.run(t, cfg)
	defer stop()

	// 1. Healthy: every monitor is up and nothing has been posted.
	time.Sleep(3 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	// 2. Internet outage.
	t.Log("--- internet DOWN ---")
	sim.setOutage(true)
	time.Sleep(20 * time.Second)
	sim.dump(t, "during outage")

	// 3. Internet restored.
	t.Log("--- internet UP ---")
	sim.setOutage(false)
	time.Sleep(20 * time.Second)
	sim.dump(t, "after recovery")

	if n := sim.parentPosts(); n > 1 {
		t.Errorf("STORM: channel got %d parent notifications for one outage; want at most 1", n)
	}
}

// TestOutageSimulation_someMonitorsStayReachable models the realistic
// shape of an office/egress internet outage: external targets go dark
// while in-cluster targets keep answering. The self-health detector's
// zero-success veto does not trip here — so the burst dispatcher is the
// only thing standing between the operator and one message per monitor.
func TestOutageSimulation_someMonitorsStayReachable(t *testing.T) {
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{external: 8, internal: 2, selfHeal: true})
	stop := sim.run(t, cfg)
	defer stop()

	time.Sleep(3 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	t.Log("--- internet DOWN (internal targets stay up) ---")
	sim.setOutage(true)
	time.Sleep(20 * time.Second)
	sim.dump(t, "during outage")

	t.Log("--- internet UP ---")
	sim.setOutage(false)
	time.Sleep(20 * time.Second)
	sim.dump(t, "after recovery")

	if n := sim.parentPosts(); n > 1 {
		t.Errorf("STORM: channel got %d parent notifications for one outage; want at most 1", n)
	}
}

// TestOutageSimulation_trickleDefeatsTheBurstThreshold is the shape a
// production estate actually has: probe intervals far wider than the
// dispatcher's pendingWait. The scheduler jitters each monitor's first
// tick across a full interval, so a single cluster-wide outage does not
// arrive as a burst — it arrives as a trickle, a monitor or two per
// pendingWait window. Every window lands under burstThreshold, so the
// dispatcher flushes each one individually and the channel gets one
// message per monitor instead of one digest.
//
// Slack stays reachable here on purpose: this isolates the dispatcher's
// grouping decision from any delivery failure.
func TestOutageSimulation_trickleDefeatsTheBurstThreshold(t *testing.T) {
	const (
		monitors       = 12
		burstThreshold = 5
	)
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{
		external:       monitors,
		selfHeal:       true,
		interval:       "24s",
		pendingWait:    "2s",
		burstThreshold: burstThreshold,
	})
	stop := sim.run(t, cfg)
	defer stop()

	// Let the jittered first ticks spread out and every monitor come up.
	time.Sleep(26 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	t.Log("--- monitored estate DOWN (DNS + Slack still fine) ---")
	sim.setTargetsDown(true)
	time.Sleep(40 * time.Second)
	sim.dump(t, "during outage")
	storm := sim.parentPosts()

	t.Log("--- monitored estate UP ---")
	sim.setTargetsDown(false)
	time.Sleep(30 * time.Second)
	sim.dump(t, "after recovery")

	// Every DOWN parent the channel received must end up edited to its
	// resolved form. A parent that is never edited is a red circle left
	// standing in Slack after the incident closed.
	downParents, updates := sim.downParents(), sim.updates()
	if updates < downParents {
		t.Errorf("ORPHANED: %d DOWN parent(s) posted but only %d edited to resolved", downParents, updates)
	}
	// The contract: the channel pages individually until the cumulative
	// burst count crosses burstThreshold, then everything else lands in
	// one digest. So at most burstThreshold-1 individual pages plus that
	// digest — never one message per monitor.
	if storm > burstThreshold {
		t.Errorf("STORM: channel got %d parent notifications for a %d-monitor outage; "+
			"want at most %d (burstThreshold-1 individual pages + 1 digest)",
			storm, monitors, burstThreshold)
	}
}
