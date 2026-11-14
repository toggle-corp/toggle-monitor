//go:build integration

package lifecycle_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	rootSrv  *httptest.Server
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
	// rootDown takes the dependsOn root alone offline. The root has its
	// own upstream so its outage can start and end apart from its
	// children's.
	rootDown atomic.Bool

	upstreamPort string
	rootPort     string
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

	rootSrv := httptest.NewServer(gated(&sim.rootDown, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	)))
	t.Cleanup(rootSrv.Close)

	// The in-cluster upstream is not gated: an egress outage leaves
	// cluster-local services answering normally.
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
	// Pin the container's host to an address literal while the real
	// resolver is still installed. The outage models losing the
	// internet, not losing the in-cluster database, so the pool must
	// never route a reconnect through the dead zone.
	dbCfg.Host = resolveToIPv4(t, dbCfg.Host)

	// The resolver override goes in last so the Postgres container and
	// the two httptest servers are already bound via the real one.
	sim.dns = startFakeDNS(t)
	sim.slack = recorder
	sim.upstream = upstream
	sim.rootSrv = rootSrv
	sim.internal = internalSrv
	sim.slackSrv = slackSrv
	sim.dbCfg = dbCfg
	sim.upstreamPort = portOf(t, upstream.URL)
	sim.rootPort = portOf(t, rootSrv.URL)
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
	s.rootDown.Store(down)
	s.dns.setUp(!down)
	s.upstream.CloseClientConnections()
	s.rootSrv.CloseClientConnections()
	s.slackSrv.CloseClientConnections()
}

// setTargetsDown takes the monitored estate offline while leaving the
// monitor's resolver and Slack egress intact.
func (s *outageSim) setTargetsDown(down bool) {
	s.setLeavesDown(down)
	s.setRootDown(down)
}

// setLeavesDown takes every external target except the dependsOn root
// offline.
func (s *outageSim) setLeavesDown(down bool) {
	s.targetDown.Store(down)
	s.upstream.CloseClientConnections()
}

// setRootDown takes the dependsOn root alone offline.
func (s *outageSim) setRootDown(down bool) {
	s.rootDown.Store(down)
	s.rootSrv.CloseClientConnections()
}

// setSlackDown takes Slack alone offline: its connections are torn down
// while DNS and the monitored estate are untouched, so every Slack call
// fails and nothing else does.
func (s *outageSim) setSlackDown(down bool) {
	s.netDown.Store(down)
	s.slackSrv.CloseClientConnections()
}

// resolveToIPv4 turns a host name into an IPv4 literal, passing address
// literals through untouched.
func resolveToIPv4(t *testing.T, host string) string {
	t.Helper()
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), host)
	if err != nil {
		t.Fatalf("resolve %q: %v", host, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a
		}
	}
	t.Fatalf("resolve %q: no IPv4 address in %v", host, addrs)
	return ""
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

// parentBodies returns the full text of every top-level message, header
// blocks and attachment blocks alike, untruncated — banners and digest
// rows render inside the attachment.
func (s *outageSim) parentBodies() []string {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	var out []string
	for _, p := range s.slack.postMessages {
		if _, threaded := p["thread_ts"]; threaded {
			continue
		}
		text := flatten(p["blocks"])
		if atts, ok := p["attachments"].([]any); ok {
			for _, a := range atts {
				if am, ok := a.(map[string]any); ok {
					text += " " + flatten(am["blocks"])
				}
			}
		}
		out = append(out, text)
	}
	return out
}

// messageTexts returns the full text of every message the fake Slack
// received, posts and threaded replies and edits alike — a monitor that
// joins an open digest is named only in the digest's edit and its delta
// reply.
func (s *outageSim) messageTexts() []string {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	var out []string
	for _, batch := range [][]map[string]any{s.slack.postMessages, s.slack.updateMessages} {
		for _, p := range batch {
			text := flatten(p["blocks"])
			if atts, ok := p["attachments"].([]any); ok {
				for _, a := range atts {
					if am, ok := a.(map[string]any); ok {
						text += " " + flatten(am["blocks"])
					}
				}
			}
			out = append(out, text)
		}
	}
	return out
}

func (s *outageSim) dump(t *testing.T, label string) {
	t.Helper()
	posts := s.posts()
	t.Logf("=== %s: %d chat.postMessage call(s) ===", label, len(posts))
	for i, p := range posts {
		t.Logf("  [%02d] %s", i, p)
	}
	t.Logf("  (%d chat.update call(s) across %d message(s))", s.updates(), s.editedParents())
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

// editedToUp counts the distinct parents that received a chat.update
// rendering them as UP.
func (s *outageSim) editedToUp() int {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	seen := map[string]struct{}{}
	for _, u := range s.slack.updateMessages {
		ts, _ := u["ts"].(string)
		if strings.Contains(flatten(u["blocks"]), "is UP") {
			seen[ts] = struct{}{}
		}
	}
	return len(seen)
}

func (s *outageSim) updates() int {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	return len(s.slack.updateMessages)
}

// editedParents counts the distinct message timestamps that received a
// chat.update — one per parent the daemon edited to its resolved form.
// Counting raw chat.update calls instead would let a single digest's
// heartbeat edits stand in for every orphaned individual parent.
func (s *outageSim) editedParents() int {
	s.slack.mu.Lock()
	defer s.slack.mu.Unlock()
	seen := map[string]struct{}{}
	for _, u := range s.slack.updateMessages {
		if ts, ok := u["ts"].(string); ok {
			seen[ts] = struct{}{}
		}
	}
	return len(seen)
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
	// pendingWait / burstThreshold / burstWindow are the dispatcher
	// knobs (ADR-0004, ADR-0015). An empty burstWindow takes the config
	// default.
	pendingWait    string
	burstThreshold int
	burstWindow    string
	// reminderInterval is the per-monitor still-down reminder cadence.
	reminderInterval string
	// children adds that many `child-N` monitors that dependsOn a
	// critical `root` monitor. root and every child probe external
	// targets, so setTargetsDown takes the whole tree down.
	children int
	// rootInterval and rootShape override the root monitor's interval
	// and retry shape (YAML lines appended to its entry). An empty
	// rootInterval keeps the slow root described in yaml.
	rootInterval string
	rootShape    string
	// childShape is appended to every child's YAML entry (e.g. a retry
	// shape).
	childShape string
	// childChannels posts each child to a channel of its own, the shape
	// of an estate whose projects each have their own Slack channel. A
	// child's pending pool then never holds a sibling, so the
	// dispatcher's on-demand parent probe never fires for it.
	childChannels bool
	// leaves builds a nested tree instead: `mid` dependsOn the critical
	// `root` and shares root's upstream, and that many `leaf-N` monitors
	// dependsOn mid alone and probe the leaves' upstream. mid and every
	// leaf post to a channel of their own. setRootDown takes root and mid
	// down together; setLeavesDown takes the leaves down.
	leaves int
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
	if sc.reminderInterval == "" {
		sc.reminderInterval = "3s"
	}
	return sc
}

func (s *outageSim) yaml(t *testing.T, sc simConfig) config.Config {
	t.Helper()
	sc = sc.withDefaults()
	var mons strings.Builder
	// extra is appended verbatim to the monitor's YAML; it overrides the
	// retry shape when it sets retries / retryBackoff.
	monitor := func(slug, host, port, interval, channel, extra string) {
		retries := "\n    retries: 0\n    retryBackoff: 1s"
		if strings.Contains(extra, "retries:") {
			retries = ""
		}
		fmt.Fprintf(&mons, `
  - slug: %s
    friendlyName: %s
    url: http://%s:%s/
    tags: [prod]
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: %s
    timeout: 300ms%s
    followRedirects: false
    reminderInterval: %s
    slack: %s%s
`, slug, slug, host, port, interval, retries, sc.reminderInterval, channel, extra)
	}
	for i := 1; i <= sc.external; i++ {
		monitor(fmt.Sprintf("ext-%d", i), fmt.Sprintf("svc%d.outage.test", i), s.upstreamPort, sc.interval, "ops-alerts", "")
	}
	for i := 1; i <= sc.internal; i++ {
		monitor(fmt.Sprintf("int-%d", i), "127.0.0.1", s.internalPort, sc.interval, "ops-alerts", "")
	}
	if sc.children > 0 || sc.leaves > 0 {
		// root retries for longer than one child interval, so its own
		// tick concludes "down" only after the children's first pending
		// pool has expired. That makes the dispatcher's on-demand parent
		// probe the path that finds root down. Config validation wants
		// the retry budget inside the interval, hence root's wider one.
		rootInterval, rootShape := "20s", "\n    retries: 3\n    retryBackoff: 4s"
		if sc.rootInterval != "" {
			rootInterval, rootShape = sc.rootInterval, sc.rootShape
		}
		monitor("root", "root.outage.test", s.rootPort, rootInterval, "ops-alerts", "\n    critical: true"+rootShape)
		for i := 1; i <= sc.children; i++ {
			channel := "ops-alerts"
			if sc.childChannels {
				channel = fmt.Sprintf("child-%d-alerts", i)
			}
			monitor(fmt.Sprintf("child-%d", i), fmt.Sprintf("child%d.outage.test", i), s.upstreamPort,
				sc.interval, channel, "\n    dependsOn: [root]"+sc.childShape)
		}
		if sc.leaves > 0 {
			monitor("mid", "mid.outage.test", s.rootPort, sc.interval, "mid-alerts", "\n    dependsOn: [root]")
		}
		for i := 1; i <= sc.leaves; i++ {
			monitor(fmt.Sprintf("leaf-%d", i), fmt.Sprintf("leaf%d.outage.test", i), s.upstreamPort,
				sc.interval, fmt.Sprintf("leaf-%d-alerts", i), "\n    dependsOn: [mid]")
		}
	}

	channels := "\n    - { slug: ops-alerts, channelId: C0123ABCD, tokenEnv: TOGGLE_SLACK_TOKEN }"
	if sc.childChannels {
		for i := 1; i <= sc.children; i++ {
			channels += fmt.Sprintf("\n    - { slug: child-%d-alerts, channelId: C0CHILD%02d, tokenEnv: TOGGLE_SLACK_TOKEN }", i, i)
		}
	}
	if sc.leaves > 0 {
		channels += "\n    - { slug: mid-alerts, channelId: C0MID0001, tokenEnv: TOGGLE_SLACK_TOKEN }"
		for i := 1; i <= sc.leaves; i++ {
			channels += fmt.Sprintf("\n    - { slug: leaf-%d-alerts, channelId: C0LEAF%03d, tokenEnv: TOGGLE_SLACK_TOKEN }", i, i)
		}
	}

	burstWindow := ""
	if sc.burstWindow != "" {
		burstWindow = "\n    burstWindow: " + sc.burstWindow
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
  channels:%s
  coalesce:
    pendingWait: %s
    burstThreshold: %d%s
    groupInterval: 3s
    repeatInterval: 1h
%s
monitors:%s
`,
		s.dbCfg.Host, s.dbCfg.Port, s.dbCfg.User, s.dbCfg.Name, s.dbCfg.SSLMode,
		channels, sc.pendingWait, sc.burstThreshold, burstWindow, selfHealth, mons.String(),
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
	downParents, edited := sim.downParents(), sim.editedParents()
	if edited < downParents {
		t.Errorf("ORPHANED: %d DOWN parent(s) posted but only %d edited to resolved", downParents, edited)
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

// TestOutageSimulation_orphanResolvesGroupIntoOneMessage covers the tail
// of an outage that also took Slack down: every Open failed to deliver,
// so no monitor has a parent message to edit, and each recovery would
// post its own standalone "is UP" with the late-notice banner. The
// channel must instead get one grouped recovery message naming every
// monitor.
//
// The estate stays under burstThreshold so each Open flushes
// individually, and reminders are pushed out of the test's reach so
// none synthesizes a fresh parent once Slack returns. burstWindow sits
// above the probe interval so the jittered trickle of recoveries lands
// in one orphan pool.
func TestOutageSimulation_orphanResolvesGroupIntoOneMessage(t *testing.T) {
	const monitors = 4
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{
		external:         monitors,
		interval:         "8s",
		pendingWait:      "2s",
		burstThreshold:   5,
		burstWindow:      "12s",
		reminderInterval: "1h",
	})
	stop := sim.run(t, cfg)
	defer stop()

	time.Sleep(10 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	// Long enough for every monitor to fail and every Open flush to
	// exhaust the Slack client's retry budget.
	t.Log("--- monitored estate + Slack DOWN ---")
	sim.setSlackDown(true)
	sim.setTargetsDown(true)
	time.Sleep(55 * time.Second)
	sim.dump(t, "during outage")
	if n := sim.parentPosts(); n != 0 {
		t.Fatalf("setup: %d parent message(s) reached Slack while it was down", n)
	}

	t.Log("--- monitored estate + Slack UP ---")
	sim.setTargetsDown(false)
	sim.setSlackDown(false)
	time.Sleep(30 * time.Second)
	sim.dump(t, "after recovery")

	var late []string
	for _, body := range sim.parentBodies() {
		if strings.Contains(body, "Initial notification delivery failed") {
			late = append(late, body)
		}
	}
	if len(late) != 1 {
		t.Fatalf("STORM: channel got %d late-notice recovery message(s); want exactly 1", len(late))
	}
	for i := 1; i <= monitors; i++ {
		if name := fmt.Sprintf("ext-%d", i); !strings.Contains(late[0], name) {
			t.Errorf("grouped recovery does not list %s: %s", name, late[0])
		}
	}
	if n := sim.parentPosts(); n != 1 {
		t.Errorf("channel got %d parent message(s) for the outage; want 1", n)
	}
}

// TestOutageSimulation_dependsOnTreeAnnouncesOnlyTheRoot covers an
// estate where every monitor dependsOn one critical root, and
// the cut takes the root and its children down together while Slack
// keeps working. The children that fail before the root is known down
// sit in the dispatcher's pending pool; the on-demand parent probe finds
// the root down at pendingWait expiry and push-propagation pauses them
// out of the pool. Nobody was paged about those children, so their
// later recoveries must post nothing — and the root must be announced
// exactly once and edited to UP when it recovers.
//
// Children probe several times per pendingWait-sized slice of their
// interval so the first pending pool almost always holds two or more of
// them, which is what makes root a hot parent.
func TestOutageSimulation_dependsOnTreeAnnouncesOnlyTheRoot(t *testing.T) {
	const children = 16
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{
		children:         children,
		interval:         "6s",
		pendingWait:      "2s",
		burstThreshold:   5,
		burstWindow:      "12s",
		reminderInterval: "1h",
	})
	stop := sim.run(t, cfg)
	defer stop()

	time.Sleep(8 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	t.Log("--- root + children DOWN (Slack up) ---")
	sim.setTargetsDown(true)
	time.Sleep(30 * time.Second)
	sim.dump(t, "during outage")

	// root resolves on its next tick (up to 20s away); each child then
	// resolves on its own next tick.
	t.Log("--- root + children UP ---")
	sim.setTargetsDown(false)
	time.Sleep(40 * time.Second)
	sim.dump(t, "after recovery")

	// root's DOWN lists the children it pauses, so a message is about
	// root only by its title.
	var rootDown, other, lateNotice int
	for _, body := range sim.parentBodies() {
		if strings.Contains(body, "Initial notification delivery failed") {
			lateNotice++
		}
		if strings.Contains(body, "*root is DOWN*") {
			rootDown++
		} else {
			other++
		}
	}
	if lateNotice != 0 {
		t.Errorf("FALSE BANNER: %d message(s) claim the initial notification failed; Slack was up throughout", lateNotice)
	}
	if rootDown != 1 {
		t.Errorf("channel got %d DOWN message(s) for root; want exactly 1", rootDown)
	}
	if other != 0 {
		t.Errorf("channel got %d parent message(s) besides root's DOWN; want 0", other)
	}
	if up := sim.editedToUp(); up != 1 {
		t.Errorf("root's DOWN parent edited to UP %d time(s); want 1", up)
	}
}

// TestOutageSimulation_childProbesInFlightWhenRootGoesDown: the
// children's targets fail first and their probes start retrying while
// root is still up, so every child passes its dependsOn gate. root then
// goes down and is announced before any child's retries run out. Each
// child's failure is root's outage, so none may be announced — with
// every child on its own channel, no sibling shares its pending pool and
// the dispatcher's on-demand parent probe never steps in.
func TestOutageSimulation_childProbesInFlightWhenRootGoesDown(t *testing.T) {
	const children = 8
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{
		children:         children,
		childChannels:    true,
		interval:         "8s",
		childShape:       "\n    retries: 2\n    retryBackoff: 3s",
		rootInterval:     "1s",
		pendingWait:      "2s",
		burstThreshold:   5,
		burstWindow:      "16s",
		reminderInterval: "1h",
	})
	stop := sim.run(t, cfg)
	defer stop()

	time.Sleep(10 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	// A child probing in the next 3s fails and retries for 6s; root,
	// probing every second, is down and announced well before that.
	t.Log("--- children DOWN, root still up ---")
	sim.setLeavesDown(true)
	time.Sleep(3 * time.Second)
	t.Log("--- root DOWN ---")
	sim.setRootDown(true)
	time.Sleep(20 * time.Second)
	sim.dump(t, "during outage")

	t.Log("--- root + children UP ---")
	sim.setTargetsDown(false)
	time.Sleep(20 * time.Second)
	sim.dump(t, "after recovery")

	var rootDown, other int
	for _, body := range sim.parentBodies() {
		if strings.Contains(body, "*root is DOWN*") {
			rootDown++
		} else {
			other++
		}
	}
	if rootDown != 1 {
		t.Errorf("channel got %d DOWN message(s) for root; want exactly 1", rootDown)
	}
	if other != 0 {
		t.Errorf("RACE: %d parent message(s) besides root's DOWN; children failing with root must stay silent", other)
	}
	if up := sim.editedToUp(); up != 1 {
		t.Errorf("root's DOWN parent edited to UP %d time(s); want 1", up)
	}
}

// TestOutageSimulation_childrenStillDownWhenRootRecovers: the tree goes
// down together, the children's Opens are drained from the pending pool
// when root is found down, and root then recovers while every child
// keeps failing. Nobody was paged about the children, so each one's
// first resumed tick must announce it through the dispatcher: they
// trickle in, count toward the burst, and land as at most
// burstThreshold-1 individual pages plus one digest, within a probe
// interval of root's recovery rather than at their first reminder.
// Their own recoveries then edit whatever announced them.
func TestOutageSimulation_childrenStillDownWhenRootRecovers(t *testing.T) {
	const (
		children       = 12
		burstThreshold = 5
	)
	sim := newOutageSim(t)
	cfg := sim.yaml(t, simConfig{
		children:         children,
		interval:         "6s",
		pendingWait:      "2s",
		burstThreshold:   burstThreshold,
		burstWindow:      "12s",
		reminderInterval: "1h",
	})
	stop := sim.run(t, cfg)
	defer stop()

	time.Sleep(8 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	t.Log("--- root + children DOWN (Slack up) ---")
	sim.setTargetsDown(true)
	time.Sleep(30 * time.Second)
	sim.dump(t, "during outage")
	before := len(sim.parentBodies())

	// root resolves on its next tick (up to 20s away); each child then
	// resumes on its own next tick, still failing.
	t.Log("--- root UP, children still DOWN ---")
	sim.setRootDown(false)
	time.Sleep(45 * time.Second)
	sim.dump(t, "root recovered")

	bodies := sim.parentBodies()
	for _, body := range bodies {
		if strings.Contains(body, "Initial notification delivery failed") {
			t.Errorf("FALSE BANNER: Slack was up throughout: %s", body)
		}
	}
	// root's own messages list the children it pauses and resumes, so
	// only messages that do not mention root count.
	texts := sim.messageTexts()
	for i := 1; i <= children; i++ {
		named := regexp.MustCompile(fmt.Sprintf(`child-%d\b`, i))
		found := false
		for _, text := range texts {
			if !strings.Contains(text, "root") && named.MatchString(text) {
				found = true
			}
		}
		if !found {
			t.Errorf("SILENT: child-%d is still down after root recovered and no message names it", i)
		}
	}
	if after := len(bodies) - before; after > burstThreshold {
		t.Errorf("STORM: %d parent message(s) for the resumed children; want at most %d "+
			"(burstThreshold-1 individual pages + 1 digest)", after, burstThreshold)
	}

	t.Log("--- children UP ---")
	sim.setLeavesDown(false)
	time.Sleep(20 * time.Second)
	sim.dump(t, "after recovery")
	if down, edited := sim.downParents(), sim.editedParents(); edited < down {
		t.Errorf("ORPHANED: %d DOWN parent(s) posted but only %d edited to resolved", down, edited)
	}
}

// nestedTreeConfig is the root ← mid ← leaf-N tree the nested-dependsOn
// scenarios share. root probes every second so it is known down within
// a leaf's first failing ticks; every other interval is several times
// pendingWait.
func nestedTreeConfig(leaves int) simConfig {
	return simConfig{
		leaves:           leaves,
		interval:         "8s",
		rootInterval:     "1s",
		pendingWait:      "2s",
		burstThreshold:   5,
		burstWindow:      "16s",
		reminderInterval: "1h",
	}
}

// nestedTally sorts the top-level messages of a nested-tree run by the
// monitor their title names.
type nestedTally struct {
	rootDown, mid, lateNotice int
	leafDown                  map[string]int
	other                     []string
}

func (s *outageSim) nestedTally(leaves int) nestedTally {
	tally := nestedTally{leafDown: map[string]int{}}
	for _, body := range s.parentBodies() {
		if strings.Contains(body, "Initial notification delivery failed") {
			tally.lateNotice++
		}
		switch {
		case strings.Contains(body, "*root is DOWN*"):
			tally.rootDown++
			continue
		case strings.Contains(body, "*mid is"):
			tally.mid++
			continue
		}
		matched := false
		for i := 1; i <= leaves; i++ {
			leaf := fmt.Sprintf("leaf-%d", i)
			if strings.Contains(body, "*"+leaf+" is DOWN*") {
				tally.leafDown[leaf]++
				matched = true
			}
		}
		if !matched {
			tally.other = append(tally.other, body)
		}
	}
	return tally
}

// TestOutageSimulation_nestedTreeAnnouncesOnlyTheRoot: root ← mid ←
// leaf-N, with mid and every leaf on a channel of its own, and the whole
// tree goes down while Slack keeps working. root's outage pauses mid,
// and a leaf behind a paused mid is as unreachable as one behind a down
// parent: only root is announced, mid and the leaves stay silent, and
// root's DOWN is edited to UP when the tree recovers.
func TestOutageSimulation_nestedTreeAnnouncesOnlyTheRoot(t *testing.T) {
	const leaves = 6
	sim := newOutageSim(t)
	stop := sim.run(t, sim.yaml(t, nestedTreeConfig(leaves)))
	defer stop()

	time.Sleep(10 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	t.Log("--- root + mid + leaves DOWN (Slack up) ---")
	sim.setTargetsDown(true)
	time.Sleep(25 * time.Second)
	sim.dump(t, "during outage")

	t.Log("--- root + mid + leaves UP ---")
	sim.setTargetsDown(false)
	time.Sleep(25 * time.Second)
	sim.dump(t, "after recovery")

	tally := sim.nestedTally(leaves)
	if tally.lateNotice != 0 {
		t.Errorf("FALSE BANNER: %d message(s) claim the initial notification failed; Slack was up throughout", tally.lateNotice)
	}
	if tally.rootDown != 1 {
		t.Errorf("channel got %d DOWN message(s) for root; want exactly 1", tally.rootDown)
	}
	if tally.mid != 0 {
		t.Errorf("STORM: %d message(s) about mid; mid is paused behind root", tally.mid)
	}
	if len(tally.leafDown) != 0 || len(tally.other) != 0 {
		t.Errorf("STORM: leaves paged behind a paused mid: %v, other parents: %d", tally.leafDown, len(tally.other))
	}
	if up := sim.editedToUp(); up != 1 {
		t.Errorf("root's DOWN parent edited to UP %d time(s); want 1", up)
	}
}

// TestOutageSimulation_nestedLeavesStillDownWhenRootRecovers: the
// nested tree goes down together, then root and mid recover while every
// leaf keeps failing. Nothing about the leaves is posted while mid is
// held behind root. Once mid is back up, each leaf's first resumed tick
// announces it through the dispatcher, once, on its own channel, and its
// recovery edits that message.
func TestOutageSimulation_nestedLeavesStillDownWhenRootRecovers(t *testing.T) {
	const leaves = 4
	sim := newOutageSim(t)
	stop := sim.run(t, sim.yaml(t, nestedTreeConfig(leaves)))
	defer stop()

	time.Sleep(10 * time.Second)
	if n := sim.parentPosts(); n != 0 {
		sim.dump(t, "healthy")
		t.Fatalf("healthy phase already posted %d parent message(s)", n)
	}

	t.Log("--- root + mid + leaves DOWN (Slack up) ---")
	sim.setTargetsDown(true)
	time.Sleep(25 * time.Second)
	sim.dump(t, "during outage")
	if tally := sim.nestedTally(leaves); tally.rootDown != 1 || tally.mid != 0 || len(tally.leafDown) != 0 || len(tally.other) != 0 {
		t.Errorf("during outage: root DOWN %d (want 1), mid %d, leaves %v, other %d (want none)",
			tally.rootDown, tally.mid, tally.leafDown, len(tally.other))
	}

	// mid resumes on its next tick after root's recovery; each leaf then
	// resumes still failing on its own next tick.
	t.Log("--- root + mid UP, leaves still DOWN ---")
	sim.setRootDown(false)
	time.Sleep(30 * time.Second)
	sim.dump(t, "root recovered")

	tally := sim.nestedTally(leaves)
	if tally.lateNotice != 0 {
		t.Errorf("FALSE BANNER: %d message(s) claim the initial notification failed; Slack was up throughout", tally.lateNotice)
	}
	if tally.mid != 0 {
		t.Errorf("%d message(s) about mid; mid never failed on its own", tally.mid)
	}
	for i := 1; i <= leaves; i++ {
		leaf := fmt.Sprintf("leaf-%d", i)
		if n := tally.leafDown[leaf]; n != 1 {
			t.Errorf("%s announced %d time(s) after mid recovered; want exactly 1", leaf, n)
		}
	}
	if len(tally.other) != 0 {
		t.Errorf("unexpected parent message(s): %v", tally.other)
	}
	if up := sim.editedToUp(); up != 1 {
		t.Errorf("root's DOWN parent edited to UP %d time(s); want 1", up)
	}

	t.Log("--- leaves UP ---")
	sim.setLeavesDown(false)
	time.Sleep(20 * time.Second)
	sim.dump(t, "after recovery")
	if down, edited := sim.downParents(), sim.editedParents(); edited < down {
		t.Errorf("ORPHANED: %d DOWN parent(s) posted but only %d edited to resolved", down, edited)
	}
}
