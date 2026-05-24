//go:build integration

package merger_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/testpg"
)

func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	dsn := testpg.Start(t)
	if err := migrate.Up(dsn); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// ingress builds a minimal *networkingv1.Ingress for a single (ns,
// name, hosts) fixture. labels populates ObjectMeta.Labels (consumed
// by KubeMatchWhen.Labels selectors). hosts seed spec.rules[*].host.
func ingress(ns, name string, labels map[string]string, hosts ...string) *networkingv1.Ingress {
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, networkingv1.IngressRule{Host: h})
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
}

// rootMatchYAML is the canonical root-rule YAML that every test
// fixture extends — every required-at-root field is set so the
// validator passes. Tests inject extra rules via fixtureKube().
const rootMatchYAML = `
- when: {}
  config:
    scheme: https
    path: /health
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 30s
    timeout: 5s
    retries: 0
    retryBackoff: 1s
    followRedirects: false
    reminderInterval: 1h
    sslAlertThreshold: 30d
    sslEscalationThreshold: 7d
    sslReminderInterval: 1h
    slack: ops-alerts
`

// fixtureKube parses a YAML fragment as []KubeMatchRule, prepending
// the canonical root if `withRoot` is true. The fragment is YAML
// matching kube.match[]'s shape so the test reads close to the
// real config a user would write.
func fixtureKube(t *testing.T, matchYAML string, withRoot bool) *config.Kube {
	t.Helper()
	full := matchYAML
	if withRoot {
		full = rootMatchYAML + matchYAML
	}
	var rules []config.KubeMatchRule
	if err := yaml.Unmarshal([]byte(full), &rules); err != nil {
		t.Fatalf("parse match yaml: %v\nyaml was:\n%s", err, full)
	}
	return &config.Kube{
		ResyncInterval: config.Duration(time.Minute),
		Match:          rules,
	}
}

// withKube constructs a minimal config.Config whose only meaningful
// content is the kube block + static monitors. Slack.UserMapping is
// pre-seeded with a couple of fixtures so notify-list tests can
// resolve mentions.
func withKube(kc *config.Kube, statics []config.Monitor) config.Config {
	return config.Config{
		Kube:     kc,
		Monitors: statics,
		Slack: config.Slack{
			UserMapping: map[string]string{
				"thenav56": "U111111111",
				"barsha":   "U222222222",
				"carol":    "U333333333",
				"dave":     "U444444444",
			},
		},
	}
}

func TestMaterializer_rootRuleAlwaysMaterializes(t *testing.T) {
	repo := newRepo(t)
	m := merger.New(repo, withKube(fixtureKube(t, ``, true), nil), nil)
	ing := ingress("default", "api", nil, "api.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Errorf("status: got %q, want 'added'", row.Status)
	}
	if want := "default__api__api-example-com"; row.MonitorSlug == nil || *row.MonitorSlug != want {
		t.Errorf("monitor slug: got %v, want %q", row.MonitorSlug, want)
	}
	mrow, err := repo.GetMonitor(context.Background(), *row.MonitorSlug)
	if err != nil {
		t.Fatalf("monitor lookup: %v", err)
	}
	if mrow.URL != "https://api.example.com/health" {
		t.Errorf("URL: got %q, want https://api.example.com/health", mrow.URL)
	}
	if mrow.Source != store.SourceKube {
		t.Errorf("source: got %q, want kube", mrow.Source)
	}
	// reason carries the rule-chain summary so /discovery can render it.
	if row.Reason == nil || !strings.Contains(*row.Reason, "match[0]") {
		t.Errorf("reason should include match[0] in the rule chain, got %v", row.Reason)
	}
}

func TestMaterializer_multiMatchAccumulate(t *testing.T) {
	// Root sets path /health; namespace rule overrides to /v2; nested
	// label rule overrides to /minio/health/live. Deepest matching
	// scalar wins.
	repo := newRepo(t)
	extra := `
- when: {namespace: "acme-*"}
  config:
    path: /v2
  nested:
    - when: {labels: {"app.kubernetes.io/name": "minio"}}
      config:
        path: /minio/health/live
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("acme-api-1", "minio", map[string]string{"app.kubernetes.io/name": "minio"}, "minio.example.com")

	row, err := m.Materialize(context.Background(), ing, "minio.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Fatalf("status: got %q (reason=%v), want 'added'", row.Status, row.Reason)
	}
	mrow, _ := repo.GetMonitor(context.Background(), *row.MonitorSlug)
	if want := "https://minio.example.com/minio/health/live"; mrow.URL != want {
		t.Errorf("URL: got %q, want %q (deepest path should win)", mrow.URL, want)
	}
	// Rule chain should mention both layers.
	if row.Reason == nil || !strings.Contains(*row.Reason, "match[1]") || !strings.Contains(*row.Reason, "nested[0]") {
		t.Errorf("reason should chain match[1] → nested[0], got %v", row.Reason)
	}
}

func TestMaterializer_arrayUnionAndOverride(t *testing.T) {
	repo := newRepo(t)
	extra := `
- when: {namespace: "acme-*"}
  config:
    tags: [region-asia]
    notify: [thenav56]
  nested:
    - when: {host: "*.example.com"}
      config:
        tags: [tier-1]                       # unions with region-asia
        notify: !override [barsha]           # replaces ancestor thenav56
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("acme-api-1", "api", nil, "api.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	mrow, _ := repo.GetMonitor(context.Background(), *row.MonitorSlug)
	wantTags := map[string]bool{"region-asia": false, "tier-1": false}
	for _, g := range mrow.Tags {
		if _, ok := wantTags[g]; !ok {
			t.Errorf("unexpected tag %q", g)
		}
		wantTags[g] = true
	}
	for tag, seen := range wantTags {
		if !seen {
			t.Errorf("missing tag %q in resolved monitor", tag)
		}
	}
	// CurrentPlans carries the resolved Notify (after !override).
	plans := m.CurrentPlans()
	if len(plans) != 1 {
		t.Fatalf("plans: got %d, want 1", len(plans))
	}
	mentions := plans[0].Mentions
	// thenav56 → U111… should be GONE (overridden); barsha → U222… present.
	for _, mt := range mentions {
		if strings.Contains(mt, "U111111111") {
			t.Errorf("ancestor mention thenav56 should have been overridden, got %v", mentions)
		}
	}
	foundBarsha := false
	for _, mt := range mentions {
		if strings.Contains(mt, "U222222222") {
			foundBarsha = true
		}
	}
	if !foundBarsha {
		t.Errorf("override list should contribute barsha, got %v", mentions)
	}
}

func TestMaterializer_overrideMidCascadeContinuesUnion(t *testing.T) {
	// ADR-0002 §Merge rules: `!override` flips exactly ONE layer to
	// replace mode; deeper layers continue to union on top of the
	// override-result. The 2-layer case is covered by
	// TestMaterializer_arrayUnionAndOverride; this asserts the
	// 3-layer cascade explicitly.
	//
	//   layer 1 (match[1]) : notify=[thenav56, barsha]        → accumulator
	//   layer 2 (nested[0]): notify=!override [carol]         → resets to [carol]
	//   layer 3 (nested[1]): notify=[dave]                    → unions → [carol, dave]
	repo := newRepo(t)
	extra := `
- when: {namespace: "acme-*"}
  config:
    notify: [thenav56, barsha]
  nested:
    - when: {host: "*.example.com"}
      config:
        notify: !override [carol]
      nested:
        - when: {labels: {tier: "edge"}}
          config:
            notify: [dave]
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("acme-api-1", "api", map[string]string{"tier": "edge"}, "api.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Fatalf("status: got %q (reason=%v), want 'added'", row.Status, row.Reason)
	}

	plans := m.CurrentPlans()
	if len(plans) != 1 {
		t.Fatalf("plans: got %d, want 1", len(plans))
	}
	mentions := plans[0].Mentions

	// Ancestor notify list (thenav56, barsha) must have been cleared
	// by !override at layer 2.
	for _, mt := range mentions {
		if strings.Contains(mt, "U111111111") || strings.Contains(mt, "U222222222") {
			t.Errorf("ancestor notify (thenav56/barsha) must have been overridden, got %v", mentions)
		}
	}

	// carol (override layer) AND dave (deeper-than-override layer) must
	// both be present — !override does NOT short-circuit the cascade,
	// it just resets the accumulator at the layer that carries it.
	wantUsers := map[string]bool{"U333333333": false, "U444444444": false}
	for _, mt := range mentions {
		for u := range wantUsers {
			if strings.Contains(mt, u) {
				wantUsers[u] = true
			}
		}
	}
	for u, seen := range wantUsers {
		if !seen {
			t.Errorf("expected mention containing %s; got %v", u, mentions)
		}
	}

	// Order: shallow-first per mergeStrings dedup semantics. carol (the
	// override layer's contribution) should precede dave (the deeper
	// layer's union contribution).
	var carolIdx, daveIdx = -1, -1
	for i, mt := range mentions {
		if strings.Contains(mt, "U333333333") {
			carolIdx = i
		}
		if strings.Contains(mt, "U444444444") {
			daveIdx = i
		}
	}
	if carolIdx == -1 || daveIdx == -1 || carolIdx >= daveIdx {
		t.Errorf("expected carol (override) before dave (deeper union), got %v", mentions)
	}
}

func TestMaterializer_acceptedStatusCodesReplaceByDefault(t *testing.T) {
	repo := newRepo(t)
	extra := `
- when: {namespace: "acme-*"}
  config:
    acceptedStatusCodes: [301]
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("acme-api-1", "api", nil, "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Status != "added" {
		t.Fatalf("status: got %q, want 'added' (reason=%v)", row.Status, row.Reason)
	}
	plans := m.CurrentPlans()
	if len(plans) != 1 {
		t.Fatalf("plans: got %d, want 1", len(plans))
	}
	got := plans[0].AcceptedStatusCodes
	if len(got) != 1 || got[0] != 301 {
		t.Errorf("acceptedStatusCodes should be REPLACED not unioned, got %v want [301]", got)
	}
}

func TestMaterializer_finalHaltsCascade(t *testing.T) {
	repo := newRepo(t)
	// Two top-level rules. The first matches (with final:true) and
	// sets path=/early; the second would otherwise overwrite path to
	// /late. Final must halt traversal AFTER descending the first
	// rule's own nested, so the late rule never contributes.
	extra := `
- when: {namespace: "acme-*"}
  final: true
  config:
    path: /early
  nested:
    - when: {host: "*.example.com"}
      config:
        path: /nested-still-runs
- when: {namespace: "acme-*"}
  config:
    path: /late
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("acme-api-1", "api", nil, "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Status != "added" {
		t.Fatalf("status: %q (reason=%v)", row.Status, row.Reason)
	}
	mrow, _ := repo.GetMonitor(context.Background(), *row.MonitorSlug)
	if want := "https://api.example.com/nested-still-runs"; mrow.URL != want {
		t.Errorf("URL: got %q, want %q (final's own nested still runs; later sibling does NOT)", mrow.URL, want)
	}
	if row.Reason == nil || !strings.Contains(*row.Reason, "[final]") {
		t.Errorf("reason should mark the final rule, got %v", row.Reason)
	}
}

func TestMaterializer_namespaceRegexMatchesAnchored(t *testing.T) {
	// `namespaceRegex` is auto-anchored at use-time (^...$ per ADR-0002),
	// so "acme-\d+" matches "acme-1" but NOT "acme-1-foo".
	// The rule's config: contribution must land on the resolved monitor
	// for the matching namespace.
	extra := `
- when: {namespaceRegex: "acme-\\d+"}
  config:
    path: /regex-matched
    tags: [regex-hit]
`

	t.Run("matches exact pattern", func(t *testing.T) {
		repo := newRepo(t)
		kc := fixtureKube(t, extra, true)
		m := merger.New(repo, withKube(kc, nil), nil)
		ing := ingress("acme-1", "api", nil, "api.example.com")

		row, err := m.Materialize(context.Background(), ing, "api.example.com")
		if err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if row.Status != "added" {
			t.Fatalf("status: got %q (reason=%v), want 'added'", row.Status, row.Reason)
		}
		mrow, _ := repo.GetMonitor(context.Background(), *row.MonitorSlug)
		if want := "https://api.example.com/regex-matched"; mrow.URL != want {
			t.Errorf("URL: got %q, want %q (regex rule should have contributed path)", mrow.URL, want)
		}
		foundTag := false
		for _, tg := range mrow.Tags {
			if tg == "regex-hit" {
				foundTag = true
			}
		}
		if !foundTag {
			t.Errorf("tags: %v missing 'regex-hit' from regex rule", mrow.Tags)
		}
		if row.Reason == nil || !strings.Contains(*row.Reason, "match[1]") {
			t.Errorf("reason should mention the regex rule, got %v", row.Reason)
		}
	})

	t.Run("rejects superset due to anchoring", func(t *testing.T) {
		repo := newRepo(t)
		kc := fixtureKube(t, extra, true)
		m := merger.New(repo, withKube(kc, nil), nil)
		ing := ingress("acme-1-foo", "api", nil, "api.example.com")

		row, err := m.Materialize(context.Background(), ing, "api.example.com")
		if err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if row.Status != "added" {
			t.Fatalf("status: got %q (reason=%v), want 'added'", row.Status, row.Reason)
		}
		mrow, _ := repo.GetMonitor(context.Background(), *row.MonitorSlug)
		// Root path is /health; regex rule must NOT have fired because
		// auto-anchoring rejects "acme-1-foo" against "acme-\d+".
		if mrow.URL != "https://api.example.com/health" {
			t.Errorf("URL: got %q, want root /health (regex must not match superset string)", mrow.URL)
		}
		for _, tg := range mrow.Tags {
			if tg == "regex-hit" {
				t.Errorf("tags: %v should NOT include 'regex-hit' — regex anchoring should reject acme-1-foo", mrow.Tags)
			}
		}
	})
}

func TestMaterializer_finalHaltsLaterUncle(t *testing.T) {
	// A `final: true` rule nested inside an earlier sibling must halt
	// later top-level rules from contributing — halt propagates up to
	// the top of the cascade once a final rule fires, regardless of
	// the depth at which it fires.
	repo := newRepo(t)
	extra := `
- when: {namespace: "a-*"}
  nested:
    - when: {namespace: "a-deep-*"}
      final: true
      config:
        path: /deep
- when: {namespace: "a-*"}
  config:
    path: /should-not-apply
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("a-deep-1", "api", nil, "api.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Fatalf("status: %q (reason=%v)", row.Status, row.Reason)
	}
	mrow, _ := repo.GetMonitor(context.Background(), *row.MonitorSlug)
	if want := "https://api.example.com/deep"; mrow.URL != want {
		t.Errorf("URL: got %q, want %q (deep final must halt the later uncle rule)", mrow.URL, want)
	}
	if row.Reason == nil || !strings.Contains(*row.Reason, "[final]") {
		t.Errorf("reason should mark the final rule, got %v", row.Reason)
	}
	if row.Reason != nil && strings.Contains(*row.Reason, "match[2]") {
		t.Errorf("reason should NOT include match[2] (the uncle should never have contributed), got %v", row.Reason)
	}
}

func TestMaterializer_ignoreTrueProducesIgnoredRow(t *testing.T) {
	repo := newRepo(t)
	extra := `
- when: {namespace: "test-*"}
  ignore: true
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("test-ephemeral", "api", nil, "ephemeral.example.com")

	row, err := m.Materialize(context.Background(), ing, "ephemeral.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "kube-ignored" {
		t.Errorf("status: got %q, want kube-ignored", row.Status)
	}
	if row.Reason == nil || !strings.Contains(*row.Reason, "match[1]") {
		t.Errorf("reason should mention the matching rule, got %v", row.Reason)
	}
	if row.MonitorSlug != nil {
		t.Errorf("ignored row must NOT carry a monitor slug, got %v", row.MonitorSlug)
	}
	if plans := m.CurrentPlans(); len(plans) != 0 {
		t.Errorf("ignored ingress should not produce a plan, got %d", len(plans))
	}
}

func TestMaterializer_ignoreFalseUnignoresAncestor(t *testing.T) {
	repo := newRepo(t)
	extra := `
- when: {namespace: "test-*"}
  ignore: true
  nested:
    - when: {namespace: "test-critical-*"}
      ignore: false
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("test-critical-1", "api", nil, "critical.example.com")

	row, err := m.Materialize(context.Background(), ing, "critical.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Errorf("status: got %q, want 'added' (child ignore:false should override ancestor's true; reason=%v)",
			row.Status, row.Reason)
	}
}

func TestMaterializer_resolvedIntervalLessThanTimeoutIsInvalid(t *testing.T) {
	repo := newRepo(t)
	// Child overrides timeout to 60s while root interval is 30s →
	// interval < timeout after the merge.
	extra := `
- when: {namespace: "broken-*"}
  config:
    timeout: 60s
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("broken-1", "api", nil, "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Status != "kube-invalid" {
		t.Errorf("status: got %q, want kube-invalid (resolved interval<timeout)", row.Status)
	}
	if row.Reason == nil || !strings.Contains(*row.Reason, "interval") {
		t.Errorf("reason should explain the timeout/interval violation, got %v", row.Reason)
	}
}

func TestMaterializer_resolvedSlackOverriddenEmptyIsInvalid(t *testing.T) {
	repo := newRepo(t)
	// Root sets slack=ops-alerts; child overrides to empty string —
	// post-merge resolution must catch this.
	extra := `
- when: {namespace: "broken-*"}
  config:
    slack: ""
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("broken-1", "api", nil, "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Status != "kube-invalid" {
		t.Errorf("status: got %q, want kube-invalid (slack overridden to empty)", row.Status)
	}
	if row.Reason == nil || !strings.Contains(*row.Reason, "slack") {
		t.Errorf("reason should call out the slack field, got %v", row.Reason)
	}
}

func TestMaterializer_staticCollisionIsInvalid(t *testing.T) {
	repo := newRepo(t)
	statics := []config.Monitor{{Slug: "default__api__api-example-com"}}
	m := merger.New(repo, withKube(fixtureKube(t, ``, true), statics), nil)
	ing := ingress("default", "api", nil, "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Status != "kube-invalid" || row.Reason == nil || !strings.Contains(*row.Reason, "conflicts with static") {
		t.Errorf("expected static-collision reason, got %+v", row)
	}
}

func TestMaterializer_slugFormat(t *testing.T) {
	// Pin the ADR-0002 §Identity format: <ns>__<name>__<host>, with
	// per-part sanitization (lowercase, non-alnum→'-', collapse).
	repo := newRepo(t)
	m := merger.New(repo, withKube(fixtureKube(t, ``, true), nil), nil)
	ing := ingress("Prod-NS", "MyApp", nil, "api.dev.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.dev.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if want := "prod-ns__myapp__api-dev-example-com"; row.MonitorSlug == nil || *row.MonitorSlug != want {
		t.Errorf("slug: got %v, want %q", row.MonitorSlug, want)
	}
}

func TestMaterializer_ruleChainShape(t *testing.T) {
	// Two nested levels — confirm the chain renders match[N] →
	// nested[M] with selector summaries.
	repo := newRepo(t)
	extra := `
- when: {namespace: "acme-*"}
  nested:
    - when: {host: "*.example.com"}
`
	kc := fixtureKube(t, extra, true)
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("acme-api-1", "api", nil, "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Reason == nil {
		t.Fatalf("no reason set; row=%+v", row)
	}
	for _, want := range []string{"match[0]", "match[1]", "ns=acme-*", "nested[0]", "host=*.example.com"} {
		if !strings.Contains(*row.Reason, want) {
			t.Errorf("rule chain missing %q; got %q", want, *row.Reason)
		}
	}
}
