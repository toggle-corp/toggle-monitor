package config_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// ADR-0013 — `*From` value sources for alertmanager routing. An
// alertmanager.match rule's config: block may source slack / notify
// from a Namespace annotation, keyed off the alert's namespace label.

// withKubeAndAlertmanager composes a config carrying both blocks, plus a
// userMapping so notify defaults have a roster to resolve against. The
// namespaceAnnotation: scope reads through the kube watcher's Namespace
// informer, so a config using it must configure kube: too.
func withKubeAndAlertmanager(amBlock string) []byte {
	return []byte(withUserMapping(validMinimal) + canonicalKubeTree + amBlock)
}

// withUserMapping injects a one-entry userMapping into a config fixture.
func withUserMapping(cfg string) string {
	return strings.Replace(cfg,
		"      tokenEnv: SLACK_BOT_TOKEN\n",
		"      tokenEnv: SLACK_BOT_TOKEN\n  userMapping:\n    alice: U01ABCDEF12\n",
		1)
}

const amSlackFromBlock = `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slackFrom:
          namespaceAnnotation: app.example.test/slack
          default: ops-alerts
`

func TestLoad_alertmanager_slackFromParsesNamespaceScope(t *testing.T) {
	cfg, err := config.Load(withKubeAndAlertmanager(amSlackFromBlock))
	if err != nil {
		t.Fatalf("slackFrom should parse, got: %v", err)
	}
	src := cfg.Alertmanager.Match[0].Config.SlackFrom
	if src == nil {
		t.Fatal("SlackFrom should be populated")
	}
	if src.NamespaceAnnotation != "app.example.test/slack" {
		t.Errorf("namespaceAnnotation = %q, want app.example.test/slack", src.NamespaceAnnotation)
	}
	if !src.HasDefault || src.DefaultScalar != "ops-alerts" {
		t.Errorf("default: got HasDefault=%v scalar=%q, want true/ops-alerts", src.HasDefault, src.DefaultScalar)
	}
	if got := cfg.Alertmanager.Match[0].Config.ValueSources(); len(got) != 1 || got[0].Key != "slackFrom" {
		t.Errorf("ValueSources: got %+v, want one slackFrom entry", got)
	}
}

// An alert's own annotations are written by whoever authored the
// PrometheusRule, not by the workload's owner, so they are not a
// routing source.
func TestLoad_alertmanager_rejectsIngressAnnotationScope(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when: { labels: { namespace: "acme-*" } }
      config:
        slackFrom:
          annotation: app.example.test/slack
          namespaceAnnotation: app.example.test/slack
`))
	if err == nil {
		t.Fatal("expected annotation: to be rejected under alertmanager.match")
	}
	// Assert the scope-specific message, not just the substring
	// "namespaceAnnotation" — the missing-scope error contains that too.
	if !strings.Contains(err.Error(), "annotation: is not accepted here") {
		t.Errorf("error should reject the ingress scope by name, got: %v", err)
	}
}

// The Namespace informer belongs to the kube watcher, which only exists
// when kube: is configured.
func TestLoad_alertmanager_rejectsNamespaceScopeWithoutKubeBlock(t *testing.T) {
	_, err := config.Load([]byte(withUserMapping(validMinimal) + amSlackFromBlock))
	if err == nil {
		t.Fatal("expected namespaceAnnotation: without kube: to be rejected")
	}
	if !strings.Contains(err.Error(), "kube") {
		t.Errorf("error should explain the kube: dependency, got: %v", err)
	}
}

// ADR-0005 requires config.slack at the root. A slackFrom that carries
// no default leaves an unannotated namespace with no channel at all.
func TestLoad_alertmanager_rejectsRootSlackFromWithoutDefault(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slackFrom:
          namespaceAnnotation: app.example.test/slack
`))
	if err == nil {
		t.Fatal("expected a root slackFrom with no default: to be rejected")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("error should point at the missing default:, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsLiteralAndValueSourceInSameBlock(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when: { labels: { namespace: "acme-*" } }
      config:
        notify: [alice]
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
`))
	if err == nil {
		t.Fatal("expected notify + notifyFrom in one block to be rejected")
	}
	if !strings.Contains(err.Error(), "alertmanager.match[1].config.notifyFrom") {
		t.Errorf("error should point at the notifyFrom key, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsNotifyFromAndOverrideTwinTogether(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
    - when: { labels: { namespace: "acme-*" } }
      config:
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
        notifyOverrideFrom:
          namespaceAnnotation: app.example.test/notify
`))
	if err == nil {
		t.Fatal("expected notifyFrom + notifyOverrideFrom to be rejected")
	}
	if !strings.Contains(err.Error(), "notifyOverrideFrom") {
		t.Errorf("error should name the twin, got: %v", err)
	}
}

// Defaults live in reviewed config, so they are held to the same
// standard as the literal field.
func TestLoad_alertmanager_rejectsUnknownChannelInDefault(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slackFrom:
          namespaceAnnotation: app.example.test/slack
          default: no-such-channel
`))
	if err == nil {
		t.Fatal("expected an unknown channel slug in default: to be rejected")
	}
	if !strings.Contains(err.Error(), "no-such-channel") {
		t.Errorf("error should name the unknown slug, got: %v", err)
	}
}

func TestLoad_alertmanager_rejectsUnknownNotifySlugInDefault(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
          default: [nosuchperson]
`))
	if err == nil {
		t.Fatal("expected an unknown userMapping slug in default: to be rejected")
	}
	if !strings.Contains(err.Error(), "nosuchperson") {
		t.Errorf("error should name the unknown slug, got: %v", err)
	}
}

func TestLoad_alertmanager_acceptsNamespaceLabelOverride(t *testing.T) {
	cfg, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
          namespaceLabel: exported_namespace
`))
	if err != nil {
		t.Fatalf("namespaceLabel: should be accepted, got: %v", err)
	}
	if got := cfg.Alertmanager.Match[0].Config.NotifyFrom.NamespaceLabel; got != "exported_namespace" {
		t.Errorf("namespaceLabel = %q, want exported_namespace", got)
	}
}

func TestLoad_alertmanager_rejectsInvalidNamespaceLabel(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
          namespaceLabel: "not a label"
`))
	if err == nil {
		t.Fatal("expected an invalid label name to be rejected")
	}
	if !strings.Contains(err.Error(), "namespaceLabel") {
		t.Errorf("error should point at namespaceLabel, got: %v", err)
	}
}

// namespaceLabel: has no meaning under kube.match — the namespace comes
// from the Ingress being materialized, not from a label.
func TestLoad_kube_rejectsNamespaceLabelOnValueSource(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        pathFrom:
          annotation: app.example.test/health-check
          namespaceLabel: namespace
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected namespaceLabel: under kube.match to be rejected")
	}
	if !strings.Contains(err.Error(), "namespaceLabel") {
		t.Errorf("error should point at namespaceLabel, got: %v", err)
	}
}

// An annotation may never supply raw `<…>` markup, so with no
// userMapping a notify source can provably never contribute a value.
func TestLoad_alertmanager_rejectsNotifyFromWithoutUserMapping(t *testing.T) {
	_, err := config.Load([]byte(validMinimal + canonicalKubeTree + `
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
`))
	if err == nil {
		t.Fatal("expected notifyFrom with no slack.userMapping to be rejected")
	}
	if !strings.Contains(err.Error(), "userMapping") {
		t.Errorf("error should name the empty userMapping, got: %v", err)
	}
}

// namespaceLabel names a Prometheus label, not a k8s object key.
func TestLoad_alertmanager_namespaceLabelUsesPrometheusNaming(t *testing.T) {
	load := func(label string) error {
		_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slack: ops-alerts
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
          namespaceLabel: ` + label + `
`))
		return err
	}
	// Legal Prometheus label names.
	for _, ok := range []string{"namespace", "exported_namespace", "_namespace", "ns0"} {
		if err := load(ok); err != nil {
			t.Errorf("namespaceLabel %q should be accepted, got: %v", ok, err)
		}
	}
	// Illegal as a Prometheus label name, whatever k8s thinks of them.
	for _, bad := range []string{`"app.example.test/ns"`, `"exported-namespace"`, `"my.namespace"`, `"0namespace"`} {
		if err := load(bad); err == nil {
			t.Errorf("namespaceLabel %s should be rejected", bad)
		}
	}
}

// A scalar field's default: must be a scalar; a sequence silently reads
// as the empty string otherwise.
func TestLoad_alertmanager_rejectsListDefaultOnScalarSource(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: {}
      config:
        slackFrom:
          namespaceAnnotation: app.example.test/slack
          default: [ops-alerts]
`))
	if err == nil {
		t.Fatal("expected a sequence default: on slackFrom to be rejected")
	}
	if !strings.Contains(err.Error(), "single value") {
		t.Errorf("error should explain that slack takes one value, got: %v", err)
	}
}

// c.errf accumulates, so a root rule that is wrong in two ways should
// report both rather than stopping at the selector.
func TestLoad_alertmanager_rootWithSelectorAndNoSlack_reportsBoth(t *testing.T) {
	_, err := config.Load(withKubeAndAlertmanager(`
alertmanager:
  endpoint:
    tokenEnv: ALERTMANAGER_WEBHOOK_TOKEN
  match:
    - when: { alertname: Foo }
`))
	if err == nil {
		t.Fatal("expected a non-empty root when: with no config to be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "must have an empty when:") {
		t.Errorf("error should report the selector, got: %v", err)
	}
	// Not just the substring "config.slack" — case 1's own message
	// mentions it in passing, so assert the distinct requirement.
	if !strings.Contains(msg, "required at the root rule") {
		t.Errorf("error should also report the missing root slack, got: %v", err)
	}
}
