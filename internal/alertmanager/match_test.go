package alertmanager_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/alertmanager"
	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// boolPtr is a tiny helper for building the *bool ignore directive
// inline in fixtures.
func boolPtr(b bool) *bool { return &b }

// alert is a tiny constructor for building a labels-only Alert in
// table-driven tests; the cascade evaluator only inspects labels and
// the envelope so the rest of the Alert wire shape is irrelevant.
func alert(labels map[string]string) alertmanager.Alert {
	return alertmanager.Alert{Labels: labels}
}

// rootRule builds a baseline rule (empty when:, root config) for use
// in every test — the validator (Slice 2) makes this the mandatory
// root in production configs.
func rootRule(slack string, notify ...string) config.AlertmanagerMatchRule {
	return config.AlertmanagerMatchRule{
		Config: &config.AlertmanagerMatchConfig{
			Slack:  slack,
			Notify: config.NotifyList{Values: notify},
		},
	}
}

func TestEvaluate_rootOnly_returnsRootConfig(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{rootRule("ops-default", "ops")}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "Anything"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Ignored {
		t.Fatalf("Ignored: got true, want false")
	}
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
	if !reflect.DeepEqual(got.Notify, []string{"ops"}) {
		t.Errorf("Notify: got %v, want [ops]", got.Notify)
	}
	if got.RuleChain != "match[0]" {
		t.Errorf("RuleChain: got %q, want %q", got.RuleChain, "match[0]")
	}
	if got.Final {
		t.Errorf("Final: got true, want false")
	}
}

// --- Selector dimensions -----------------------------------------

func TestEvaluate_alertname_globMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "High*"},
			Config: &config.AlertmanagerMatchConfig{Slack: "high-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "HighCPU"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "high-channel" {
		t.Errorf("Channel: got %q, want high-channel", got.Channel)
	}
}

func TestEvaluate_alertname_globMiss(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "High*"},
			Config: &config.AlertmanagerMatchConfig{Slack: "high-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "LowDisk"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

func TestEvaluate_alertname_emptySelector_matchesAnything(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			// When set but Alertname empty — should still match.
			When:   &config.AlertmanagerMatchWhen{},
			Config: &config.AlertmanagerMatchConfig{Slack: "child"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "Anything"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "child" {
		t.Errorf("Channel: got %q, want child", got.Channel)
	}
}

func TestEvaluate_alertnameRegex_match(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{AlertnameRegex: "Pod.*"},
			Config: &config.AlertmanagerMatchConfig{Slack: "pod-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "PodCrashLooping"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "pod-channel" {
		t.Errorf("Channel: got %q, want pod-channel", got.Channel)
	}
}

func TestEvaluate_alertnameRegex_autoAnchored(t *testing.T) {
	// "acme" must NOT match "acme-prod" — auto-anchor wraps as ^acme$.
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{AlertnameRegex: "acme"},
			Config: &config.AlertmanagerMatchConfig{Slack: "acme-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "acme-prod"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default (auto-anchor should reject acme-prod)", got.Channel)
	}
	// But "acme" should match "acme" exactly.
	got = alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "acme"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "acme-channel" {
		t.Errorf("Channel: got %q, want acme-channel for exact match", got.Channel)
	}
}

func TestEvaluate_labels_exactMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{
				Labels: map[string]string{"severity": "critical"},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "crit-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"severity": "critical"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "crit-channel" {
		t.Errorf("Channel: got %q, want crit-channel", got.Channel)
	}
}

func TestEvaluate_labels_globMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{
				Labels: map[string]string{"namespace": "acme-*"},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "acme-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"namespace": "acme-prod"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "acme-channel" {
		t.Errorf("Channel: got %q, want acme-channel", got.Channel)
	}
}

func TestEvaluate_labels_regexMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{
				Labels: map[string]string{"instanceRegex": `pod-\d+`},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "pod-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"instance": "pod-42"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "pod-channel" {
		t.Errorf("Channel: got %q, want pod-channel", got.Channel)
	}
	// pod-foo must not match (auto-anchored regex).
	got = alertmanager.Evaluate(rules, alert(map[string]string{"instance": "pod-foo"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

func TestEvaluate_labels_missingOnAlert_doesNotMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{
				Labels: map[string]string{"severity": "critical"},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "crit-channel"},
		},
	}
	// Alert has alertname but no severity label.
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "HighCPU"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

func TestEvaluate_receiver_exactMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{Receiver: "toggle_monitor"},
			Config: &config.AlertmanagerMatchConfig{Slack: "tm-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{Receiver: "toggle_monitor"}, alertmanager.Env{})
	if got.Channel != "tm-channel" {
		t.Errorf("Channel: got %q, want tm-channel", got.Channel)
	}
	// Miss
	got = alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{Receiver: "other"}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

func TestEvaluate_externalURL_exactMatch(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{ExternalURL: "https://am.staging.example.test"},
			Config: &config.AlertmanagerMatchConfig{Slack: "staging-channel"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{ExternalURL: "https://am.staging.example.test"}, alertmanager.Env{})
	if got.Channel != "staging-channel" {
		t.Errorf("Channel: got %q, want staging-channel", got.Channel)
	}
	// Miss
	got = alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{ExternalURL: "https://am.prod.example.test"}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

func TestEvaluate_multiField_AND(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When: &config.AlertmanagerMatchWhen{
				Alertname: "HighCPU",
				Labels:    map[string]string{"severity": "critical"},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "critical-cpu"},
		},
	}
	// Both match
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "HighCPU", "severity": "critical"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "critical-cpu" {
		t.Errorf("Channel: got %q, want critical-cpu", got.Channel)
	}
	// Only alertname matches, severity wrong
	got = alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "HighCPU", "severity": "warning"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

// --- Tree walk ----------------------------------------------------

func TestEvaluate_nestedOverridesRoot(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		{
			Config: &config.AlertmanagerMatchConfig{Slack: "root-channel"},
			Nested: []config.AlertmanagerMatchRule{
				{
					When:   &config.AlertmanagerMatchWhen{Alertname: "HighCPU"},
					Config: &config.AlertmanagerMatchConfig{Slack: "child-channel"},
				},
			},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "HighCPU"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "child-channel" {
		t.Errorf("Channel: got %q, want child-channel", got.Channel)
	}
}

func TestEvaluate_multipleMatchingSiblings_allContribute(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root-channel", "a"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{Slack: "s1", Notify: config.NotifyList{Values: []string{"b"}}},
		},
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{Slack: "s2", Notify: config.NotifyList{Values: []string{"c"}}},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	// Deepest in document order wins for slack.
	if got.Channel != "s2" {
		t.Errorf("Channel: got %q, want s2", got.Channel)
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v", got.Notify, want)
	}
}

func TestEvaluate_nonMatchingNested_skipped(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root-channel"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "Different"},
			Config: &config.AlertmanagerMatchConfig{Slack: "other"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "root-channel" {
		t.Errorf("Channel: got %q, want root-channel", got.Channel)
	}
}

func TestEvaluate_finalHaltsTraversal(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Final:  true,
			Config: &config.AlertmanagerMatchConfig{Slack: "a"},
		},
		{
			// This rule would match but final on prior sibling halts traversal.
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{Slack: "b"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "a" {
		t.Errorf("Channel: got %q, want a (final on rule index 1 should halt traversal)", got.Channel)
	}
	if !got.Final {
		t.Errorf("Final: got false, want true")
	}
	if !strings.Contains(got.RuleChain, "[final]") {
		t.Errorf("RuleChain: %q should contain [final]", got.RuleChain)
	}
}

func TestEvaluate_finalDoesNotHaltOwnNested(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Final:  true,
			Config: &config.AlertmanagerMatchConfig{Slack: "a"},
			Nested: []config.AlertmanagerMatchRule{
				{
					When:   &config.AlertmanagerMatchWhen{Labels: map[string]string{"severity": "critical"}},
					Config: &config.AlertmanagerMatchConfig{Slack: "a-crit"},
				},
			},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X", "severity": "critical"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "a-crit" {
		t.Errorf("Channel: got %q, want a-crit (final's own nested still contributes)", got.Channel)
	}
}

// --- Merge --------------------------------------------------------

func TestEvaluate_scalarUnsetDeeperPreservesShallower(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root-channel"),
		{
			When: &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{
				// Slack unset: should not override root.
				Notify: config.NotifyList{Values: []string{"team"}},
			},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Channel != "root-channel" {
		t.Errorf("Channel: got %q, want root-channel (deeper unset slack should preserve root)", got.Channel)
	}
}

func TestEvaluate_notifyUnion_dedupes(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root", "a", "b"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{Notify: config.NotifyList{Values: []string{"b", "c"}}},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v (union dedup, shallow-first)", got.Notify, want)
	}
}

func TestEvaluate_notifyOverride_replaces(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root", "a", "b"),
		{
			When: &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{
				Notify: config.NotifyList{Values: []string{"x"}, Override: true},
			},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	want := []string{"x"}
	if !reflect.DeepEqual(got.Notify, want) {
		t.Errorf("Notify: got %v, want %v (override replaces ancestors)", got.Notify, want)
	}
}

func TestEvaluate_notifyOverrideEmpty_resolvesEmpty(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root", "a", "b"),
		{
			When: &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{
				Notify: config.NotifyList{Values: []string{}, Override: true},
			},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if len(got.Notify) != 0 {
		t.Errorf("Notify: got %v, want empty (override [] clears)", got.Notify)
	}
}

// --- Ignore -------------------------------------------------------

func TestEvaluate_noIgnore_returnsConfig(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{rootRule("ops-default")}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Ignored {
		t.Errorf("Ignored: got true, want false")
	}
}

func TestEvaluate_nestedIgnoreTrue_setsIgnored(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "Watchdog"},
			Ignore: boolPtr(true),
			Final:  true,
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "Watchdog"}), alertmanager.Envelope{}, alertmanager.Env{})
	if !got.Ignored {
		t.Errorf("Ignored: got false, want true")
	}
	if !strings.Contains(got.RuleChain, "[ignored]") {
		t.Errorf("RuleChain: %q should contain [ignored]", got.RuleChain)
	}
}

func TestEvaluate_deeperUnignore_unsetsIgnore(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default"),
		{
			When:   &config.AlertmanagerMatchWhen{Labels: map[string]string{"namespace": "test-*"}},
			Ignore: boolPtr(true),
			Nested: []config.AlertmanagerMatchRule{
				{
					When:   &config.AlertmanagerMatchWhen{Labels: map[string]string{"namespace": "test-critical-*"}},
					Ignore: boolPtr(false),
				},
			},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"namespace": "test-critical-foo"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.Ignored {
		t.Errorf("Ignored: got true, want false (deeper un-ignore)")
	}
	if got.Channel != "ops-default" {
		t.Errorf("Channel: got %q, want ops-default", got.Channel)
	}
}

func TestEvaluate_ignored_channelAndNotifyEmpty(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("ops-default", "team"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "Watchdog"},
			Ignore: boolPtr(true),
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "Watchdog"}), alertmanager.Envelope{}, alertmanager.Env{})
	if !got.Ignored {
		t.Fatalf("Ignored: got false, want true")
	}
	if got.Channel != "" {
		t.Errorf("Channel: got %q, want empty when Ignored", got.Channel)
	}
	if len(got.Notify) != 0 {
		t.Errorf("Notify: got %v, want empty when Ignored", got.Notify)
	}
	if got.RuleChain == "" {
		t.Errorf("RuleChain: got empty, want non-empty (operator still needs the trace)")
	}
}

// --- Rule chain rendering -----------------------------------------

func TestEvaluate_ruleChain_singleRoot(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{rootRule("ops-default")}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if got.RuleChain != "match[0]" {
		t.Errorf("RuleChain: got %q, want %q", got.RuleChain, "match[0]")
	}
}

func TestEvaluate_ruleChain_rootPlusNested(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root"),
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "X"},
			Config: &config.AlertmanagerMatchConfig{Slack: "child"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "X"}), alertmanager.Envelope{}, alertmanager.Env{})
	if !strings.Contains(got.RuleChain, "match[0]") {
		t.Errorf("RuleChain: %q should contain match[0]", got.RuleChain)
	}
	if !strings.Contains(got.RuleChain, "match[1]") {
		t.Errorf("RuleChain: %q should contain match[1]", got.RuleChain)
	}
	if !strings.Contains(got.RuleChain, "alertname=X") {
		t.Errorf("RuleChain: %q should contain alertname=X selector summary", got.RuleChain)
	}
	if !strings.Contains(got.RuleChain, " → ") {
		t.Errorf("RuleChain: %q should join with arrow", got.RuleChain)
	}
}

func TestEvaluate_ruleChain_selectorSummary(t *testing.T) {
	// Cover every selector vocabulary token in one chain.
	rules := []config.AlertmanagerMatchRule{
		rootRule("root"),
		{
			When: &config.AlertmanagerMatchWhen{
				Alertname:   "Watchdog",
				Labels:      map[string]string{"severity": "critical"},
				Receiver:    "rcv",
				ExternalURL: "https://am.example.test",
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "child"},
		},
	}
	// Build alert that matches every selector.
	got := alertmanager.Evaluate(rules,
		alert(map[string]string{"alertname": "Watchdog", "severity": "critical"}),
		alertmanager.Envelope{Receiver: "rcv", ExternalURL: "https://am.example.test"}, alertmanager.Env{})
	for _, want := range []string{"alertname=Watchdog", "labels.severity=critical", "receiver=rcv", "externalURL=https://am.example.test"} {
		if !strings.Contains(got.RuleChain, want) {
			t.Errorf("RuleChain: %q should contain %q", got.RuleChain, want)
		}
	}
}

func TestEvaluate_ruleChain_alertnameRegex(t *testing.T) {
	rules := []config.AlertmanagerMatchRule{
		rootRule("root"),
		{
			When:   &config.AlertmanagerMatchWhen{AlertnameRegex: "Pod.*"},
			Config: &config.AlertmanagerMatchConfig{Slack: "child"},
		},
	}
	got := alertmanager.Evaluate(rules, alert(map[string]string{"alertname": "PodCrashLooping"}), alertmanager.Envelope{}, alertmanager.Env{})
	if !strings.Contains(got.RuleChain, "alertnameRegex=Pod.*") {
		t.Errorf("RuleChain: %q should contain alertnameRegex=Pod.*", got.RuleChain)
	}
}

// --- Realistic end-to-end fixture ---------------------------------

func TestEvaluate_realisticFixture(t *testing.T) {
	// Mirrors the ADR-0005 Decision-section example, fleshed out for
	// the routing scenarios in the spec.
	rules := []config.AlertmanagerMatchRule{
		// 0: root
		{
			Config: &config.AlertmanagerMatchConfig{
				Slack:  "ops-default",
				Notify: config.NotifyList{Values: []string{"ops-team"}},
			},
		},
		// 1: Watchdog — ignored, final
		{
			When:   &config.AlertmanagerMatchWhen{Alertname: "Watchdog"},
			Ignore: boolPtr(true),
			Final:  true,
		},
		// 2: critical severity → oncall-pager
		{
			When: &config.AlertmanagerMatchWhen{
				Labels: map[string]string{"severity": "critical"},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "oncall-pager"},
		},
		// 3: kube-system namespace → k8s-infra
		{
			When: &config.AlertmanagerMatchWhen{
				Labels: map[string]string{"namespace": "kube-system"},
			},
			Config: &config.AlertmanagerMatchConfig{Slack: "k8s-infra"},
		},
		// 4: staging externalURL → staging-noise
		{
			When:   &config.AlertmanagerMatchWhen{ExternalURL: "https://am.staging.example.test"},
			Config: &config.AlertmanagerMatchConfig{Slack: "staging-noise"},
		},
	}

	cases := []struct {
		name        string
		alertLabels map[string]string
		envelope    alertmanager.Envelope
		wantIgnored bool
		wantChannel string
	}{
		{
			name:        "Watchdog ignored",
			alertLabels: map[string]string{"alertname": "Watchdog"},
			wantIgnored: true,
		},
		{
			name:        "critical → oncall-pager",
			alertLabels: map[string]string{"alertname": "HighCPU", "severity": "critical"},
			wantChannel: "oncall-pager",
		},
		{
			name:        "kube-system → k8s-infra",
			alertLabels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "kube-system"},
			wantChannel: "k8s-infra",
		},
		{
			name:        "staging externalURL → staging-noise",
			alertLabels: map[string]string{"alertname": "X"},
			envelope:    alertmanager.Envelope{ExternalURL: "https://am.staging.example.test"},
			wantChannel: "staging-noise",
		},
		{
			name:        "unmatched → ops-default",
			alertLabels: map[string]string{"alertname": "Mystery"},
			wantChannel: "ops-default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alertmanager.Evaluate(rules, alert(tc.alertLabels), tc.envelope, alertmanager.Env{})
			if got.Ignored != tc.wantIgnored {
				t.Errorf("Ignored: got %v, want %v (chain=%q)", got.Ignored, tc.wantIgnored, got.RuleChain)
			}
			if !tc.wantIgnored && got.Channel != tc.wantChannel {
				t.Errorf("Channel: got %q, want %q (chain=%q)", got.Channel, tc.wantChannel, got.RuleChain)
			}
		})
	}
}
