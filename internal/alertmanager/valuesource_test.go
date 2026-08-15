package alertmanager_test

import (
	"reflect"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// fakeNamespaces is an in-memory alertmanager.NamespaceAnnotationSource.
// A namespace absent from the map reads as unreadable (nil), which is
// what the real informer returns for a cache miss.
type fakeNamespaces map[string]map[string]string

func (f fakeNamespaces) NamespaceAnnotations(namespace string) map[string]string {
	return f[namespace]
}

// envWith builds an Env whose rosters admit the given channels and
// handles, so tests only have to name what they care about.
func envWith(ns fakeNamespaces, channels []string, handles []string) alertmanager.Env {
	env := alertmanager.Env{Namespaces: ns}
	if channels != nil {
		env.KnownChannel = member(channels)
	}
	if handles != nil {
		env.KnownHandle = member(handles)
	}
	return env
}

// member returns a membership predicate over the given slugs.
func member(slugs []string) func(string) bool {
	set := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		set[s] = struct{}{}
	}
	return func(s string) bool {
		_, ok := set[s]
		return ok
	}
}

func TestEvaluate_slackFrom_readsNamespaceAnnotation(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/slack": "team-a-alerts"}},
		[]string{"ops-default", "team-a-alerts"}, nil,
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "team-a-alerts" {
		t.Errorf("Channel: got %q, want team-a-alerts", got.Channel)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings: got %v, want none", got.Warnings)
	}
	want := []alertmanager.Provenance{{
		Rule:  "match[1]",
		Field: "slack",
		Key:   "app.example.test/slack",
		Scope: alertmanager.ScopeNamespace,
		Value: "team-a-alerts",
	}}
	if !reflect.DeepEqual(got.Provenance, want) {
		t.Errorf("Provenance: got %+v, want %+v", got.Provenance, want)
	}
}

func TestEvaluate_notifyFrom_unionsWithTheCascade(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default", "oncall"),
		{
			Config: &config.AlertmanagerMatchConfig{
				NotifyFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/notify": "alice, bob"}},
		nil, []string{"oncall", "alice", "bob"},
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if want := []string{"oncall", "alice", "bob"}; !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v", got.Notify, want)
	}
}

func TestEvaluate_notifyOverrideFrom_replacesTheCascade(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default", "oncall"),
		{
			Config: &config.AlertmanagerMatchConfig{
				NotifyOverrideFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/notify": "alice,bob"}},
		nil, []string{"oncall", "alice", "bob"},
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if want := []string{"alice", "bob"}; !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v", got.Notify, want)
	}
}

func TestEvaluate_slackFrom_unknownChannel_keepsCascadeValue(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/slack": "typo-channel"}},
		[]string{"ops-default"}, nil,
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want the cascade's ops-default", got.Channel)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("Warnings: got %v, want exactly one", got.Warnings)
	}
	if w := got.Warnings[0]; w.Value != "typo-channel" || w.Field != "slack" {
		t.Errorf("Warnings[0]: got %+v, want the rejected slack value", w)
	}
	if len(got.Provenance) != 0 {
		t.Errorf("Provenance: got %v, want none (nothing was sourced)", got.Provenance)
	}
}

func TestEvaluate_slackFrom_unknownChannel_fallsBackToDefault(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/slack",
					DefaultScalar:       "team-fallback",
					HasDefault:          true,
				},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/slack": "typo-channel"}},
		[]string{"ops-default", "team-fallback"}, nil,
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "team-fallback" {
		t.Errorf("Channel: got %q, want team-fallback", got.Channel)
	}
	if len(got.Warnings) != 1 {
		t.Errorf("Warnings: got %v, want exactly one", got.Warnings)
	}
}

func TestEvaluate_notifyFrom_keepsUsableHandlesAndWarnsOnTheRest(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				NotifyFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/notify": "alice,nosuchperson,<!here>"}},
		nil, []string{"alice"},
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if want := []string{"alice"}; !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v", got.Notify, want)
	}
	if len(got.Warnings) != 2 {
		t.Fatalf("Warnings: got %v, want two (unknown slug + raw markup)", got.Warnings)
	}
}

func TestEvaluate_notifyOverrideFrom_allEntriesUnusable_keepsTheCascade(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default", "oncall"),
		{
			Config: &config.AlertmanagerMatchConfig{
				NotifyOverrideFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/notify": "nosuchperson"}},
		nil, []string{"oncall"},
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if want := []string{"oncall"}; !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want the cascade's %v — an unusable override must not silence mentions", got.Notify, want)
	}
}

func TestEvaluate_slackFrom_alertHasNoNamespaceLabel_silentlyFallsBack(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
			},
		},
	}
	env := envWith(fakeNamespaces{"team-a": {"app.example.test/slack": "team-a-alerts"}},
		[]string{"ops-default", "team-a-alerts"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "Watchdog"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
	// A cluster-scoped alert (Watchdog, node pressure) has no namespace by
	// nature. Warning on every one of them would pin the rejection
	// counter permanently non-zero and drown the real misconfigurations.
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings: got %v, want none — an unnamespaced alert is not a misconfiguration", got.Warnings)
	}
}

// The informer lister returns nil for a namespace it has never seen AND
// for one that simply carries no annotations. Both mean "no annotation",
// and 10 of the cluster's 78 ingress-bearing namespaces are unannotated,
// so neither may warn.
func TestEvaluate_slackFrom_namespaceHasNoAnnotations_silentlyFallsBack(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
			},
		},
	}
	env := envWith(fakeNamespaces{}, []string{"ops-default"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "unannotated-ns"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings: got %v, want none", got.Warnings)
	}
}

func TestEvaluate_slackFrom_namespaceLabelOverride(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/slack",
					NamespaceLabel:      "exported_namespace",
				},
			},
		},
	}
	env := envWith(fakeNamespaces{"team-a": {"app.example.test/slack": "team-a-alerts"}},
		[]string{"ops-default", "team-a-alerts"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "exported_namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "team-a-alerts" {
		t.Errorf("Channel: got %q, want team-a-alerts", got.Channel)
	}
}

func TestEvaluate_slackFrom_noAnnotationSource_warnsAndFallsBack(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
			},
		},
	}

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, alertmanager.Env{})

	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
	if len(got.Warnings) != 1 {
		t.Errorf("Warnings: got %v, want exactly one", got.Warnings)
	}
}

func TestEvaluate_slackFrom_annotationAbsent_usesDefaultAndNotesProvenance(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/slack",
					DefaultScalar:       "team-fallback",
					HasDefault:          true,
				},
			},
		},
	}
	env := envWith(fakeNamespaces{"team-a": {}}, []string{"ops-default", "team-fallback"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "team-fallback" {
		t.Errorf("Channel: got %q, want team-fallback", got.Channel)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings: got %v, want none — an absent annotation is not a misconfiguration", got.Warnings)
	}
	if len(got.Provenance) != 1 || got.Provenance[0].Scope != alertmanager.ScopeDefault {
		t.Errorf("Provenance: got %+v, want one default-scoped entry", got.Provenance)
	}
}

// Warning codes are the metric's label values, so they must be a small
// fixed vocabulary rather than the free-text Reason.
func TestEvaluate_warningCodes_areStable(t *testing.T) {
	src := func(l string) *config.ValueSource {
		return &config.ValueSource{NamespaceAnnotation: "app.example.test/slack", NamespaceLabel: l}
	}
	cases := []struct {
		name   string
		rules  []config.AlertmanagerMatchRule
		env    alertmanager.Env
		labels map[string]string
		want   string
	}{
		{
			name: "no annotation source wired",
			rules: []config.AlertmanagerMatchRule{rootRule("ops-default"),
				{Config: &config.AlertmanagerMatchConfig{SlackFrom: src("")}}},
			env:    alertmanager.Env{},
			labels: map[string]string{"alertname": "X", "namespace": "team-a"},
			want:   alertmanager.CodeNoSource,
		},
		{
			name: "unknown notify handle",
			rules: []config.AlertmanagerMatchRule{rootRule("ops-default"),
				{Config: &config.AlertmanagerMatchConfig{
					NotifyFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"}}}},
			env: envWith(fakeNamespaces{"team-a": {"app.example.test/notify": "nosuchperson"}},
				nil, []string{"oncall"}),
			labels: map[string]string{"alertname": "X", "namespace": "team-a"},
			want:   alertmanager.CodeValueRejected,
		},
		{
			name: "unknown channel slug",
			rules: []config.AlertmanagerMatchRule{rootRule("ops-default"),
				{Config: &config.AlertmanagerMatchConfig{SlackFrom: src("")}}},
			env: envWith(fakeNamespaces{"team-a": {"app.example.test/slack": "nope"}},
				[]string{"ops-default"}, nil),
			labels: map[string]string{"alertname": "X", "namespace": "team-a"},
			want:   alertmanager.CodeValueRejected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alertmanager.Evaluate(tc.rules, alert(tc.labels), alertmanager.Envelope{}, tc.env)
			if len(got.Warnings) != 1 {
				t.Fatalf("Warnings: got %v, want exactly one", got.Warnings)
			}
			if got.Warnings[0].Code != tc.want {
				t.Errorf("Code: got %q, want %q", got.Warnings[0].Code, tc.want)
			}
		})
	}
}

// The rule chain is the AM tree's only debugging surface — there is no
// explain subcommand — so a sourced value has to name itself there.
func TestEvaluate_ruleChain_carriesAnnotationProvenance(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{Labels: map[string]string{"namespace": "team-*"}},
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
			},
		},
	}
	env := envWith(fakeNamespaces{"team-a": {"app.example.test/slack": "team-a-alerts"}},
		[]string{"ops-default", "team-a-alerts"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	want := "match[0] → match[1] (labels.namespace=team-*) | slack=team-a-alerts ← namespaceAnnotation app.example.test/slack"
	if got.RuleChain != want {
		t.Errorf("RuleChain:\n got %q\nwant %q", got.RuleChain, want)
	}
}

func TestEvaluate_ruleChain_unchangedWithoutValueSources(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{rootRule("ops-default")}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}),
		alertmanager.Envelope{}, alertmanager.Env{})
	if got.RuleChain != "match[0]" {
		t.Errorf("RuleChain: got %q, want match[0]", got.RuleChain)
	}
}

// Lowering happens at the rule's own position, so a sourced value is
// an ordinary layer in the cascade — a deeper literal still wins, and a
// deeper literal notify still unions on top of a sourced one. Every
// other test puts the *From rule last, where "lands in position" and
// "sourced values win" are indistinguishable.
func TestEvaluate_sourcedValueIsJustAnotherLayer(t *testing.T) {
	env := envWith(
		fakeNamespaces{"team-a": {
			"app.example.test/slack":  "team-a-alerts",
			"app.example.test/notify": "alice",
		}},
		[]string{"ops-default", "team-a-alerts", "deepest-alerts"},
		[]string{"oncall", "alice", "bob"},
	)
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default", "oncall"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom:          &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
				NotifyOverrideFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"},
			},
		},
		{
			When: &config.AlertmanagerMatchWhen{Alertname: "HighCPU"},
			Config: &config.AlertmanagerMatchConfig{
				Slack:  "deepest-alerts",
				Notify: config.NotifyList{Values: []string{"bob"}},
			},
		},
	}

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "deepest-alerts" {
		t.Errorf("Channel: got %q, want deepest-alerts — a deeper literal outranks a sourced value", got.Channel)
	}
	// notifyOverrideFrom replaced the root's [oncall] at its own
	// position; the deeper literal then unions onto the result.
	if want := []string{"alice", "bob"}; !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v", got.Notify, want)
	}
}

// The markup ban must hold on its own, not only where the roster check
// would have rejected the entry anyway. With no roster wired, an
// annotation could otherwise inject a broadcast mention.
func TestEvaluate_notifyFrom_rejectsRawMarkupWithNoRoster(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default", "oncall"),
		{
			Config: &config.AlertmanagerMatchConfig{
				NotifyFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/notify"},
			},
		},
	}
	env := alertmanager.Env{
		Namespaces: fakeNamespaces{"team-a": {"app.example.test/notify": "<!channel>"}},
	}

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if want := []string{"oncall"}; !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v — an annotation may not set raw Slack markup", got.Notify, want)
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Code != alertmanager.CodeValueRejected {
		t.Errorf("Warnings: got %+v, want one value_rejected", got.Warnings)
	}
}

// A *From block under nested: resolves against its nested rule label.
func TestEvaluate_slackFrom_underNestedRule(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{Labels: map[string]string{"namespace": "team-*"}},
			Nested: []config.AlertmanagerMatchRule{{
				Config: &config.AlertmanagerMatchConfig{
					SlackFrom: &config.ValueSource{NamespaceAnnotation: "app.example.test/slack"},
				},
			}},
		},
	}
	env := envWith(fakeNamespaces{"team-a": {"app.example.test/slack": "team-a-alerts"}},
		[]string{"ops-default", "team-a-alerts"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	if got.Channel != "team-a-alerts" {
		t.Errorf("Channel: got %q, want team-a-alerts", got.Channel)
	}
	if len(got.Provenance) != 1 || got.Provenance[0].Rule != "match[1].nested[0]" {
		t.Errorf("Provenance rule label: got %+v, want match[1].nested[0]", got.Provenance)
	}
}

// The default-scope provenance string lands in am_alerts.rule_chain, so
// its rendering is operator-visible.
func TestEvaluate_provenanceRendering_defaultScope(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/slack",
					DefaultScalar:       "team-fallback",
					HasDefault:          true,
				},
			},
		},
	}
	env := envWith(fakeNamespaces{"team-a": {}}, []string{"ops-default", "team-fallback"}, nil)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	want := "match[0] → match[1] | slack=team-fallback ← default (app.example.test/slack absent)"
	if got.RuleChain != want {
		t.Errorf("RuleChain:\n got %q\nwant %q", got.RuleChain, want)
	}
}

// A rejected value and an absent one both land on the default, but they
// are different operator problems: the first says "your annotation is
// wrong", the second says "you have no annotation". The rule chain is
// the first thing read when debugging a routing surprise, so it has to
// tell them apart.
func TestEvaluate_provenanceRendering_rejectedValueIsNotAbsent(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/slack",
					DefaultScalar:       "team-fallback",
					HasDefault:          true,
				},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/slack": "no-such-channel"}},
		[]string{"ops-default", "team-fallback"}, nil,
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	want := "match[0] → match[1] | slack=team-fallback ← default (app.example.test/slack rejected)"
	if got.RuleChain != want {
		t.Errorf("RuleChain:\n got %q\nwant %q", got.RuleChain, want)
	}
	if len(got.Provenance) != 1 || got.Provenance[0].Cause != alertmanager.CauseRejected {
		t.Errorf("Provenance: got %+v, want one entry with Cause=%q", got.Provenance, alertmanager.CauseRejected)
	}
}

// With no annotation source the annotation was never read, so the chain
// must not claim it was absent — the namespace may well carry one.
func TestEvaluate_provenanceRendering_unreadableIsNotAbsent(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				SlackFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/slack",
					DefaultScalar:       "team-fallback",
					HasDefault:          true,
				},
			},
		},
	}
	env := alertmanager.Env{KnownChannel: member([]string{"ops-default", "team-fallback"})}

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	want := "match[0] → match[1] | slack=team-fallback ← default (app.example.test/slack unreadable)"
	if got.RuleChain != want {
		t.Errorf("RuleChain:\n got %q\nwant %q", got.RuleChain, want)
	}
}

// Every entry of a list source being rejected is a rejection, not an
// absence — the Override twin silently replacing a roster is exactly
// the case an operator needs named accurately.
func TestEvaluate_provenanceRendering_listWithEveryEntryRejected(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			Config: &config.AlertmanagerMatchConfig{
				NotifyFrom: &config.ValueSource{
					NamespaceAnnotation: "app.example.test/notify",
					DefaultList:         []string{"oncall"},
					HasDefault:          true,
				},
			},
		},
	}
	env := envWith(
		fakeNamespaces{"team-a": {"app.example.test/notify": "ghost,phantom"}},
		[]string{"ops-default"}, []string{"oncall"},
	)

	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "HighCPU", "namespace": "team-a"}),
		alertmanager.Envelope{}, env)

	want := "match[0] → match[1] | notify=[oncall] ← default (app.example.test/notify rejected)"
	if got.RuleChain != want {
		t.Errorf("RuleChain:\n got %q\nwant %q", got.RuleChain, want)
	}
}
