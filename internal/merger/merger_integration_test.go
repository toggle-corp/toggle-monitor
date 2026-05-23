//go:build integration

package merger_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

func ann(s ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(s); i += 2 {
		out[s[i]] = s[i+1]
	}
	return out
}

func ingress(ns, name string, annotations map[string]string, hosts ...string) *networkingv1.Ingress {
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, networkingv1.IngressRule{Host: h})
	}
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
}

const domain = "monitor.example.com"

func presetCfg() *config.Kube {
	return &config.Kube{
		AnnotationDomain: domain,
		ResyncInterval:   config.Duration(time.Minute),
		Presets: []config.KubePreset{
			{Slug: "internal-api", Scheme: "https", Path: "/health"},
		},
	}
}

// withKube constructs a minimal config.Config whose only meaningful
// content is the kube block + static monitors, mirroring what
// merger.New cares about.
func withKube(kc *config.Kube, statics []config.Monitor) config.Config {
	return config.Config{Kube: kc, Monitors: statics}
}

func TestMaterializer_addedRowForHappyPath(t *testing.T) {
	repo := newRepo(t)
	m := merger.New(repo, withKube(presetCfg(), nil), nil)
	ing := ingress("default", "api", ann(domain+"/kube.preset", "internal-api"), "api.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Errorf("status: got %q, want 'added'", row.Status)
	}
	if row.MonitorSlug == nil || *row.MonitorSlug != "kube-default-api-api-example-com" {
		t.Errorf("monitor slug: got %v", row.MonitorSlug)
	}
	// Monitor reconciled.
	mrow, err := repo.GetMonitor(context.Background(), *row.MonitorSlug)
	if err != nil {
		t.Fatalf("monitor lookup: %v", err)
	}
	if mrow.URL != "https://api.example.com/health" {
		t.Errorf("URL: got %q", mrow.URL)
	}
	if mrow.Source != store.SourceKube {
		t.Errorf("source: got %q, want kube", mrow.Source)
	}
}

func TestMaterializer_noPreset_reasonRecorded(t *testing.T) {
	repo := newRepo(t)
	m := merger.New(repo, withKube(presetCfg(), nil), nil)
	ing := ingress("default", "naked", nil, "naked.example.com")

	row, _ := m.Materialize(context.Background(), ing, "naked.example.com")
	if row.Status != "kube-invalid" || row.Reason == nil || *row.Reason != "no preset annotation" {
		t.Errorf("expected kube-invalid:no preset annotation, got %+v", row)
	}
}

func TestMaterializer_unknownPreset_reasonRecorded(t *testing.T) {
	repo := newRepo(t)
	m := merger.New(repo, withKube(presetCfg(), nil), nil)
	ing := ingress("default", "wrong", ann(domain+"/kube.preset", "ghost"), "wrong.example.com")

	row, _ := m.Materialize(context.Background(), ing, "wrong.example.com")
	if row.Status != "kube-invalid" {
		t.Errorf("status: got %q", row.Status)
	}
	if row.Reason == nil || !contains(*row.Reason, "ghost") {
		t.Errorf("reason should mention missing preset, got %v", row.Reason)
	}
}

func TestMaterializer_optOutViaEnabledFalse(t *testing.T) {
	repo := newRepo(t)
	m := merger.New(repo, withKube(presetCfg(), nil), nil)
	ing := ingress("default", "off", ann(
		domain+"/kube.preset", "internal-api",
		domain+"/config.enabled", "false",
	), "off.example.com")
	row, _ := m.Materialize(context.Background(), ing, "off.example.com")
	if row.Status != "kube-invalid" || row.Reason == nil || !contains(*row.Reason, "opt-out") {
		t.Errorf("expected opt-out reason, got %+v", row)
	}
}

func TestMaterializer_staticCollision(t *testing.T) {
	repo := newRepo(t)
	statics := []config.Monitor{{Slug: "kube-default-api-api-example-com"}}
	m := merger.New(repo, withKube(presetCfg(), statics), nil)
	ing := ingress("default", "api", ann(domain+"/kube.preset", "internal-api"), "api.example.com")
	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.Status != "kube-invalid" || row.Reason == nil || !contains(*row.Reason, "conflicts with static") {
		t.Errorf("expected static-collision reason, got %+v", row)
	}
}

func TestMaterializer_pauseMatchesGlob(t *testing.T) {
	repo := newRepo(t)
	kc := presetCfg()
	kc.Pause = []config.KubePause{{Host: "*.staging.example.com", Reason: "maintenance"}}
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("ns", "staging", ann(domain+"/kube.preset", "internal-api"), "api.staging.example.com")

	row, err := m.Materialize(context.Background(), ing, "api.staging.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "kube-paused" {
		t.Errorf("status: got %q, want kube-paused", row.Status)
	}
	if row.Reason == nil || !contains(*row.Reason, "maintenance") {
		t.Errorf("reason should carry the pause reason, got %v", row.Reason)
	}
}

// presetCfgWith returns a kube block carrying two presets so the
// resolution tests can verify which one the materializer picked.
func presetCfgWith() *config.Kube {
	return &config.Kube{
		AnnotationDomain: domain,
		ResyncInterval:   config.Duration(time.Minute),
		Presets: []config.KubePreset{
			{Slug: "internal-api", Scheme: "https", Path: "/health"},
			{Slug: "catchall", Scheme: "https", Path: "/"},
		},
	}
}

func TestMaterializer_wildcardFallback_appliedWhenNoAnnotation(t *testing.T) {
	repo := newRepo(t)
	kc := presetCfgWith()
	kc.Match = []config.KubeMatch{{Preset: "catchall"}} // wildcard
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("default", "naked", nil, "naked.example.com")

	row, err := m.Materialize(context.Background(), ing, "naked.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "added" {
		t.Errorf("status: got %q, want 'added'", row.Status)
	}
	if row.PresetSlug == nil || *row.PresetSlug != "catchall" {
		t.Errorf("preset slug: got %v, want catchall", row.PresetSlug)
	}
	if row.Reason == nil || !contains(*row.Reason, "fallback") {
		t.Errorf("reason should mention fallback, got %v", row.Reason)
	}
}

func TestMaterializer_matchRule_firstMatchWins(t *testing.T) {
	repo := newRepo(t)
	kc := presetCfgWith()
	kc.Match = []config.KubeMatch{
		{When: config.KubeMatchWhen{Namespace: "betaco-core-backend-*"}, Preset: "internal-api"},
		{Preset: "catchall"}, // wildcard fallback
	}
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("betaco-core-backend-1", "api", nil, "core-1.example.com")

	row, _ := m.Materialize(context.Background(), ing, "core-1.example.com")
	if row.Status != "added" {
		t.Errorf("status: got %q", row.Status)
	}
	if row.PresetSlug == nil || *row.PresetSlug != "internal-api" {
		t.Errorf("preset slug: got %v, want internal-api (specific rule should win over wildcard)", row.PresetSlug)
	}
	if row.Reason == nil || !contains(*row.Reason, "match[0]") {
		t.Errorf("reason should call out the match rule, got %v", row.Reason)
	}
}

func TestMaterializer_annotationBeatsMatchAndFallback(t *testing.T) {
	repo := newRepo(t)
	kc := presetCfgWith()
	kc.Match = []config.KubeMatch{
		{When: config.KubeMatchWhen{Namespace: "default"}, Preset: "catchall"},
		{Preset: "catchall"}, // wildcard fallback
	}
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("default", "api", ann(domain+"/kube.preset", "internal-api"), "api.example.com")

	row, _ := m.Materialize(context.Background(), ing, "api.example.com")
	if row.PresetSlug == nil || *row.PresetSlug != "internal-api" {
		t.Errorf("annotation should win, got %v", row.PresetSlug)
	}
	if row.Reason == nil || *row.Reason != "added" {
		t.Errorf("annotation path keeps reason 'added', got %v", row.Reason)
	}
}

func TestMaterializer_matchRule_namespaceAndHostAND(t *testing.T) {
	repo := newRepo(t)
	kc := presetCfgWith()
	kc.Match = []config.KubeMatch{
		{
			When:   config.KubeMatchWhen{Namespace: "acme-*", Host: "*.example.com"},
			Preset: "internal-api",
		},
		{Preset: "catchall"}, // wildcard fallback
	}
	m := merger.New(repo, withKube(kc, nil), nil)

	// Namespace matches, host doesn't → rule skipped, falls through to wildcard.
	ing1 := ingress("acme-api-1", "api", nil, "alpha-1.example.com")
	row1, _ := m.Materialize(context.Background(), ing1, "alpha-1.example.com")
	if row1.PresetSlug == nil || *row1.PresetSlug != "catchall" {
		t.Errorf("ns match + host miss should fall through to wildcard, got %v", row1.PresetSlug)
	}

	// Both match → rule fires.
	ing2 := ingress("acme-api-2", "api", nil, "alpha-2.example.com")
	row2, _ := m.Materialize(context.Background(), ing2, "alpha-2.example.com")
	if row2.PresetSlug == nil || *row2.PresetSlug != "internal-api" {
		t.Errorf("both conditions match → rule should fire, got %v", row2.PresetSlug)
	}
}

func TestMaterializer_matchRule_ignoreSkipsMaterialization(t *testing.T) {
	repo := newRepo(t)
	kc := presetCfgWith()
	kc.Match = []config.KubeMatch{
		{When: config.KubeMatchWhen{Namespace: "test-*"}, Ignore: true},
		{Preset: "catchall"}, // wildcard fallback
	}
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("test-ephemeral", "api", nil, "ephemeral.example.com")

	row, err := m.Materialize(context.Background(), ing, "ephemeral.example.com")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if row.Status != "kube-ignored" {
		t.Errorf("status: got %q, want 'kube-ignored'", row.Status)
	}
	if row.Reason == nil || !contains(*row.Reason, "match[0]") || !contains(*row.Reason, "test-*") {
		t.Errorf("reason should call out the match rule + namespace glob, got %v", row.Reason)
	}
	if row.MonitorSlug != nil {
		t.Errorf("ignored row must not point at a materialized monitor, got %v", row.MonitorSlug)
	}
	// No plan should land in CurrentPlans either — otherwise the
	// scheduler would still spawn a goroutine for an ingress the
	// operator explicitly opted out of.
	if plans := m.CurrentPlans(); len(plans) != 0 {
		t.Errorf("expected zero plans for an ignored ingress, got %d", len(plans))
	}
	// And no monitor row should exist in the DB.
	slug := "kube-test-ephemeral-api-ephemeral-example-com"
	if _, err := repo.GetMonitor(context.Background(), slug); err == nil {
		t.Errorf("expected no monitor row for ignored ingress (slug %q exists)", slug)
	}
}

func TestMaterializer_unknownExplicitPreset_stillFlags(t *testing.T) {
	// Wildcard fallback + match[] do NOT rescue an explicit-but-unknown
	// annotation. Typos should stay visible.
	repo := newRepo(t)
	kc := presetCfgWith()
	kc.Match = []config.KubeMatch{{Preset: "catchall"}} // wildcard fallback
	m := merger.New(repo, withKube(kc, nil), nil)
	ing := ingress("default", "wrong", ann(domain+"/kube.preset", "ghost"), "wrong.example.com")

	row, _ := m.Materialize(context.Background(), ing, "wrong.example.com")
	if row.Status != "kube-invalid" {
		t.Errorf("unknown explicit annotation should stay invalid, got %q", row.Status)
	}
	if row.Reason == nil || !contains(*row.Reason, "ghost") {
		t.Errorf("reason should call out the typo, got %v", row.Reason)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
