package merger

import (
	"strings"
	"testing"
)

// ADR-0014 — a rule may select on Ingress or Namespace annotations, so
// an app team can opt its own object out of monitoring using the same
// channel it already uses for every other monitoring hint.
//
// These drive the public Resolve kernel rather than whenMatches, so
// they assert the operator-visible outcome (skipped / not skipped, and
// the rule chain that says why).

// skipFixtureYAML pairs each annotation scope with `ignore: true`, the
// shape an operator writes to make a key mean "skip".
const skipFixtureYAML = `
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
- when:
    annotations:
      monitor.example.test/skip: "true"
  ignore: true
  final: true
- when:
    namespaceAnnotations:
      monitor.example.test/skip: "true"
  ignore: true
  final: true
`

func TestResolve_ingressAnnotationSkipsHost(t *testing.T) {
	rules := parseRules(t, skipFixtureYAML)
	in := annotatedIng("acme", "api",
		map[string]string{"monitor.example.test/skip": "true"}, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{})

	if !res.Ignored {
		t.Fatalf("Ignored should be true; chain: %v", res.Chain)
	}
}

func TestResolve_namespaceAnnotationSkipsHost(t *testing.T) {
	rules := parseRules(t, skipFixtureYAML)
	in := annotatedIng("acme", "api", nil, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{
		NamespaceAnnotations: map[string]string{"monitor.example.test/skip": "true"},
	})

	if !res.Ignored {
		t.Fatalf("Ignored should be true; chain: %v", res.Chain)
	}
}

// The two scopes are distinct objects: an Ingress annotation must not
// satisfy a namespaceAnnotations selector, or a single app team could
// skip everything its namespace contains.
func TestResolve_ingressAnnotationDoesNotSatisfyNamespaceScope(t *testing.T) {
	rules := parseRules(t, `
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
- when:
    namespaceAnnotations:
      monitor.example.test/skip: "true"
  ignore: true
`)
	in := annotatedIng("acme", "api",
		map[string]string{"monitor.example.test/skip": "true"}, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{})

	if res.Ignored {
		t.Error("an Ingress annotation must not match a namespaceAnnotations selector")
	}
}

func TestResolve_annotationAbsentLeavesHostMonitored(t *testing.T) {
	rules := parseRules(t, skipFixtureYAML)
	in := annotatedIng("acme", "api", nil, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{})

	if res.Ignored {
		t.Error("no skip annotation on either object, so the host stays monitored")
	}
}

// Matching is on the pair, not the key: the operator's rule names the
// value that means skip, so any other value leaves the host monitored.
func TestResolve_annotationValueMustMatch(t *testing.T) {
	rules := parseRules(t, skipFixtureYAML)
	in := annotatedIng("acme", "api",
		map[string]string{"monitor.example.test/skip": "false"}, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{})

	if res.Ignored {
		t.Error(`skip: "false" must not match a rule selecting skip: "true"`)
	}
}

// All pairs in one selector AND together, as for labels.
func TestResolve_everyAnnotationPairMustMatch(t *testing.T) {
	rules := parseRules(t, `
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
- when:
    annotations:
      monitor.example.test/skip: "true"
      monitor.example.test/reason: decommissioned
  ignore: true
`)
	in := annotatedIng("acme", "api",
		map[string]string{"monitor.example.test/skip": "true"}, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{})

	if res.Ignored {
		t.Error("one of two required pairs matched; the rule must not fire")
	}
}

// The chain is what /discovery and discovery_snapshot.reason show, so a
// host that vanished has to name the annotation that removed it.
func TestResolve_ruleChainNamesTheMatchedAnnotation(t *testing.T) {
	rules := parseRules(t, skipFixtureYAML)
	in := annotatedIng("acme", "api",
		map[string]string{"monitor.example.test/skip": "true"}, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{})

	want := "annotations.monitor.example.test/skip=true"
	if !containsSubstring(res.Chain, want) {
		t.Errorf("RuleChain %v should name %q", res.Chain, want)
	}
}

func TestResolve_ruleChainNamesTheMatchedNamespaceAnnotation(t *testing.T) {
	rules := parseRules(t, skipFixtureYAML)
	in := annotatedIng("acme", "api", nil, "api.example.test")

	res := Resolve(rules, in, "api.example.test", Env{
		NamespaceAnnotations: map[string]string{"monitor.example.test/skip": "true"},
	})

	want := "namespaceAnnotations.monitor.example.test/skip=true"
	if !containsSubstring(res.Chain, want) {
		t.Errorf("RuleChain %v should name %q", res.Chain, want)
	}
}

// containsSubstring reports whether any chain entry mentions want.
func containsSubstring(chain []string, want string) bool {
	for _, c := range chain {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}
