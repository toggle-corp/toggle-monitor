package alertmanager

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// Envelope carries the AM webhook envelope fields that `when:`
// selectors can match against. The handler (later slice) builds one
// per inbound delivery and reuses it for every alert in the batch.
type Envelope struct {
	Receiver    string
	ExternalURL string
}

// Resolved is the cascade evaluator's output for one alert. The
// rendered RuleChain feeds the am_alerts.rule_chain debug column;
// Channel + Notify drive the Slack post. When Ignored is true the
// post is suppressed and Channel/Notify are not meaningful — callers
// record an "am-ignored" row instead.
type Resolved struct {
	Ignored   bool
	Channel   string
	Notify    []string
	RuleChain string
	Final     bool

	// Provenance and Warnings cover the ADR-0013 annotation inputs:
	// which fields came from a Namespace annotation rather than the
	// tree, and which annotation values were rejected. Both empty for a
	// tree that uses only literals.
	Provenance []Provenance
	Warnings   []Warning
}

// Evaluate walks the configured AM match tree against one alert and
// returns its resolved routing. Mirrors the kube cascade in
// internal/merger (ADR-0002) — same overall shape (tree walker →
// merge stack → resolved config + rule chain) adapted to AM's
// selector vocabulary.
//
// The function form takes the rules per call (rather than a long-
// lived Evaluator struct) because the regex cache is keyed on the
// source string and shared across calls via a package-level sync.Map;
// no per-Evaluator state would buy us anything. Selectors are
// validated at config-load (Slice 2) so regexp.Compile here cannot
// fail in practice — failures fall through to "this rule doesn't
// match" rather than panicking.
func Evaluate(rules []config.AlertmanagerMatchRule, alert Alert, envelope Envelope, env Env) Resolved {
	vr := newValueResolver(alert, env)
	stack, chain, final := walkRules(rules, alert, envelope, vr)

	// Deepest-wins ignore resolution across the matched stack.
	ignored := false
	ignoreIdx := -1
	for i, layer := range stack {
		if layer.ignore != nil {
			ignored = *layer.ignore
			ignoreIdx = i
		}
	}
	// Annotate the ignoring step in the chain.
	if ignored && ignoreIdx >= 0 && ignoreIdx < len(chain.steps) {
		chain.steps[ignoreIdx] += " [ignored]"
	}

	res := Resolved{
		RuleChain:  chain.render(vr.provenance),
		Final:      final,
		Provenance: vr.provenance,
		Warnings:   vr.warnings,
	}
	if ignored {
		res.Ignored = true
		return res
	}
	cfg := resolveStack(stack)
	res.Channel = cfg.Slack
	res.Notify = cfg.Notify.Values
	return res
}

// stackEntry pairs a rule's config with its ignore directive so the
// resolver can apply both in stack order.
type stackEntry struct {
	cfg    config.AlertmanagerMatchConfig
	ignore *bool
}

// ruleChain captures the ordered sequence of matched rule labels for
// the debug surface. Same shape as merger.ruleChain — keep them
// visually consistent so operators can read both traces at a glance.
type ruleChain struct {
	steps []string
}

func (c *ruleChain) push(s string) { c.steps = append(c.steps, s) }
func (c ruleChain) String() string { return strings.Join(c.steps, " → ") }

// render appends annotation provenance to the chain, pipe-separated. The
// rule chain is the AM tree's only debugging surface — there is no
// explain subcommand for it — so "which rules matched" and "where the
// values came from" share the one column.
func (c ruleChain) render(prov []Provenance) string {
	out := c.String()
	for _, p := range prov {
		out += " | " + p.String()
	}
	return out
}

// walkRules traverses the AM match tree depth-first in document
// order, collecting every rule whose `when:` matches the (alert,
// envelope) pair into the merge stack. `final: true` halts the entire
// traversal after the rule's own nested subtree has been visited.
func walkRules(rules []config.AlertmanagerMatchRule, alert Alert, env Envelope, vr *valueResolver) ([]stackEntry, ruleChain, bool) {
	var stack []stackEntry
	var chain ruleChain
	finalHit := false
	for i := range rules {
		if finalHit {
			break
		}
		halted := visitRule(&rules[i], alert, env, fmt.Sprintf("match[%d]", i), &stack, &chain, vr)
		if halted {
			finalHit = true
		}
	}
	return stack, chain, finalHit
}

// visitRule applies one rule against (alert, env). If it matches it
// pushes its config + ignore onto the stack, records its label in the
// chain, recurses into its nested children, and then — if
// final:true — signals the caller to halt the whole traversal.
// Returns true when the traversal should halt.
func visitRule(
	r *config.AlertmanagerMatchRule,
	alert Alert,
	env Envelope,
	label string,
	stack *[]stackEntry,
	chain *ruleChain,
	vr *valueResolver,
) (halt bool) {
	if !whenMatches(r.When, alert, env) {
		return false
	}
	step := label + selectorSummary(r.When)
	if r.Final {
		step += " [final]"
	}
	chain.push(step)
	var cfg config.AlertmanagerMatchConfig
	if r.Config != nil {
		cfg = vr.apply(label, *r.Config)
	}
	*stack = append(*stack, stackEntry{cfg: cfg, ignore: r.Ignore})

	for j := range r.Nested {
		nestedLabel := fmt.Sprintf("%s.nested[%d]", label, j)
		if visitRule(&r.Nested[j], alert, env, nestedLabel, stack, chain, vr) {
			return true
		}
	}
	return r.Final
}

// resolveStack collapses the merge stack into a single
// AlertmanagerMatchConfig per ADR-0005 §"Match tree" (mirrors
// ADR-0002 §Merge rules for the two fields v1 carries):
//
//   - Slack: deepest layer that set it wins; an empty string does
//     not override.
//   - Notify: union by default, deduped, shallow-first insertion
//     order. An Override=true layer flips to replace.
func resolveStack(stack []stackEntry) config.AlertmanagerMatchConfig {
	var out config.AlertmanagerMatchConfig
	for _, e := range stack {
		if e.cfg.Slack != "" {
			out.Slack = e.cfg.Slack
		}
		// A Notify list is "set" iff it has values OR is marked
		// Override (an explicit `!override []` clears ancestors). An
		// unset list at this layer must not clobber accumulated
		// values.
		if len(e.cfg.Notify.Values) > 0 || e.cfg.Notify.Override {
			out.Notify.Values = mergeStrings(out.Notify.Values, e.cfg.Notify.Values, e.cfg.Notify.Override)
		}
	}
	return out
}

// mergeStrings is the union-with-!override helper. Override=true
// discards accumulated values and starts fresh from the deeper
// layer's values; deeper layers after that continue to union on top.
// Same semantics and code shape as merger.mergeStrings — kept
// duplicated rather than lifted into a shared util because the two
// packages have no other coupling and ADR-0002 explicitly calls out
// the merge rules as a per-cascade contract.
func mergeStrings(accum, incoming []string, override bool) []string {
	if override {
		out := make([]string, 0, len(incoming))
		seen := map[string]struct{}{}
		for _, v := range incoming {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
		return out
	}
	out := make([]string, 0, len(accum)+len(incoming))
	seen := map[string]struct{}{}
	for _, v := range accum {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range incoming {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// whenMatches returns true iff every set field on w matches the
// (alert, envelope) pair. A nil or all-empty selector matches
// everything (the root baseline). Missing labels on the alert side
// mean the rule doesn't match for any non-empty selector on that
// key.
func whenMatches(w *config.AlertmanagerMatchWhen, alert Alert, env Envelope) bool {
	if w == nil {
		return true
	}
	if w.Alertname != "" {
		if !matchGlob(w.Alertname, alert.Labels["alertname"]) {
			return false
		}
	}
	if w.AlertnameRegex != "" {
		if !matchRegex(w.AlertnameRegex, alert.Labels["alertname"]) {
			return false
		}
	}
	for k, v := range w.Labels {
		if strings.HasSuffix(k, config.LabelRegexSuffix) && k != config.LabelRegexSuffix {
			bare := strings.TrimSuffix(k, config.LabelRegexSuffix)
			if !matchRegex(v, alert.Labels[bare]) {
				return false
			}
		} else if !matchGlob(v, alert.Labels[k]) {
			return false
		}
	}
	if w.Receiver != "" && w.Receiver != env.Receiver {
		return false
	}
	if w.ExternalURL != "" && w.ExternalURL != env.ExternalURL {
		return false
	}
	return true
}

// selectorSummary renders a compact, deterministic view of w for the
// rule-chain debug string. Empty / nil selector renders as "" (per
// ADR-0005: the root rule traces as "match[0]" with no parenthetical)
// — diverging here from merger.selectorSummary's " ()" stylistic
// choice, since the AM debug surface explicitly calls out the bare
// root form.
//
// Map iteration order is non-deterministic; sort the label keys so
// the chain string is reproducible across runs (the am_alerts
// rule_chain column is operator-visible and should not flap on every
// delivery).
func selectorSummary(w *config.AlertmanagerMatchWhen) string {
	if w == nil {
		return ""
	}
	parts := []string{}
	if w.Alertname != "" {
		parts = append(parts, "alertname="+w.Alertname)
	}
	if w.AlertnameRegex != "" {
		parts = append(parts, "alertnameRegex="+w.AlertnameRegex)
	}
	if len(w.Labels) > 0 {
		keys := make([]string, 0, len(w.Labels))
		for k := range w.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, "labels."+k+"="+w.Labels[k])
		}
	}
	if w.Receiver != "" {
		parts = append(parts, "receiver="+w.Receiver)
	}
	if w.ExternalURL != "" {
		parts = append(parts, "externalURL="+w.ExternalURL)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// matchGlob is the `*`-style matcher used for alertname + label
// glob values. `*` matches any run of non-`/` characters (path.Match
// semantics) — same convention as kube's namespace/host selectors.
func matchGlob(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if pattern == value {
		return true
	}
	ok, err := path.Match(pattern, value)
	if err != nil {
		return false
	}
	return ok
}

// regexCache memoizes compiled regex selectors keyed on the source
// string. Selectors are validated at config-load so compilation here
// cannot fail in practice; a compilation failure falls through as
// "no match" rather than panicking, mirroring merger.matchRegex.
//
// Package-level rather than per-Evaluator: the regex source strings
// come from config and stay stable for the binary's lifetime, so a
// single shared cache is the right scope; per-Evaluator caching would
// pay double for the same patterns when multiple call sites share a
// config tree.
var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

func matchRegex(pattern, value string) bool {
	regexCacheMu.Lock()
	re, ok := regexCache[pattern]
	if !ok {
		// Auto-anchor as ^...$ per ADR-0005 §"Match tree" — "acme"
		// matches "acme" exactly, not "acme-prod". If the operator
		// already wrote `^`/`$` the double-anchor is harmless
		// (`^^acme$$` is equivalent to `^acme$`).
		anchored := pattern
		if !strings.HasPrefix(anchored, "^") {
			anchored = "^" + anchored
		}
		if !strings.HasSuffix(anchored, "$") {
			anchored += "$"
		}
		compiled, err := regexp.Compile(anchored)
		if err != nil {
			regexCacheMu.Unlock()
			return false
		}
		re = compiled
		regexCache[pattern] = re
	}
	regexCacheMu.Unlock()
	return re.MatchString(value)
}
