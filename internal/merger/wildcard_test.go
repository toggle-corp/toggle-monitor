package merger

import (
	"errors"
	"testing"
)

// wildcardFixtureYAML is a minimal root rule plus an ignore rule that
// matches any wildcard host. hostRegex is auto-anchored, so `\*\..*`
// matches only hosts whose leftmost label is the `*` k8s permits.
const wildcardFixtureYAML = `
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
- when: {namespace: "ignored-*"}
  ignore: true
`

// Resolve is the shared kernel behind Materialize, the explain CLI and
// the discovery detail page. Classifying the wildcard here — rather
// than in Materialize alone — is what keeps all three in agreement.
func TestResolve_wildcardHostIsInvalid(t *testing.T) {
	rules := parseRules(t, wildcardFixtureYAML)

	res := Resolve(rules, ing("acme", "x", nil, "*.foo.example.test"), "*.foo.example.test", Env{})

	if !res.Matched {
		t.Fatal("root rule should match")
	}
	if !res.WildcardHost {
		t.Error("WildcardHost should be true")
	}
	if !errors.Is(res.Err, ErrWildcardHost) {
		t.Errorf("Err: got %v, want ErrWildcardHost", res.Err)
	}
}

func TestResolve_ignoreSuppressesWildcardErr(t *testing.T) {
	rules := parseRules(t, wildcardFixtureYAML)

	res := Resolve(rules, ing("ignored-acme", "x", nil, "*.foo.example.test"), "*.foo.example.test", Env{})

	if !res.Ignored {
		t.Fatal("Ignored should be true")
	}
	if !res.WildcardHost {
		t.Error("WildcardHost should stay true so the row can explain itself")
	}
	if res.Err != nil {
		t.Errorf("Err should be nil for an ignored row, got %v", res.Err)
	}
}

func TestResolve_concreteHostIsNotWildcard(t *testing.T) {
	rules := parseRules(t, wildcardFixtureYAML)

	res := Resolve(rules, ing("acme", "x", nil, "api.example.test"), "api.example.test", Env{})

	if res.WildcardHost {
		t.Error("WildcardHost should be false for a concrete host")
	}
	if res.Err != nil {
		t.Errorf("Err: got %v, want nil", res.Err)
	}
}

// ResolveWithTrace shares the classifier with Resolve; assert it does
// not drift.
func TestResolveWithTrace_wildcardMatchesResolve(t *testing.T) {
	rules := parseRules(t, wildcardFixtureYAML)
	i := ing("acme", "x", nil, "*.foo.example.test")

	plain := Resolve(rules, i, "*.foo.example.test", Env{})
	traced, _ := ResolveWithTrace(rules, i, "*.foo.example.test", Env{})

	if plain.WildcardHost != traced.WildcardHost {
		t.Errorf("WildcardHost: plain %v, traced %v", plain.WildcardHost, traced.WildcardHost)
	}
	if !errors.Is(traced.Err, ErrWildcardHost) {
		t.Errorf("traced Err: got %v, want ErrWildcardHost", traced.Err)
	}
}

// The documented acknowledge rule leans on hostRegex auto-anchoring, so
// the anchors must bind every alternation branch — "^a|b$" would leave
// one side unanchored and silently over-match.
func TestMatchRegex_anchorsEveryAlternationBranch(t *testing.T) {
	const pattern = `a\.example\.test|b\.example\.test`
	for _, host := range []string{"evil-b.example.test", "a.example.test-foo"} {
		if matchRegex(pattern, host) {
			t.Errorf("%q should not match anchored %q", host, pattern)
		}
	}
	for _, host := range []string{"a.example.test", "b.example.test"} {
		if !matchRegex(pattern, host) {
			t.Errorf("%q should match anchored %q", host, pattern)
		}
	}
	// An operator who anchors by hand still gets the same answer.
	if !matchRegex(`^a\.example\.test$`, "a.example.test") {
		t.Error("hand-anchored pattern should still match")
	}
}
