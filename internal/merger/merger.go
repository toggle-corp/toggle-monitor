// Package merger walks the cascading kube.match[] tree against a
// discovered Ingress + host, accumulates the matching rules' config
// blocks into a merge stack, resolves them according to the rules in
// ADR-0002 §Merge rules, and produces:
//
//   - a discovery snapshot row (status = added | kube-ignored | kube-invalid)
//   - a scheduler.Plan (when the resolved monitor materializes cleanly)
//   - a reconciled monitor row in the database
//
// It implements kube.Materializer so the watcher can stay free of
// merge plumbing.
package merger

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/httpcheck"
	"github.com/toggle-corp/toggle-monitor/internal/proxypool"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/slug"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// MonitorStore is the slim seam the materializer needs to detect
// static-vs-kube slug collisions and to upsert kube-discovered
// monitors. MarkTemporaryPaused is retained for parity with the
// scheduler's pause callback but the new merger does not call it
// (kube.pause is gone — operators express pauses with ignore: true).
type MonitorStore interface {
	GetMonitor(ctx context.Context, slug string) (store.MonitorRow, error)
	ReconcileMonitor(ctx context.Context, spec store.MonitorSpec) error
	MarkTemporaryPaused(ctx context.Context, slug string) error
}

// Materializer drives the per-(Ingress, host) walk over the cascading
// kube.match tree. For each pair it builds a merge stack of matching
// rules, resolves them, and either records a discovery row + emits a
// scheduler plan, or records an ignore/invalid row with the rule chain
// in the reason field.
type Materializer struct {
	store             MonitorStore
	match             []config.KubeMatchRule
	staticSlugs       map[string]struct{}
	friendlyNameStyle string

	// httpClientUA + slack.UserMapping carry through into the Plan.
	// proxies resolves preset proxy slugs to the pre-built socks
	// dialers.
	userAgent   string
	userMapping map[string]string
	bodyMaxBase int
	proxies     *proxypool.Pool

	// slackChannels is the roster an annotation-sourced `slack` value
	// must select from; nsSource reads Namespace annotations for
	// namespaceAnnotation-scoped sources (nil until the watcher's
	// informer is wired, and harmless — those sources fall back to
	// their defaults).
	slackChannels map[string]struct{}
	nsSource      NamespaceAnnotationSource
	log           *slog.Logger

	mu        sync.RWMutex
	kubePlans map[string]scheduler.Plan
	// annotationWarnings holds the last pass's rejected annotation
	// values per materialized monitor, for /issues and the issues
	// gauge. Keyed by monitor slug so a reconcile overwrites rather
	// than accumulates.
	annotationWarnings map[string]MonitorWarnings
}

// NamespaceAnnotationSource resolves a namespace's annotations.
// Production wires the kube watcher's Namespace informer; a nil source
// means namespaceAnnotation-scoped values simply resolve to their
// defaults.
type NamespaceAnnotationSource interface {
	NamespaceAnnotations(namespace string) map[string]string
}

// MonitorWarnings is one monitor's rejected annotation values, with
// enough identity to link back to the discovery row.
type MonitorWarnings struct {
	Slug        string
	Namespace   string
	IngressName string
	Host        string
	Warnings    []Warning
}

// New builds a Materializer from the loaded YAML. staticSlugs is the
// set of slugs declared in config.Monitors — used to detect kube ↔
// static collisions. proxies is the pre-resolved proxy pool; nil is
// acceptable when no proxies are configured.
func New(s MonitorStore, cfg config.Config, proxies *proxypool.Pool) *Materializer {
	if cfg.Kube == nil {
		return nil
	}
	statics := make(map[string]struct{}, len(cfg.Monitors))
	for _, m := range cfg.Monitors {
		statics[m.Slug] = struct{}{}
	}
	style := cfg.Kube.FriendlyName
	if style == "" {
		style = config.KubeFriendlyNameCompact
	}
	channels := make(map[string]struct{}, len(cfg.Slack.Channels))
	for _, ch := range cfg.Slack.Channels {
		channels[ch.Slug] = struct{}{}
	}
	return &Materializer{
		store:              s,
		match:              cfg.Kube.Match,
		staticSlugs:        statics,
		friendlyNameStyle:  style,
		userAgent:          cfg.HTTPClient.UserAgent,
		userMapping:        cfg.Slack.UserMapping,
		bodyMaxBase:        cfg.Slack.BodyMaxChars,
		proxies:            proxies,
		slackChannels:      channels,
		log:                slog.Default(),
		kubePlans:          map[string]scheduler.Plan{},
		annotationWarnings: map[string]MonitorWarnings{},
	}
}

// SetNamespaceAnnotationSource plugs in the Namespace informer. The
// watcher owns the informer and is constructed after the materializer,
// so this is a setter rather than a New parameter.
func (m *Materializer) SetNamespaceAnnotationSource(src NamespaceAnnotationSource) {
	m.nsSource = src
}

// SetLogger overrides the destination for annotation-rejection WARN
// lines. Production passes the lifecycle's logger.
func (m *Materializer) SetLogger(l *slog.Logger) {
	if l != nil {
		m.log = l
	}
}

// ResolveEnv builds the annotation environment for one namespace. The
// discovery detail page uses it to re-run the cascade against exactly
// the inputs the daemon would see.
func (m *Materializer) ResolveEnv(namespace string) Env {
	var nsAnnotations map[string]string
	if m.nsSource != nil {
		nsAnnotations = m.nsSource.NamespaceAnnotations(namespace)
	}
	return Env{
		NamespaceAnnotations: nsAnnotations,
		UserMapping:          m.userMapping,
		SlackChannels:        m.slackChannels,
	}
}

// recordWarnings stores this pass's rejected annotation values for the
// monitor and logs each one. WARN, not Sentry — a bad annotation is app
// team input error, not a toggle-monitor fault.
func (m *Materializer) recordWarnings(ing *networkingv1.Ingress, host, monSlug string, warnings []Warning) {
	m.mu.Lock()
	if len(warnings) == 0 {
		delete(m.annotationWarnings, monSlug)
	} else {
		m.annotationWarnings[monSlug] = MonitorWarnings{
			Slug:        monSlug,
			Namespace:   ing.Namespace,
			IngressName: ing.Name,
			Host:        host,
			Warnings:    warnings,
		}
	}
	m.mu.Unlock()

	for _, w := range warnings {
		m.log.Warn("kube annotation value rejected",
			"ns", ing.Namespace, "ingress", ing.Name, "host", host,
			"field", w.Field, "scope", w.Scope, "annotation", w.Key,
			"value", w.Value, "reason", w.Reason)
	}
}

// AnnotationWarnings returns the rejected annotation values from the
// last reconcile, one entry per affected monitor. Read by /issues and
// by the issues gauge.
func (m *Materializer) AnnotationWarnings() []MonitorWarnings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MonitorWarnings, 0, len(m.annotationWarnings))
	for _, mw := range m.annotationWarnings {
		out = append(out, mw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// Resolution is everything one cascade walk produces for a single
// (Ingress, host) pair.
//
// Chain is in match-order (root → leaf), with the same formatting
// Materialize emits in the discovery reason field (one step per matched
// rule, with selectorSummary appended and a trailing " [final]" when
// the rule halted the cascade).
//
// Ignored is the deepest-wins resolution of rule-level ignore:
// directives across the matched stack. Err is the resolved-value
// validation error (interval > timeout, sslAlert > escalation > 0,
// required-at-root fields not regressed) — non-nil means the resolved
// config would emit a kube-invalid discovery row rather than
// materialize. Matched is false when no rule in the tree fired (only
// possible against a hand-constructed tree without a root rule;
// production configs always carry a matching root).
//
// Provenance and Warnings cover the ADR-0009 annotation inputs and are
// empty for a tree that uses only literals.
type Resolution struct {
	Config     config.KubeConfig
	Chain      []string
	Provenance []Provenance
	Warnings   []Warning
	Ignored    bool
	Matched    bool
	Err        error
}

// Resolve walks the cascading kube.match[] tree against (ing, host)
// and returns the merge stack's resolved value plus the rule chain
// summary, without any DB side effects. It is the shared kernel
// behind Materializer.Materialize and the `toggle-monitor explain`
// CLI subcommand — keep them in lockstep by routing every consumer
// through this helper rather than re-implementing the tree walk.
func Resolve(rules []config.KubeMatchRule, ing *networkingv1.Ingress, host string, env Env) Resolution {
	vr := newValueResolver(ing.Annotations, env)
	stack, ch := walkRules(rules, ing, host, vr)
	out := Resolution{
		Chain:      append([]string(nil), ch.steps...),
		Provenance: vr.provenance,
		Warnings:   vr.warnings,
	}
	if len(stack) == 0 {
		return out
	}
	out.Matched = true
	for _, layer := range stack {
		if layer.ignore != nil {
			out.Ignored = *layer.ignore
		}
	}
	out.Config = resolveStack(stack)
	if !out.Ignored {
		out.Err = checkResolved(out.Config)
	}
	return out
}

// Materialize implements kube.Materializer. For one (Ingress, host)
// pair it produces a snapshot row carrying the rule chain (in the
// reason field) and — for non-ignored, non-invalid rows — also
// reconciles the materialized monitor and stashes a scheduler.Plan
// for the dynamic refresh loop to pick up.
func (m *Materializer) Materialize(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error) {
	base := store.DiscoverySnapshotRow{
		Namespace:   ing.Namespace,
		IngressName: ing.Name,
		Host:        host,
	}

	// Wildcard guard — runs BEFORE the cascade walk. A wildcard ingress
	// host (k8s only permits `*` as the leftmost label, e.g.
	// `*.foo.example.test`) is not a resolvable name: probing
	// https://*.foo/ yields a perpetual "no such host". Such a host is
	// structurally unprobeable regardless of which match rules it would
	// hit, so it must short-circuit to kube-invalid before any ignore
	// or override rule can reclassify it. ContainsRune is a deliberate
	// superset of the `*.`-prefix form — cheap, and robust to fake
	// listers and the no-materializer fallback path.
	if strings.ContainsRune(host, '*') {
		reason := "kube-invalid: wildcard host not probeable"
		base.Status, base.Reason = "kube-invalid", &reason
		return base, nil
	}

	res := Resolve(m.match, ing, host, m.ResolveEnv(ing.Namespace))
	chainStr := strings.Join(res.Chain, " → ")
	// The root rule always matches (validator enforces an empty
	// when: at index 0), so stack is never empty in production. Guard
	// regardless — tests can pass an empty match[].
	if !res.Matched {
		reason := "no matching kube.match rule"
		base.Status, base.Reason = "kube-invalid", &reason
		return base, nil
	}

	// Annotation warnings ride along on every subsequent reason so the
	// operator sees a rejected value wherever the row surfaces, and the
	// monitor still materializes as `added` — a typo in an app team's
	// chart must not cost availability monitoring.
	suffix := annotationSuffix(res)

	if res.Ignored {
		reason := "kube-ignored: " + chainStr + suffix
		base.Status, base.Reason = "kube-ignored", &reason
		return base, nil
	}

	resolved := res.Config

	// Resolved-value validation per ADR-0002 §Validation. Children may
	// have overridden a root-required field to something invalid — the
	// merger catches that at materialization time and emits a
	// kube-invalid row instead of silently materializing garbage.
	if res.Err != nil {
		reason := "kube-invalid: " + res.Err.Error() + " (" + chainStr + ")" + suffix
		base.Status, base.Reason = "kube-invalid", &reason
		return base, nil
	}

	monSlug, slugErr := slug.SanitizeKubeDiscovered(ing.Namespace, ing.Name, host)
	if slugErr != nil {
		reason := "slug generation failed: " + slugErr.Error()
		base.Status, base.Reason = "kube-invalid", &reason
		return base, nil
	}
	if _, conflict := m.staticSlugs[monSlug]; conflict {
		reason := "slug conflicts with static monitor"
		base.Status, base.Reason = "kube-invalid", &reason
		return base, nil
	}

	scheme := resolved.Scheme
	if scheme == "" {
		scheme = "https"
	}
	friendly := m.friendlyName(ing, host)
	url := buildURL(scheme, host, resolved.Path)

	if err := m.store.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug:             monSlug,
		Kind:             store.KindHTTP,
		FriendlyName:     friendly,
		URL:              url,
		Source:           store.SourceKube,
		DependsOn:        append([]string(nil), resolved.DependsOn.Values...),
		Tags:             append([]string(nil), resolved.Tags.Values...),
		SlackChannelSlug: resolved.Slack,
	}); err != nil {
		return base, fmt.Errorf("reconcile kube monitor: %w", err)
	}

	mentions := slack.ResolveMentions(resolved.Notify.Values, m.userMapping)
	plan := scheduler.Plan{
		Slug:                monSlug,
		Kind:                "http",
		FriendlyName:        friendly,
		URL:                 url,
		HTTPMethod:          resolved.HTTPMethod,
		AcceptedStatusCodes: append([]int(nil), resolved.AcceptedStatusCodes...),
		Prober: httpcheck.Config{
			URL:                   url,
			Method:                resolved.HTTPMethod,
			AcceptedStatusCodes:   append([]int(nil), resolved.AcceptedStatusCodes...),
			Timeout:               resolved.Timeout.AsDuration(),
			FollowRedirects:       resolved.FollowRedirects,
			TLSInsecureSkipVerify: resolved.TLSInsecureSkipVerify,
			ProxyDialer:           m.proxies.Get(resolved.Proxy),
			UserAgent:             m.userAgent,
		},
		Interval:               resolved.Interval.AsDuration(),
		Timeout:                resolved.Timeout.AsDuration(),
		Retries:                resolved.Retries,
		RetryBackoff:           resolved.RetryBackoff.AsDuration(),
		FollowRedirects:        resolved.FollowRedirects,
		TLSInsecureSkipVerify:  resolved.TLSInsecureSkipVerify,
		ProxyDialer:            m.proxies.Get(resolved.Proxy),
		Proxy:                  resolved.Proxy,
		UserAgent:              m.userAgent,
		ReminderInterval:       resolved.ReminderInterval.AsDuration(),
		ChannelSlug:            resolved.Slack,
		Mentions:               mentions,
		DependsOn:              append([]string(nil), resolved.DependsOn.Values...),
		TLSBearing:             scheme == "https",
		SSLAlertThreshold:      resolved.SSLAlertThreshold.AsDuration(),
		SSLEscalationThreshold: resolved.SSLEscalationThreshold.AsDuration(),
		SSLReminderInterval:    resolved.SSLReminderInterval.AsDuration(),
	}
	m.mu.Lock()
	m.kubePlans[monSlug] = plan
	m.mu.Unlock()

	m.recordWarnings(ing, host, monSlug, res.Warnings)

	reason := "added: " + chainStr + suffix
	base.Status, base.Reason, base.MonitorSlug = "added", &reason, &monSlug
	return base, nil
}

// annotationSuffix renders the provenance and warning tails appended to
// a discovery row's reason. Empty when the resolved monitor used no
// annotation input.
func annotationSuffix(res Resolution) string {
	var sb strings.Builder
	for _, p := range res.Provenance {
		sb.WriteString(" | ")
		sb.WriteString(p.String())
	}
	for _, w := range res.Warnings {
		sb.WriteString(" | warn: ")
		sb.WriteString(w.String())
	}
	return sb.String()
}

// stackEntry is one layer in the merge stack: the rule's config and
// its ignore directive, paired so resolveStack can apply both.
type stackEntry struct {
	cfg    config.KubeConfig
	ignore *bool
}

// ruleChain captures the (ordered) sequence of matched rules so the
// discovery snapshot's reason field can name them. Stored as a slice
// of strings so callers can join with whatever separator they like
// (the default String() uses " → ").
type ruleChain struct {
	steps []string
}

func (c *ruleChain) push(s string) { c.steps = append(c.steps, s) }
func (c ruleChain) String() string { return strings.Join(c.steps, " → ") }

// walkRules traverses the given kube.match[] tree depth-first in
// document order, collecting every rule whose `when:` matches the
// (Ingress, host) pair into the merge stack. `final: true` halts the
// traversal after descending into its own nested children. Returns
// the merge stack in match order plus the rule-chain summary for the
// discovery reason.
//
// vr lowers each matched rule's `*From` blocks before its config
// reaches the stack, so the stack carries literals only.
func walkRules(rules []config.KubeMatchRule, ing *networkingv1.Ingress, host string, vr *valueResolver) ([]stackEntry, ruleChain) {
	var stack []stackEntry
	var chain ruleChain
	halted := false
	for i := range rules {
		if halted {
			break
		}
		halted = visitRule(&rules[i], ing, host, fmt.Sprintf("match[%d]", i), &stack, &chain, vr)
	}
	return stack, chain
}

// visitRule applies one rule against (ing, host). If it matches it
// pushes its config + ignore onto the stack, records its label in the
// chain, recurses into its nested children, and then — if final:true
// — signals the caller to halt the whole traversal. Returns true when
// the traversal should halt.
func visitRule(
	r *config.KubeMatchRule,
	ing *networkingv1.Ingress,
	host string,
	label string,
	stack *[]stackEntry,
	chain *ruleChain,
	vr *valueResolver,
) (halt bool) {
	if !whenMatches(r.When, ing, host) {
		return false
	}
	step := label + selectorSummary(r.When)
	if r.Final {
		step += " [final]"
	}
	chain.push(step)
	*stack = append(*stack, stackEntry{cfg: vr.apply(label, r.Config), ignore: r.Ignore})

	for j := range r.Nested {
		nestedLabel := fmt.Sprintf("%s.nested[%d]", label, j)
		if visitRule(&r.Nested[j], ing, host, nestedLabel, stack, chain, vr) {
			// A descendant fired final:true → halt the whole walk.
			return true
		}
	}
	return r.Final
}

// resolveStack collapses the merge stack into a single KubeConfig per
// the rules in ADR-0002 §Merge rules:
//
//   - Scalars: deepest layer that set the field wins.
//   - NotifyList / TagList / DependsOnList: union by default, deduped,
//     shallow-first. An Override=true layer flips to replace — that
//     layer (with its own values) becomes the new baseline; deeper
//     layers continue to union on top.
//   - StatusCodeList (acceptedStatusCodes): replace-by-default. Each
//     layer that sets it replaces the prior; deepest wins.
func resolveStack(stack []stackEntry) config.KubeConfig {
	var out config.KubeConfig
	out.AcceptedStatusCodes = nil

	for _, e := range stack {
		c := e.cfg
		// Scalar fields: copy-on-set.
		if c.IsSet("scheme") {
			out.Scheme = c.Scheme
		}
		if c.IsSet("path") {
			out.Path = c.Path
		}
		if c.IsSet("httpMethod") {
			out.HTTPMethod = c.HTTPMethod
		}
		if c.IsSet("interval") {
			out.Interval = c.Interval
		}
		if c.IsSet("timeout") {
			out.Timeout = c.Timeout
		}
		if c.IsSet("retries") {
			out.Retries = c.Retries
		}
		if c.IsSet("retryBackoff") {
			out.RetryBackoff = c.RetryBackoff
		}
		if c.IsSet("followRedirects") {
			out.FollowRedirects = c.FollowRedirects
		}
		if c.IsSet("tlsInsecureSkipVerify") {
			out.TLSInsecureSkipVerify = c.TLSInsecureSkipVerify
		}
		if c.IsSet("proxy") {
			out.Proxy = c.Proxy
		}
		if c.IsSet("reminderInterval") {
			out.ReminderInterval = c.ReminderInterval
		}
		if c.IsSet("sslAlertThreshold") {
			out.SSLAlertThreshold = c.SSLAlertThreshold
		}
		if c.IsSet("sslEscalationThreshold") {
			out.SSLEscalationThreshold = c.SSLEscalationThreshold
		}
		if c.IsSet("sslReminderInterval") {
			out.SSLReminderInterval = c.SSLReminderInterval
		}
		if c.IsSet("slack") {
			out.Slack = c.Slack
		}

		// acceptedStatusCodes: replace-by-default.
		if c.IsSet("acceptedStatusCodes") {
			out.AcceptedStatusCodes = append(config.StatusCodeList(nil), c.AcceptedStatusCodes...)
		}

		// Union-by-default array fields.
		if c.IsSet("notify") {
			out.Notify.Values = mergeStrings(out.Notify.Values, c.Notify.Values, c.Notify.Override)
		}
		if c.IsSet("tags") {
			out.Tags.Values = mergeStrings(out.Tags.Values, c.Tags.Values, c.Tags.Override)
		}
		if c.IsSet("dependsOn") {
			out.DependsOn.Values = mergeStrings(out.DependsOn.Values, c.DependsOn.Values, c.DependsOn.Override)
		}
	}
	return out
}

// mergeStrings is the union-with-!override helper used by every
// overridable list field. Override=true discards the accumulated
// values and starts fresh from the deeper layer's values; deeper
// layers after that continue to union on top.
//
// Deduplication is order-preserving (shallow-first); duplicates from
// the deeper layer are dropped.
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

// checkResolved enforces the resolved-value invariants from ADR-0002
// §Validation. These are the checks that depend on which Ingresses
// actually exist at materialization time (i.e. they can only fire
// against a fully-merged config). Returns a human-readable error
// message used in the kube-invalid discovery row's reason.
func checkResolved(r config.KubeConfig) error {
	// Every required-at-root field must still carry a usable value
	// after the merge. A descendant that overrode `slack: ""` or
	// `httpMethod: ""` regresses to an unmonitorable state.
	for _, key := range config.KubeRequiredAtRoot {
		switch key {
		case "path":
			if r.Path == "" || !strings.HasPrefix(r.Path, "/") {
				return fmt.Errorf("path must start with '/' (got %q)", r.Path)
			}
		case "httpMethod":
			if !validHTTPMethod(r.HTTPMethod) {
				return fmt.Errorf("httpMethod %q is not one of GET/HEAD/POST/PUT/DELETE", r.HTTPMethod)
			}
		case "acceptedStatusCodes":
			if len(r.AcceptedStatusCodes) == 0 {
				return fmt.Errorf("acceptedStatusCodes must not be empty after merge")
			}
		case "interval":
			if r.Interval.AsDuration() <= 0 {
				return fmt.Errorf("interval must be > 0 (got %s)", r.Interval)
			}
		case "timeout":
			if r.Timeout.AsDuration() <= 0 {
				return fmt.Errorf("timeout must be > 0 (got %s)", r.Timeout)
			}
		case "retries":
			if r.Retries < 0 {
				return fmt.Errorf("retries must be >= 0 (got %d)", r.Retries)
			}
		case "retryBackoff":
			if r.RetryBackoff.AsDuration() <= 0 {
				return fmt.Errorf("retryBackoff must be > 0 (got %s)", r.RetryBackoff)
			}
		case "followRedirects":
			// Boolean — both values are legitimate.
		case "reminderInterval":
			if r.ReminderInterval.AsDuration() <= 0 {
				return fmt.Errorf("reminderInterval must be > 0 (got %s)", r.ReminderInterval)
			}
		case "sslAlertThreshold":
			if r.SSLAlertThreshold.AsDuration() <= 0 {
				return fmt.Errorf("sslAlertThreshold must be > 0 (got %s)", r.SSLAlertThreshold)
			}
		case "sslEscalationThreshold":
			if r.SSLEscalationThreshold.AsDuration() <= 0 {
				return fmt.Errorf("sslEscalationThreshold must be > 0 (got %s)", r.SSLEscalationThreshold)
			}
		case "sslReminderInterval":
			if r.SSLReminderInterval.AsDuration() <= 0 {
				return fmt.Errorf("sslReminderInterval must be > 0 (got %s)", r.SSLReminderInterval)
			}
		case "slack":
			if r.Slack == "" {
				return fmt.Errorf("slack must not be empty after merge")
			}
		}
	}

	// Cross-field: interval > timeout.
	if r.Timeout.AsDuration() >= r.Interval.AsDuration() {
		return fmt.Errorf("interval (%s) must be greater than timeout (%s)",
			r.Interval, r.Timeout)
	}

	// SSL: alert > escalation > 0 (only meaningful when scheme is
	// HTTPS; defaults to https when unset, so apply unconditionally —
	// resolved.scheme=="http" still keeps the invariant from being
	// violated for no good reason, and an explicit scheme=http monitor
	// is fine since the validator allows it).
	scheme := r.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if scheme == "https" {
		alert := r.SSLAlertThreshold.AsDuration()
		esc := r.SSLEscalationThreshold.AsDuration()
		if esc <= 0 {
			return fmt.Errorf("sslEscalationThreshold must be > 0 for HTTPS monitors (got %s)", r.SSLEscalationThreshold)
		}
		if alert <= esc {
			return fmt.Errorf("sslAlertThreshold (%s) must be greater than sslEscalationThreshold (%s)",
				r.SSLAlertThreshold, r.SSLEscalationThreshold)
		}
	}
	return nil
}

func validHTTPMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "DELETE":
		return true
	}
	return false
}

// whenMatches returns true iff every set field on w matches the
// Ingress + host. Empty selector ({}) matches everything.
func whenMatches(w config.KubeMatchWhen, ing *networkingv1.Ingress, host string) bool {
	if w.Namespace != "" && !matchGlob(w.Namespace, ing.Namespace) {
		return false
	}
	if w.NamespaceRegex != "" && !matchRegex(w.NamespaceRegex, ing.Namespace) {
		return false
	}
	if w.Host != "" && !matchGlob(w.Host, host) {
		return false
	}
	if w.HostRegex != "" && !matchRegex(w.HostRegex, host) {
		return false
	}
	if len(w.Labels) > 0 {
		for k, v := range w.Labels {
			if ing.Labels[k] != v {
				return false
			}
		}
	}
	return true
}

// selectorSummary renders a compact, human-readable view of w for the
// rule-chain string. Empty selector renders as " ()" so the rule
// label still ends in a parenthesised summary for visual rhythm.
//
// Map iteration order is non-deterministic; sort the label keys so
// the chain string is reproducible across runs (the operator-facing
// /discovery view should not flap on every reconcile).
func selectorSummary(w config.KubeMatchWhen) string {
	parts := []string{}
	if w.Namespace != "" {
		parts = append(parts, "ns="+w.Namespace)
	}
	if w.NamespaceRegex != "" {
		parts = append(parts, "nsRegex="+w.NamespaceRegex)
	}
	if w.Host != "" {
		parts = append(parts, "host="+w.Host)
	}
	if w.HostRegex != "" {
		parts = append(parts, "hostRegex="+w.HostRegex)
	}
	if len(w.Labels) > 0 {
		keys := make([]string, 0, len(w.Labels))
		for k := range w.Labels {
			keys = append(keys, k)
		}
		// stdlib sort — deterministic chain across reconciles.
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, "labels."+k+"="+w.Labels[k])
		}
	}
	if len(parts) == 0 {
		return " ()"
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// CurrentPlans returns the scheduler plans for every currently
// materialized kube monitor. Called by lifecycle.planSource on each
// scheduler refresh.
func (m *Materializer) CurrentPlans() []scheduler.Plan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]scheduler.Plan, 0, len(m.kubePlans))
	for _, p := range m.kubePlans {
		out = append(out, p)
	}
	return out
}

// Prune drops every cached plan whose slug is NOT in `observed`. The
// kube.Watcher calls this at the end of every reconcile pass with the
// set of slugs that materialized successfully, so disappeared (or
// newly kube-ignored / kube-invalid) ingresses stop being scheduled.
//
// This is observed-set based by design — the prior timestamp-watermark
// version mirrored the snapshot prune's clock-mismatch bug and could
// drop plans for monitors whose ingresses were still in the cluster.
func (m *Materializer) Prune(observed map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for slug := range m.kubePlans {
		if _, ok := observed[slug]; !ok {
			delete(m.kubePlans, slug)
		}
	}
}

// matchGlob is a permissive `*`-style matcher used by kube.match[]
// for namespace + host conditions. `*` matches any run of non-`/`
// characters (path.Match semantics).
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

// regexCache memoizes compiled selector regexes. Selectors are
// validated at config-load so compilation here can't fail in
// practice; the regex value is keyed on the source string so two
// rules with identical regexes share the same *Regexp.
var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

func matchRegex(pattern, value string) bool {
	regexCacheMu.Lock()
	re, ok := regexCache[pattern]
	if !ok {
		// Auto-anchor as ^...$ per ADR-0002 §Selector vocabulary —
		// "acme-\d+" matches "acme-1" but not "acme-1-foo".
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

func buildURL(scheme, host, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return scheme + "://" + host + path
}

// friendlyName renders a human-skimmable label for a kube monitor.
// The exact style is picked by kube.friendlyName in config; see
// formatFriendlyName for the rules.
//
// When the source ingress carries multiple distinct hosts, the
// first segment of the current host is appended (after a middle-dot)
// so the listing differentiates monitors that share an ingress.
func (m *Materializer) friendlyName(ing *networkingv1.Ingress, host string) string {
	return formatFriendlyName(m.friendlyNameStyle, ing.Namespace, ing.Name, host, hasMultipleHosts(ing))
}

// formatFriendlyName is the pure renderer — kept separate from the
// Materializer so style tests don't need DB plumbing.
func formatFriendlyName(style, namespace, ingName, host string, multiHost bool) string {
	appendHost := func(s string) string {
		if !multiHost {
			return s
		}
		seg := hostFirstSegment(host)
		if seg == "" {
			return s
		}
		return s + " · " + seg
	}
	switch style {
	case config.KubeFriendlyNamePlain:
		return appendHost("(" + namespace + ") " + ingName)

	case config.KubeFriendlyNameDedupe:
		name := stripNsPrefix(namespace, stripIngressSuffix(ingName))
		out := "(" + namespace + ")"
		if name != "" {
			out += " " + name
		}
		return appendHost(out)

	case config.KubeFriendlyNameTitle:
		name := stripNsPrefix(namespace, stripIngressSuffix(ingName))
		out := "(" + titleize(namespace) + ")"
		if name != "" {
			out += " " + titleize(name)
		}
		if multiHost {
			if seg := hostFirstSegment(host); seg != "" {
				out += " · " + titleize(seg)
			}
		}
		return out

	default: // "" or "compact"
		name := stripIngressSuffix(ingName)
		if name == "" {
			name = ingName
		}
		return appendHost("(" + namespace + ") " + name)
	}
}

// stripIngressSuffix drops a trailing "-ingress" so the boilerplate
// kube convention doesn't drown the meaningful part.
func stripIngressSuffix(name string) string {
	return strings.TrimSuffix(name, "-ingress")
}

// stripNsPrefix drops leading hyphen-separated tokens from `name`
// that also appear (as standalone tokens) in `namespace`. This is
// looser than `HasPrefix(name, ns+"-")` so e.g. ingress
// `core-backend-1-api` in namespace `betaco-core-backend-1` reduces
// to `api` (the leading core, backend, 1 tokens are all present
// in the namespace token set).
func stripNsPrefix(namespace, name string) string {
	if namespace == "" || name == "" {
		return name
	}
	nsTokens := map[string]struct{}{}
	for _, t := range strings.Split(namespace, "-") {
		if t != "" {
			nsTokens[t] = struct{}{}
		}
	}
	nameTokens := strings.Split(name, "-")
	cut := 0
	for cut < len(nameTokens) {
		if _, ok := nsTokens[nameTokens[cut]]; !ok {
			break
		}
		cut++
	}
	return strings.Join(nameTokens[cut:], "-")
}

// titleize splits on '-' and title-cases each token, joined by spaces.
// "betaco-core-backend-1" → "Betaco Core Backend 1".
func titleize(s string) string {
	if s == "" {
		return s
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// hostFirstSegment returns the portion of host before the first dot.
func hostFirstSegment(host string) string {
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}

// hasMultipleHosts returns true when ing.Spec.Rules carries more
// than one distinct non-empty host. Mirrors kube.uniqueHosts's dedup
// without importing the kube package.
func hasMultipleHosts(ing *networkingv1.Ingress) bool {
	seen := map[string]struct{}{}
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		seen[rule.Host] = struct{}{}
		if len(seen) > 1 {
			return true
		}
	}
	return false
}
