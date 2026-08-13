package config_test

import (
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// ADR-0009 — `*From` value sources. A rule's config: block may declare
// that a field takes its value from an Ingress or Namespace annotation
// instead of a literal.

func TestLoad_kube_pathFromParsesIngressAnnotationScope(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        pathFrom:
          annotation: app.example.test/health-check
`
	cfg, err := config.Load(withKubeBlock(tree))
	if err != nil {
		t.Fatalf("pathFrom should parse, got: %v", err)
	}
	rule := cfg.Kube.Match[1]
	if !rule.Config.IsSet("pathFrom") {
		t.Fatal("pathFrom should be recorded as set")
	}
	src := rule.Config.PathFrom
	if src == nil {
		t.Fatal("PathFrom should be populated")
	}
	if src.Annotation != "app.example.test/health-check" {
		t.Errorf("annotation = %q, want app.example.test/health-check", src.Annotation)
	}
	if src.NamespaceAnnotation != "" {
		t.Errorf("namespaceAnnotation should be empty, got %q", src.NamespaceAnnotation)
	}
	if src.HasDefault {
		t.Error("no default was declared, HasDefault should be false")
	}
}

// A block that sets both the literal and its *From twin is ambiguous:
// nothing in the merge order says which one the layer contributes.
func TestLoad_kube_rejectsLiteralAndValueSourceInSameBlock(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        path: /healthz
        pathFrom:
          annotation: app.example.test/health-check
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected path + pathFrom in one block to be rejected")
	}
	if !strings.Contains(err.Error(), "kube.match[1].config.pathFrom") {
		t.Errorf("error should point at the pathFrom key, got: %v", err)
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("error should name the conflicting literal, got: %v", err)
	}
}

func TestLoad_kube_rejectsValueSourceWithoutScope(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        pathFrom:
          default: /healthz
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected a *From block with no annotation scope to be rejected")
	}
	if !strings.Contains(err.Error(), "exactly one of annotation: or namespaceAnnotation:") {
		t.Errorf("error should name both scope keys, got: %v", err)
	}
}

func TestLoad_kube_rejectsValueSourceWithBothScopes(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        pathFrom:
          annotation: app.example.test/health-check
          namespaceAnnotation: app.example.test/health-check
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected a *From block with both scopes to be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should explain the exclusivity, got: %v", err)
	}
}

func TestLoad_kube_rejectsInvalidAnnotationKey(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        pathFrom:
          annotation: "not a valid key!"
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected an invalid annotation key to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid k8s annotation key") {
		t.Errorf("error should name the key syntax problem, got: %v", err)
	}
}

func TestLoad_kube_rejectsNotifyFromPairedWithNotifyOverrideFrom(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        notifyFrom:
          namespaceAnnotation: app.example.test/notify
        notifyOverrideFrom:
          namespaceAnnotation: app.example.test/notify
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected notifyFrom + notifyOverrideFrom in one block to be rejected")
	}
	if !strings.Contains(err.Error(), "notifyOverrideFrom") {
		t.Errorf("error should point at notifyOverrideFrom, got: %v", err)
	}
}

func TestLoad_kube_rejectsUnknownChannelInSlackFromDefault(t *testing.T) {
	tree := kubeRootBaseline + `    - when: { namespace: "acme-*" }
      config:
        slackFrom:
          namespaceAnnotation: app.example.test/slack
          default: no-such-channel
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected an unknown channel slug in a default to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown channel slug") {
		t.Errorf("error should name the unknown slug, got: %v", err)
	}
}

// ADR-0009: "pathFrom carrying a default satisfies ADR-0002's
// root-required path constraint." Without a default the root would
// have nothing to hand descendants when the annotation is absent.
func TestLoad_kube_rootPathFromWithDefaultSatisfiesRequiredAtRoot(t *testing.T) {
	tree := `    - when: {}
      config:
        pathFrom:
          annotation: app.example.test/health-check
          default: /
        httpMethod: GET
        acceptedStatusCodes: [200]
        interval: 5m
        timeout: 10s
        retries: 2
        retryBackoff: 5s
        followRedirects: false
        reminderInterval: 3d
        sslAlertThreshold: 30d
        sslEscalationThreshold: 7d
        sslReminderInterval: 3d
        slack: ops-alerts
`
	if _, err := config.Load(withKubeBlock(tree)); err != nil {
		t.Fatalf("pathFrom with a default should satisfy required-at-root path, got: %v", err)
	}
}

func TestLoad_kube_rootPathFromWithoutDefaultFailsRequiredAtRoot(t *testing.T) {
	tree := `    - when: {}
      config:
        pathFrom:
          annotation: app.example.test/health-check
        httpMethod: GET
        acceptedStatusCodes: [200]
        interval: 5m
        timeout: 10s
        retries: 2
        retryBackoff: 5s
        followRedirects: false
        reminderInterval: 3d
        sslAlertThreshold: 30d
        sslEscalationThreshold: 7d
        sslReminderInterval: 3d
        slack: ops-alerts
`
	_, err := config.Load(withKubeBlock(tree))
	if err == nil {
		t.Fatal("expected a defaultless root pathFrom to fail required-at-root")
	}
	if !strings.Contains(err.Error(), "kube.match[0].config.path") {
		t.Errorf("error should point at the unsatisfied path requirement, got: %v", err)
	}
}
