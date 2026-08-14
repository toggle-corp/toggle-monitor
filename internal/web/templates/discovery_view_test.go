package templates_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/merger"
	"github.com/toggle-corp/toggle-monitor/internal/store"
	"github.com/toggle-corp/toggle-monitor/internal/web/templates"
)

// ADR-0009 makes provenance mandatory output: without it "why does this
// monitor have these settings?" gains a second, invisible input and
// ADR-0002's debugging complaint returns intact.

const detailRootYAML = `
- when: {}
  config:
    scheme: https
    path: /
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 30s
    retries: 2
    retryBackoff: 5s
    followRedirects: true
    reminderInterval: 3d
    sslAlertThreshold: 14d
    sslEscalationThreshold: 7d
    sslReminderInterval: 1d
    slack: ops-alerts
- when: {namespace: "acme-*"}
  config:
    pathFrom:
      annotation: app.example.test/health-check
    notifyOverrideFrom:
      namespaceAnnotation: app.example.test/notify
`

func detailRules(t *testing.T) []config.KubeMatchRule {
	t.Helper()
	var rules []config.KubeMatchRule
	if err := yaml.Unmarshal([]byte(detailRootYAML), &rules); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return rules
}

func renderDetail(t *testing.T, view templates.DiscoveryDetailView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := templates.DiscoveryDetail(view).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestDiscoveryDetail_showsAnnotationProvenanceAndWarnings(t *testing.T) {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "acme-api-1", Name: "api",
			Annotations: map[string]string{"app.example.test/health-check": "/livez"},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "api.example.test"}},
		},
	}
	env := merger.Env{
		NamespaceAnnotations: map[string]string{"app.example.test/notify": "zed"},
		UserMapping:          map[string]string{"alice": "U1"},
	}

	view := templates.DiscoveryDetailView{
		Row: store.DiscoverySnapshotRow{
			Namespace: "acme-api-1", IngressName: "api", Host: "api.example.test", Status: "added",
		},
	}
	templates.PopulateCascadeView(&view, detailRules(t), ing, "api.example.test", env)

	if view.Outcome != templates.DiscoveryOutcomeMaterialized {
		t.Fatalf("outcome = %q, want materialized", view.Outcome)
	}
	if len(view.Provenance) != 1 {
		t.Fatalf("want one provenance entry for the accepted path, got %+v", view.Provenance)
	}
	if len(view.Warnings) != 1 {
		t.Fatalf("want one warning for the rejected notify entry, got %+v", view.Warnings)
	}

	body := renderDetail(t, view)
	for _, want := range []string{
		"Annotation sources",
		"app.example.test/health-check",
		"/livez",
		"app.example.test/notify",
		"zed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

// ADR-0012 routes the wildcard host through Resolution.Err so the
// daemon, `explain` and this page agree. The page must then say the host
// is unprobeable rather than blaming the resolved config, which is fine.
func TestDiscoveryDetail_wildcardHostIsUnprobeableNotInvalidConfig(t *testing.T) {
	const host = "*.foo.example.test"
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "acme-api-1", Name: "api"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
	view := templates.DiscoveryDetailView{
		Row: store.DiscoverySnapshotRow{
			Namespace: "acme-api-1", IngressName: "api", Host: host, Status: "kube-invalid",
		},
	}
	templates.PopulateCascadeView(&view, detailRules(t), ing, host, merger.Env{})

	if view.Outcome != templates.DiscoveryOutcomeInvalid {
		t.Fatalf("outcome = %q, want invalid", view.Outcome)
	}
	if !view.WildcardHost {
		t.Error("WildcardHost should be set for a wildcard host")
	}
	if !strings.Contains(view.InvalidError, "wildcard") {
		t.Errorf("InvalidError = %q, want it to name the wildcard", view.InvalidError)
	}

	body := renderDetail(t, view)
	if !strings.Contains(body, "Host is not probeable") {
		t.Errorf("banner should say the host is not probeable; first 600:\n%s", body[:min(len(body), 600)])
	}
	if strings.Contains(body, "Resolved config is invalid") {
		t.Error("wildcard host must not be reported as an invalid resolved config")
	}
}

// A cascade built only from literals must not grow an empty panel.
func TestDiscoveryDetail_noAnnotationPanelWithoutValueSources(t *testing.T) {
	var rules []config.KubeMatchRule
	if err := yaml.Unmarshal([]byte(detailRootYAML[:strings.Index(detailRootYAML, `- when: {namespace: "acme-*"}`)]), &rules); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "acme-api-1", Name: "api"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "api.example.test"}},
		},
	}
	view := templates.DiscoveryDetailView{
		Row: store.DiscoverySnapshotRow{
			Namespace: "acme-api-1", IngressName: "api", Host: "api.example.test", Status: "added",
		},
	}
	templates.PopulateCascadeView(&view, rules, ing, "api.example.test", merger.Env{})

	if body := renderDetail(t, view); strings.Contains(body, "Annotation sources") {
		t.Error("literals-only cascade should not render the annotation panel")
	}
}
