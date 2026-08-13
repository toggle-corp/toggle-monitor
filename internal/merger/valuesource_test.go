package merger

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
)

// ADR-0009 — a rule may declare that a field's value comes from an
// Ingress or Namespace annotation. These tests drive the resolution
// through the same public entry point the materializer and `explain`
// use, so they assert merged outcomes rather than lowering mechanics.

// vsRootYAML is the required-at-root baseline every fixture here
// extends. path=/ and slack=ops-alerts are the cascade values a
// rejected annotation must fall back to.
const vsRootYAML = `
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
    notify: [alice]
`

// annotatedIng builds an Ingress carrying annotations, which plain
// ing() does not.
func annotatedIng(ns, name string, annotations map[string]string, host string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        name,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
}

func TestResolve_pathFromIngressAnnotation(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    pathFrom:
      annotation: app.example.test/health-check
`)
	ing := annotatedIng("acme-api-1", "api",
		map[string]string{"app.example.test/health-check": "/livez"}, "api.example.test")

	got := Resolve(rules, ing, "api.example.test", Env{})

	if got.Err != nil {
		t.Fatalf("unexpected resolved-validation error: %v", got.Err)
	}
	if got.Config.Path != "/livez" {
		t.Errorf("path = %q, want /livez (from the ingress annotation)", got.Config.Path)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("a valid annotation should not warn, got: %v", got.Warnings)
	}
	// The operator must be able to answer "why does this monitor have
	// this path?" without reading the cluster.
	var provenance []string
	for _, p := range got.Provenance {
		provenance = append(provenance, p.String())
	}
	joined := strings.Join(provenance, "; ")
	if !strings.Contains(joined, "path=/livez") ||
		!strings.Contains(joined, "annotation app.example.test/health-check") {
		t.Errorf("provenance should name the field, value and annotation key, got: %v", provenance)
	}
}

func TestResolve_absentAnnotationFallsBackToDefault(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    pathFrom:
      annotation: app.example.test/health-check
      default: /healthz
`)
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", Env{})

	if got.Config.Path != "/healthz" {
		t.Errorf("path = %q, want the rule's default /healthz", got.Config.Path)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("an absent annotation is not a misconfiguration, got warnings: %v", got.Warnings)
	}
}

func TestResolve_absentAnnotationWithoutDefaultLeavesCascadeValue(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    pathFrom:
      annotation: app.example.test/health-check
`)
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", Env{})

	if got.Config.Path != "/" {
		t.Errorf("path = %q, want the root's / — a defaultless source contributes nothing", got.Config.Path)
	}
}

// Helm renders an empty list as "", and reading that as an explicit
// value would silently orphan a monitor on a values typo.
func TestResolve_whitespaceOnlyAnnotationIsAbsent(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    pathFrom:
      annotation: app.example.test/health-check
      default: /healthz
`)
	ing := annotatedIng("acme-api-1", "api",
		map[string]string{"app.example.test/health-check": "   "}, "api.example.test")

	got := Resolve(rules, ing, "api.example.test", Env{})

	if got.Config.Path != "/healthz" {
		t.Errorf("path = %q, want the default — a blank annotation is absent, not empty", got.Config.Path)
	}
}

func TestResolve_namespaceAnnotationScopeReadsTheNamespace(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    notifyOverrideFrom:
      namespaceAnnotation: app.example.test/notify
`)
	env := Env{
		NamespaceAnnotations: map[string]string{"app.example.test/notify": "bob,carol"},
		UserMapping:          map[string]string{"alice": "U1", "bob": "U2", "carol": "U3"},
	}
	// The annotation lives on the Namespace, so an identical key on the
	// Ingress must not be what satisfies this source.
	ing := annotatedIng("acme-api-1", "api",
		map[string]string{"app.example.test/notify": "eve"}, "api.example.test")

	got := Resolve(rules, ing, "api.example.test", env)

	want := []string{"bob", "carol"}
	if !equalStrings(got.Config.Notify.Values, want) {
		t.Errorf("notify = %v, want %v from the namespace annotation", got.Config.Notify.Values, want)
	}
}

func TestResolve_notifyFromUnionsAtItsPosition(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    notifyFrom:
      namespaceAnnotation: app.example.test/notify
`)
	env := Env{
		NamespaceAnnotations: map[string]string{"app.example.test/notify": "bob"},
		UserMapping:          map[string]string{"alice": "U1", "bob": "U2"},
	}
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", env)

	want := []string{"alice", "bob"}
	if !equalStrings(got.Config.Notify.Values, want) {
		t.Errorf("notify = %v, want %v — notifyFrom unions like the literal", got.Config.Notify.Values, want)
	}
}

// ADR-0009: override is positional, not final-value-replacing, because
// the tree's trailing host rules own environment tags that a chart
// cannot declare.
func TestResolve_tagsOverrideFromReplacesBaselineButLaterRulesStillUnion(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    tags: [backend]
- when: {namespace: "acme-*"}
  config:
    tagsOverrideFrom:
      namespaceAnnotation: app.example.test/tags
- when: {host: "*.dev.example.test"}
  config:
    tags: [public]
`)
	env := Env{NamespaceAnnotations: map[string]string{"app.example.test/tags": "acme/api"}}
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.dev.example.test"), "api.dev.example.test", env)

	want := []string{"acme/api", "public"}
	if !equalStrings(got.Config.Tags.Values, want) {
		t.Errorf("tags = %v, want %v — the override drops `backend` but the trailing host rule still adds `public`",
			got.Config.Tags.Values, want)
	}
}

func TestResolve_invalidNotifyEntryIsDroppedAndValidOneKept(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    notifyOverrideFrom:
      namespaceAnnotation: app.example.test/notify
`)
	env := Env{
		NamespaceAnnotations: map[string]string{"app.example.test/notify": "bob,zed"},
		UserMapping:          map[string]string{"alice": "U1", "bob": "U2"},
	}
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", env)

	if !equalStrings(got.Config.Notify.Values, []string{"bob"}) {
		t.Errorf("notify = %v, want [bob] — partial validity keeps the good entry", got.Config.Notify.Values)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("want exactly one warning for the rejected entry, got: %v", got.Warnings)
	}
	w := got.Warnings[0]
	if w.Value != "zed" || w.Key != "app.example.test/notify" {
		t.Errorf("warning should name both the key and the rejected value, got: %+v", w)
	}
}

// The roster of who can be paged stays in reviewed config; an
// annotation may only select from it.
func TestResolve_rawSlackMarkupInAnnotationIsRejected(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    notifyOverrideFrom:
      namespaceAnnotation: app.example.test/notify
`)
	env := Env{
		NamespaceAnnotations: map[string]string{"app.example.test/notify": "<!here>"},
		UserMapping:          map[string]string{"alice": "U1"},
	}
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", env)

	if !equalStrings(got.Config.Notify.Values, []string{"alice"}) {
		t.Errorf("notify = %v, want the cascade's [alice] to survive", got.Config.Notify.Values)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0].Reason, "raw Slack markup") {
		t.Errorf("want a raw-markup warning, got: %v", got.Warnings)
	}
}

// An override that yields nothing must be ignored entirely rather than
// replacing real recipients with an empty list.
func TestResolve_overrideYieldingNoValidEntriesIsIgnored(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    notifyOverrideFrom:
      namespaceAnnotation: app.example.test/notify
`)
	env := Env{
		NamespaceAnnotations: map[string]string{"app.example.test/notify": "zed,nobody"},
		UserMapping:          map[string]string{"alice": "U1"},
	}
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", env)

	if !equalStrings(got.Config.Notify.Values, []string{"alice"}) {
		t.Errorf("notify = %v, want the cascade's [alice] — an all-invalid override must not orphan the monitor",
			got.Config.Notify.Values)
	}
	if len(got.Warnings) != 2 {
		t.Errorf("want one warning per rejected entry, got: %v", got.Warnings)
	}
}

func TestResolve_unknownSlackChannelInAnnotationLeavesCascadeValue(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    slackFrom:
      namespaceAnnotation: app.example.test/slack
`)
	env := Env{
		NamespaceAnnotations: map[string]string{"app.example.test/slack": "no-such-channel"},
		SlackChannels:        map[string]struct{}{"ops-alerts": {}},
	}
	got := Resolve(rules, annotatedIng("acme-api-1", "api", nil, "api.example.test"), "api.example.test", env)

	if got.Config.Slack != "ops-alerts" {
		t.Errorf("slack = %q, want the cascade's ops-alerts", got.Config.Slack)
	}
	// The whole point: a bad annotation must not cost monitoring.
	if got.Err != nil {
		t.Errorf("a rejected annotation must still resolve to a materializable config, got: %v", got.Err)
	}
	if len(got.Warnings) != 1 {
		t.Errorf("want one warning, got: %v", got.Warnings)
	}
}

func TestResolve_pathAnnotationWithoutLeadingSlashIsRejected(t *testing.T) {
	rules := parseRules(t, vsRootYAML+`
- when: {namespace: "acme-*"}
  config:
    pathFrom:
      annotation: app.example.test/health-check
`)
	ing := annotatedIng("acme-api-1", "api",
		map[string]string{"app.example.test/health-check": "healthz"}, "api.example.test")

	got := Resolve(rules, ing, "api.example.test", Env{})

	if got.Config.Path != "/" {
		t.Errorf("path = %q, want the root's /", got.Config.Path)
	}
	if got.Err != nil {
		t.Errorf("the monitor must still materialize, got: %v", got.Err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0].Reason, "'/'") {
		t.Errorf("want a leading-slash warning, got: %v", got.Warnings)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The warning map must be swept by the same observed-set prune that
// sweeps plans. A monitor that stops materializing — its ingress is
// gone, or it flipped to kube-invalid — never reaches the success path
// that would clear its warnings, so without the sweep /issues and the
// toggle_monitor_issues gauge over-report forever and an alert on that
// series can never resolve.
func TestPrune_dropsWarningsForMonitorsThatNoLongerMaterialize(t *testing.T) {
	m := &Materializer{
		kubePlans: map[string]scheduler.Plan{},
		annotationWarnings: map[string]MonitorWarnings{
			"kube-live":  {Slug: "kube-live", Warnings: []Warning{{Field: "notify"}}},
			"kube-gone":  {Slug: "kube-gone", Warnings: []Warning{{Field: "notify"}}},
			"kube-inval": {Slug: "kube-inval", Warnings: []Warning{{Field: "path"}}},
		},
	}

	m.Prune(map[string]struct{}{"kube-live": {}})

	got := m.AnnotationWarnings()
	if len(got) != 1 || got[0].Slug != "kube-live" {
		t.Errorf("AnnotationWarnings() = %+v, want only kube-live to survive", got)
	}
}
