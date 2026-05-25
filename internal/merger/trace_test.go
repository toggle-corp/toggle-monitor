package merger

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// traceFixtureYAML mirrors the CLI's explainYAML cascade so the trace
// tests exercise the same scenarios the operator UI will render.
// match[0] root → match[1] (ns=acme-*) adds notify=[bob] → nested[0]
// labels.minio [final] replaces path. match[2] is an !override-ed
// notify rule that exists only to stress the override path against a
// hand-shaped ingress.
const traceFixtureYAML = `
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
- when: {namespace: "acme-*"}
  config:
    notify: [bob]
  nested:
    - when: {labels: {"app.kubernetes.io/name": "minio"}}
      final: true
      config:
        path: /minio/health/live
- when: {namespace: "override-*"}
  config:
    notify: !override [carol]
- when: {namespace: "ignored-*"}
  ignore: true
`

func parseRules(t *testing.T, src string) []config.KubeMatchRule {
	t.Helper()
	var rules []config.KubeMatchRule
	if err := yaml.Unmarshal([]byte(src), &rules); err != nil {
		t.Fatalf("parse fixture yaml: %v", err)
	}
	return rules
}

func ing(ns, name string, labels map[string]string, host string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
}

// eventByKey returns the first event with the given Key in the given
// rule's Events slice. Used to assert single-key writes without
// depending on the field-emit order of applyConfig.
func eventByKey(rt RuleTrace, key string) (TraceEvent, bool) {
	for _, e := range rt.Events {
		if e.Key == key {
			return e, true
		}
	}
	return TraceEvent{}, false
}

func TestResolveWithTrace_materializedHappyPath(t *testing.T) {
	rules := parseRules(t, traceFixtureYAML)
	resolved, traces, chain, ignored, matched, err :=
		ResolveWithTrace(rules, ing("acme-eoapi-3", "x", map[string]string{"app.kubernetes.io/name": "minio"}, "api.example.com"), "api.example.com")
	if err != nil {
		t.Fatalf("unexpected resolved-validation error: %v", err)
	}
	if !matched || ignored {
		t.Fatalf("expected matched/!ignored, got matched=%v ignored=%v", matched, ignored)
	}
	if resolved.Path != "/minio/health/live" {
		t.Errorf("path: got %q, want /minio/health/live (nested[0] wins)", resolved.Path)
	}
	wantNotify := []string{"alice", "bob"}
	if !reflect.DeepEqual(resolved.Notify.Values, wantNotify) {
		t.Errorf("notify: got %v, want %v (union root+ns)", resolved.Notify.Values, wantNotify)
	}

	wantChain := []string{
		"match[0] ()",
		"match[1] (ns=acme-*)",
		"match[1].nested[0] (labels.app.kubernetes.io/name=minio) [final]",
	}
	if !reflect.DeepEqual(chain, wantChain) {
		t.Errorf("chain mismatch:\n got: %v\nwant: %v", chain, wantChain)
	}

	if len(traces) != 3 {
		t.Fatalf("expected 3 RuleTrace entries, got %d", len(traces))
	}

	// match[0] root: every required scalar is a TraceSet, notify is TraceAdd.
	root := traces[0]
	if root.Label != "match[0]" || root.When != " ()" || root.Final {
		t.Errorf("root rule identity mismatch: %+v", root)
	}
	if ev, ok := eventByKey(root, "path"); !ok || ev.Action != TraceSet || ev.NewValue != "/" {
		t.Errorf("root path event: %+v ok=%v", ev, ok)
	}
	if ev, ok := eventByKey(root, "notify"); !ok || ev.Action != TraceAdd ||
		!reflect.DeepEqual(ev.Added, []string{"alice"}) ||
		!reflect.DeepEqual(ev.NewList, []string{"alice"}) {
		t.Errorf("root notify event: %+v ok=%v", ev, ok)
	}

	// match[1]: only notify (union-add bob).
	ns := traces[1]
	if ns.Label != "match[1]" || ns.When != " (ns=acme-*)" || ns.Final {
		t.Errorf("ns rule identity mismatch: %+v", ns)
	}
	if len(ns.Events) != 1 {
		t.Fatalf("ns rule expected 1 event, got %d: %+v", len(ns.Events), ns.Events)
	}
	nEv := ns.Events[0]
	if nEv.Key != "notify" || nEv.Action != TraceAdd ||
		!reflect.DeepEqual(nEv.Added, []string{"bob"}) ||
		!reflect.DeepEqual(nEv.OldList, []string{"alice"}) ||
		!reflect.DeepEqual(nEv.NewList, []string{"alice", "bob"}) {
		t.Errorf("ns notify event mismatch: %+v", nEv)
	}

	// nested[0]: path replace.
	leaf := traces[2]
	if leaf.Label != "match[1].nested[0]" || !leaf.Final {
		t.Errorf("leaf identity mismatch: %+v", leaf)
	}
	if len(leaf.Events) != 1 {
		t.Fatalf("leaf expected 1 event, got %d", len(leaf.Events))
	}
	lEv := leaf.Events[0]
	if lEv.Key != "path" || lEv.Action != TraceReplace ||
		lEv.OldValue != "/" || lEv.NewValue != "/minio/health/live" {
		t.Errorf("leaf path event mismatch: %+v", lEv)
	}
}

func TestResolveWithTrace_overrideListDiscardsPrior(t *testing.T) {
	rules := parseRules(t, traceFixtureYAML)
	_, traces, _, _, matched, _ :=
		ResolveWithTrace(rules, ing("override-foo", "x", nil, "h.example.com"), "h.example.com")
	if !matched {
		t.Fatalf("expected matched, got false")
	}
	// Two rules fire: root (sets notify=[alice]) and override rule
	// (replaces with !override [carol]).
	if len(traces) != 2 {
		t.Fatalf("expected 2 RuleTrace entries, got %d", len(traces))
	}
	ov := traces[1]
	ev, ok := eventByKey(ov, "notify")
	if !ok {
		t.Fatalf("override rule missing notify event: %+v", ov)
	}
	if ev.Action != TraceOverride {
		t.Errorf("expected TraceOverride, got %s", ev.Action)
	}
	if !reflect.DeepEqual(ev.OldList, []string{"alice"}) {
		t.Errorf("OldList: got %v, want [alice]", ev.OldList)
	}
	if !reflect.DeepEqual(ev.NewList, []string{"carol"}) {
		t.Errorf("NewList: got %v, want [carol]", ev.NewList)
	}
	if !reflect.DeepEqual(ev.Removed, []string{"alice"}) {
		t.Errorf("Removed: got %v, want [alice]", ev.Removed)
	}
	if !reflect.DeepEqual(ev.Added, []string{"carol"}) {
		t.Errorf("Added: got %v, want [carol]", ev.Added)
	}
}

func TestResolveWithTrace_ignoredEmitsIgnoreEvent(t *testing.T) {
	rules := parseRules(t, traceFixtureYAML)
	resolved, traces, _, ignored, matched, err :=
		ResolveWithTrace(rules, ing("ignored-foo", "x", nil, "h.example.com"), "h.example.com")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !matched || !ignored {
		t.Fatalf("expected matched/ignored, got matched=%v ignored=%v", matched, ignored)
	}
	// Resolved is still computed even when ignored — the UI's
	// "would-have-been" panel reads it.
	if resolved.Path != "/" {
		t.Errorf("resolved.path: got %q, want / (root inherited)", resolved.Path)
	}
	// Second rule (ignore: true) carries the ignore event.
	if len(traces) < 2 {
		t.Fatalf("expected >=2 rules, got %d", len(traces))
	}
	ig := traces[1]
	ev, ok := eventByKey(ig, "ignore")
	if !ok {
		t.Fatalf("ignore rule missing ignore event: %+v", ig)
	}
	if ev.Action != TraceSet || ev.NewValue != "true" {
		t.Errorf("ignore event mismatch: %+v", ev)
	}
}

func TestResolveWithTrace_noMatch(t *testing.T) {
	// Empty rule list → no rule fires.
	_, traces, chain, ignored, matched, err := ResolveWithTrace(nil, ing("any", "x", nil, "h"), "h")
	if err != nil || ignored || matched {
		t.Errorf("unexpected outputs: err=%v ignored=%v matched=%v", err, ignored, matched)
	}
	if len(traces) != 0 || len(chain) != 0 {
		t.Errorf("expected empty trace + chain, got traces=%d chain=%d", len(traces), len(chain))
	}
}

func TestResolveWithTrace_invalidEmitsResolvedErr(t *testing.T) {
	// Root sets interval=10s, timeout=30s — timeout >= interval is a
	// resolved-value validation failure. matched=true, ignored=false,
	// resolvedErr non-nil so the UI renders the invalid outcome.
	src := `
- when: {}
  config:
    scheme: https
    path: /
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 10s
    timeout: 30s
    retries: 0
    retryBackoff: 1s
    followRedirects: false
    reminderInterval: 1h
    sslAlertThreshold: 30d
    sslEscalationThreshold: 7d
    sslReminderInterval: 1h
    slack: ops-alerts
`
	rules := parseRules(t, src)
	_, _, _, ignored, matched, err := ResolveWithTrace(rules, ing("ns", "x", nil, "h"), "h")
	if !matched || ignored {
		t.Fatalf("expected matched/!ignored, got matched=%v ignored=%v", matched, ignored)
	}
	if err == nil {
		t.Fatal("expected resolved-validation error for timeout>=interval, got nil")
	}
}

func TestFormatTraceList_emptyAndPopulated(t *testing.T) {
	if got := FormatTraceList(nil); got != "[]" {
		t.Errorf("FormatTraceList(nil) = %q, want []", got)
	}
	if got := FormatTraceList([]string{"a"}); got != "[a]" {
		t.Errorf("FormatTraceList([a]) = %q, want [a]", got)
	}
	if got := FormatTraceList([]string{"a", "b", "c"}); got != "[a, b, c]" {
		t.Errorf("FormatTraceList([a,b,c]) = %q", got)
	}
}
