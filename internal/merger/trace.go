package merger

import (
	"fmt"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// TraceAction names the kind of change one layer applied to one
// KubeConfig key. The renderer reads this to pick a visual treatment
// (set vs replace vs union-add vs override-discard).
type TraceAction string

const (
	// TraceSet is a scalar field set for the first time in the cascade.
	TraceSet TraceAction = "set"
	// TraceReplace is a scalar field whose prior value was overwritten,
	// or acceptedStatusCodes (replace-by-default list).
	TraceReplace TraceAction = "replace"
	// TraceAdd is a union-add into notify/tags/dependsOn (the
	// !override-aware list fields) with Override=false.
	TraceAdd TraceAction = "add"
	// TraceOverride is a union-list write with the !override YAML tag —
	// the prior accumulator is discarded before the new values land.
	TraceOverride TraceAction = "override"
)

// TraceEvent is one (rule, key) write captured during a traced walk.
//
// For scalars, OldValue/NewValue carry the human-readable rendering
// of the prior + new scalar (empty OldValue when Action == TraceSet).
//
// For list fields, OldList/NewList carry the accumulator before/after
// the write, Added carries the values this layer contributed, and
// Removed carries the values discarded by an !override.
type TraceEvent struct {
	Key    string
	Action TraceAction

	// Scalar fields.
	OldValue string
	NewValue string

	// List fields.
	OldList []string
	NewList []string
	Added   []string
	Removed []string
}

// RuleTrace bundles every TraceEvent emitted by one matched rule, plus
// the rule's identity (label like "match[1].nested[0]"), selector
// summary, and final flag. The renderer iterates these in match-order
// and groups events under each card.
type RuleTrace struct {
	Label  string // "match[0]", "match[1].nested[0]" — mirrors selectorSummary's prefix
	When   string // selectorSummary output: " (ns=acme-*)" or " ()"
	Final  bool
	Events []TraceEvent
}

// ResolveWithTrace walks the cascading kube.match[] tree against (ing,
// host) and returns the same outputs as Resolve plus a per-(rule, key)
// trace stream the operator UI uses to render the cascade. Behavior
// matches Resolve byte-for-byte; the trace is the only addition.
//
// The materializer hot path stays on the untraced Resolve / walk
// pair — building trace events allocates per matched (rule, key)
// touch, which we don't want on every reconcile.
func ResolveWithTrace(rules []config.KubeMatchRule, ing *networkingv1.Ingress, host string) (
	resolved config.KubeConfig,
	rules2 []RuleTrace,
	chain []string,
	ignored bool,
	matched bool,
	resolvedErr error,
) {
	stack, traces, ch := walkRulesTraced(rules, ing, host)
	chain = append([]string(nil), ch.steps...)
	rules2 = traces
	if len(stack) == 0 {
		return config.KubeConfig{}, rules2, chain, false, false, nil
	}
	for _, layer := range stack {
		if layer.ignore != nil {
			ignored = *layer.ignore
		}
	}
	resolved = resolveStack(stack)
	if !ignored {
		resolvedErr = checkResolved(resolved)
	}
	return resolved, rules2, chain, ignored, true, resolvedErr
}

// walkRulesTraced mirrors walkRules but accumulates a RuleTrace per
// matched rule alongside the merge stack. The traversal order +
// final-rule halt semantics are identical so the trace stays in
// lockstep with the chain that Resolve / Materialize emit.
func walkRulesTraced(rules []config.KubeMatchRule, ing *networkingv1.Ingress, host string) (
	[]stackEntry, []RuleTrace, ruleChain,
) {
	var stack []stackEntry
	var traces []RuleTrace
	var chain ruleChain
	acc := newTraceAccumulator()
	halted := false
	for i := range rules {
		if halted {
			break
		}
		halted = visitRuleTraced(&rules[i], ing, host, fmt.Sprintf("match[%d]", i), &stack, &traces, &chain, acc)
	}
	return stack, traces, chain
}

// visitRuleTraced applies one rule. When it matches, it records the
// rule's identity in chain + traces, then records one TraceEvent per
// KubeConfig key the rule's config block touched (using acc to track
// the running merge accumulator). ignore tri-state writes also emit a
// TraceEvent so the renderer can surface "this rule flipped ignore".
func visitRuleTraced(
	r *config.KubeMatchRule,
	ing *networkingv1.Ingress,
	host string,
	label string,
	stack *[]stackEntry,
	traces *[]RuleTrace,
	chain *ruleChain,
	acc *traceAccumulator,
) bool {
	if !whenMatches(r.When, ing, host) {
		return false
	}
	when := selectorSummary(r.When)
	step := label + when
	if r.Final {
		step += " [final]"
	}
	chain.push(step)

	rt := RuleTrace{Label: label, When: when, Final: r.Final}
	rt.Events = acc.applyConfig(r.Config, r.Ignore)
	*traces = append(*traces, rt)
	*stack = append(*stack, stackEntry{cfg: r.Config, ignore: r.Ignore})

	for j := range r.Nested {
		nestedLabel := fmt.Sprintf("%s.nested[%d]", label, j)
		if visitRuleTraced(&r.Nested[j], ing, host, nestedLabel, stack, traces, chain, acc) {
			return true
		}
	}
	return r.Final
}

// traceAccumulator mirrors the running KubeConfig the merger would
// build by reducing stackEntry layers, plus the !override-aware list
// values, plus the ignore tri-state. It is the source of truth for
// "what was the prior value" when emitting a TraceEvent. Behavior
// matches resolveStack exactly — anything that diverges is a bug.
type traceAccumulator struct {
	// Scalars: a presence flag + the value. Sentinels avoid
	// distinguishing "unset" from "explicitly set to zero".
	scheme                    string
	hasScheme                 bool
	path                      string
	hasPath                   bool
	httpMethod                string
	hasHTTPMethod             bool
	interval                  config.Duration
	hasInterval               bool
	timeout                   config.Duration
	hasTimeout                bool
	retries                   int
	hasRetries                bool
	retryBackoff              config.Duration
	hasRetryBackoff           bool
	followRedirects           bool
	hasFollowRedirects        bool
	tlsInsecureSkipVerify     bool
	hasTLSInsecureSkipVerify  bool
	proxy                     string
	hasProxy                  bool
	reminderInterval          config.Duration
	hasReminderInterval       bool
	sslAlertThreshold         config.Duration
	hasSSLAlertThreshold      bool
	sslEscalationThreshold    config.Duration
	hasSSLEscalationThreshold bool
	sslReminderInterval       config.Duration
	hasSSLReminderInterval    bool
	slack                     string
	hasSlack                  bool
	acceptedStatusCodes       []int
	hasAcceptedStatusCodes    bool
	notify, tags, dependsOn   []string
	ignore                    bool
	hasIgnore                 bool
}

func newTraceAccumulator() *traceAccumulator { return &traceAccumulator{} }

// applyConfig consumes one rule's config block (plus its ignore
// directive) and emits the trace events. The order of writes matches
// resolveStack's IsSet checks so the trace mirrors what the merger
// will see when it actually resolves.
func (a *traceAccumulator) applyConfig(c config.KubeConfig, ignore *bool) []TraceEvent {
	var ev []TraceEvent

	// Scalar string fields.
	if c.IsSet("scheme") {
		ev = append(ev, a.setString(&a.scheme, &a.hasScheme, "scheme", c.Scheme))
	}
	if c.IsSet("path") {
		ev = append(ev, a.setString(&a.path, &a.hasPath, "path", c.Path))
	}
	if c.IsSet("httpMethod") {
		ev = append(ev, a.setString(&a.httpMethod, &a.hasHTTPMethod, "httpMethod", c.HTTPMethod))
	}
	if c.IsSet("proxy") {
		ev = append(ev, a.setString(&a.proxy, &a.hasProxy, "proxy", c.Proxy))
	}
	if c.IsSet("slack") {
		ev = append(ev, a.setString(&a.slack, &a.hasSlack, "slack", c.Slack))
	}

	// Scalar duration fields.
	if c.IsSet("interval") {
		ev = append(ev, a.setDuration(&a.interval, &a.hasInterval, "interval", c.Interval))
	}
	if c.IsSet("timeout") {
		ev = append(ev, a.setDuration(&a.timeout, &a.hasTimeout, "timeout", c.Timeout))
	}
	if c.IsSet("retryBackoff") {
		ev = append(ev, a.setDuration(&a.retryBackoff, &a.hasRetryBackoff, "retryBackoff", c.RetryBackoff))
	}
	if c.IsSet("reminderInterval") {
		ev = append(ev, a.setDuration(&a.reminderInterval, &a.hasReminderInterval, "reminderInterval", c.ReminderInterval))
	}
	if c.IsSet("sslAlertThreshold") {
		ev = append(ev, a.setDuration(&a.sslAlertThreshold, &a.hasSSLAlertThreshold, "sslAlertThreshold", c.SSLAlertThreshold))
	}
	if c.IsSet("sslEscalationThreshold") {
		ev = append(ev, a.setDuration(&a.sslEscalationThreshold, &a.hasSSLEscalationThreshold, "sslEscalationThreshold", c.SSLEscalationThreshold))
	}
	if c.IsSet("sslReminderInterval") {
		ev = append(ev, a.setDuration(&a.sslReminderInterval, &a.hasSSLReminderInterval, "sslReminderInterval", c.SSLReminderInterval))
	}

	// Scalar numeric / bool fields.
	if c.IsSet("retries") {
		ev = append(ev, a.setInt(&a.retries, &a.hasRetries, "retries", c.Retries))
	}
	if c.IsSet("followRedirects") {
		ev = append(ev, a.setBool(&a.followRedirects, &a.hasFollowRedirects, "followRedirects", c.FollowRedirects))
	}
	if c.IsSet("tlsInsecureSkipVerify") {
		ev = append(ev, a.setBool(&a.tlsInsecureSkipVerify, &a.hasTLSInsecureSkipVerify, "tlsInsecureSkipVerify", c.TLSInsecureSkipVerify))
	}

	// acceptedStatusCodes: replace-by-default list. The trace renders
	// it identically to a scalar replace so the visual reads "this
	// layer threw away the prior list" without any !override tag noise.
	if c.IsSet("acceptedStatusCodes") {
		old := a.acceptedStatusCodes
		newList := append([]int(nil), c.AcceptedStatusCodes...)
		event := TraceEvent{
			Key:     "acceptedStatusCodes",
			Action:  TraceReplace,
			OldList: intsToStrings(old),
			NewList: intsToStrings(newList),
		}
		if !a.hasAcceptedStatusCodes {
			event.Action = TraceSet
		}
		a.acceptedStatusCodes = newList
		a.hasAcceptedStatusCodes = true
		ev = append(ev, event)
	}

	// Union-list fields with optional !override.
	if c.IsSet("notify") {
		ev = append(ev, a.applyList(&a.notify, "notify", c.Notify.Values, c.Notify.Override))
	}
	if c.IsSet("tags") {
		ev = append(ev, a.applyList(&a.tags, "tags", c.Tags.Values, c.Tags.Override))
	}
	if c.IsSet("dependsOn") {
		ev = append(ev, a.applyList(&a.dependsOn, "dependsOn", c.DependsOn.Values, c.DependsOn.Override))
	}

	// Ignore tri-state. nil layers inherit; deepest non-nil wins
	// (same semantics as Resolve). Emit an event only on a write.
	if ignore != nil {
		event := TraceEvent{
			Key:      "ignore",
			Action:   TraceSet,
			NewValue: strconv.FormatBool(*ignore),
		}
		if a.hasIgnore {
			event.Action = TraceReplace
			event.OldValue = strconv.FormatBool(a.ignore)
		}
		a.ignore = *ignore
		a.hasIgnore = true
		ev = append(ev, event)
	}

	return ev
}

// setString records a string scalar write. The first write per key in
// the cascade is TraceSet (no OldValue); subsequent writes are
// TraceReplace.
func (a *traceAccumulator) setString(field *string, has *bool, key, newVal string) TraceEvent {
	event := TraceEvent{Key: key, NewValue: newVal}
	if *has {
		event.Action = TraceReplace
		event.OldValue = *field
	} else {
		event.Action = TraceSet
	}
	*field = newVal
	*has = true
	return event
}

func (a *traceAccumulator) setDuration(field *config.Duration, has *bool, key string, newVal config.Duration) TraceEvent {
	event := TraceEvent{Key: key, NewValue: newVal.String()}
	if *has {
		event.Action = TraceReplace
		event.OldValue = field.String()
	} else {
		event.Action = TraceSet
	}
	*field = newVal
	*has = true
	return event
}

func (a *traceAccumulator) setInt(field *int, has *bool, key string, newVal int) TraceEvent {
	event := TraceEvent{Key: key, NewValue: strconv.Itoa(newVal)}
	if *has {
		event.Action = TraceReplace
		event.OldValue = strconv.Itoa(*field)
	} else {
		event.Action = TraceSet
	}
	*field = newVal
	*has = true
	return event
}

func (a *traceAccumulator) setBool(field *bool, has *bool, key string, newVal bool) TraceEvent {
	event := TraceEvent{Key: key, NewValue: strconv.FormatBool(newVal)}
	if *has {
		event.Action = TraceReplace
		event.OldValue = strconv.FormatBool(*field)
	} else {
		event.Action = TraceSet
	}
	*field = newVal
	*has = true
	return event
}

// applyList computes the trace event + new accumulator value for a
// union-list write. With override=false it dedups + appends new
// values (TraceAdd); with override=true it discards the prior
// accumulator and starts from the incoming values (TraceOverride).
// Matches mergeStrings's semantics exactly — the accumulator and the
// resolved value must agree for the trace to be honest.
func (a *traceAccumulator) applyList(field *[]string, key string, incoming []string, override bool) TraceEvent {
	old := append([]string(nil), *field...)
	if override {
		// Dedup-incoming, preserving order (matches mergeStrings).
		out := make([]string, 0, len(incoming))
		seen := map[string]struct{}{}
		for _, v := range incoming {
			if _, dup := seen[v]; dup {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
		removed := diffStrings(old, out)
		added := diffStrings(out, old)
		*field = out
		return TraceEvent{
			Key:     key,
			Action:  TraceOverride,
			OldList: old,
			NewList: append([]string(nil), out...),
			Added:   added,
			Removed: removed,
		}
	}
	out := make([]string, 0, len(old)+len(incoming))
	seen := map[string]struct{}{}
	for _, v := range old {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	added := make([]string, 0, len(incoming))
	for _, v := range incoming {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		added = append(added, v)
	}
	*field = out
	return TraceEvent{
		Key:     key,
		Action:  TraceAdd,
		OldList: old,
		NewList: append([]string(nil), out...),
		Added:   added,
	}
}

// diffStrings returns the elements of `a` not present in `b`, in
// order. Used to populate Added/Removed when an !override write
// discards a subset of the prior accumulator.
func diffStrings(a, b []string) []string {
	if len(a) == 0 {
		return nil
	}
	bset := make(map[string]struct{}, len(b))
	for _, v := range b {
		bset[v] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if _, in := bset[v]; in {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// intsToStrings renders an []int as []string for the trace's OldList /
// NewList fields. Used by acceptedStatusCodes.
func intsToStrings(in []int) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.Itoa(v)
	}
	return out
}

// FormatTraceList joins a string list as "[a, b, c]" for compact
// inline rendering. Empty list renders as "[]" so the reader can
// distinguish "no values" from "unset".
func FormatTraceList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return "[" + strings.Join(values, ", ") + "]"
}
