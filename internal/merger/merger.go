// Package merger applies the preset + ingress-annotation merge rules
// for kube-discovered monitors. It implements kube.Materializer so the
// watcher can stay free of the heavy plumbing.
package merger

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/proxypool"
	"github.com/toggle-corp/toggle-monitor/internal/scheduler"
	"github.com/toggle-corp/toggle-monitor/internal/slack"
	"github.com/toggle-corp/toggle-monitor/internal/slug"
	"github.com/toggle-corp/toggle-monitor/internal/store"
)

// MonitorStore is the slim seam the materializer needs to detect
// static-vs-kube slug collisions and to upsert kube-discovered
// monitors.
type MonitorStore interface {
	GetMonitor(ctx context.Context, slug string) (store.MonitorRow, error)
	ReconcileMonitor(ctx context.Context, spec store.MonitorSpec) error
	MarkTemporaryPaused(ctx context.Context, slug string) error
}

// Materializer drives Issue-9 + Issue-10 logic: turns an
// (ingress, host) pair into a discovery snapshot row and, when
// appropriate, materializes an active monitor in the store.
//
// It also remembers the scheduler.Plan it produced for each
// materialized monitor so the running scheduler can pick them up via
// CurrentPlans(). Plans are stamped with a per-Materialize timestamp
// (lastSeen) so Prune() can drop entries for ingresses that have
// disappeared between reconciles.
type Materializer struct {
	store             MonitorStore
	presets           map[string]config.KubePreset
	annDomain         string
	pause             []config.KubePause
	staticSlugs       map[string]struct{}
	match             []config.KubeMatch
	friendlyNameStyle string

	// httpClientUA + slack.UserMapping carry through into the Plan
	// for each materialized kube monitor. proxies resolves preset
	// proxy slugs to the pre-built socks dialers.
	userAgent   string
	userMapping map[string]string
	bodyMaxBase int
	proxies     *proxypool.Pool

	mu        sync.RWMutex
	kubePlans map[string]planEntry
}

type planEntry struct {
	plan     scheduler.Plan
	lastSeen time.Time
}

// New builds a Materializer from the loaded YAML. staticSlugs is the
// set of slugs declared in config.Monitors — used to detect kube ↔
// static collisions. proxies is the pre-resolved proxy pool; nil is
// acceptable when no proxies are configured.
func New(s MonitorStore, cfg config.Config, proxies *proxypool.Pool) *Materializer {
	if cfg.Kube == nil {
		return nil
	}
	presets := make(map[string]config.KubePreset, len(cfg.Kube.Presets))
	for _, p := range cfg.Kube.Presets {
		presets[p.Slug] = p
	}
	statics := make(map[string]struct{}, len(cfg.Monitors))
	for _, m := range cfg.Monitors {
		statics[m.Slug] = struct{}{}
	}
	style := cfg.Kube.FriendlyName
	if style == "" {
		style = config.KubeFriendlyNameCompact
	}
	return &Materializer{
		store:             s,
		presets:           presets,
		annDomain:         cfg.Kube.AnnotationDomain,
		pause:             cfg.Kube.Pause,
		staticSlugs:       statics,
		match:             cfg.Kube.Match,
		friendlyNameStyle: style,
		userAgent:         cfg.HTTPClient.UserAgent,
		userMapping:       cfg.Slack.UserMapping,
		bodyMaxBase:       cfg.Slack.BodyMaxChars,
		proxies:           proxies,
		kubePlans:         map[string]planEntry{},
	}
}

// presetSource identifies how a preset slug was chosen, so the
// snapshot reason can explain *why* this preset was applied.
type presetSource int

const (
	presetSourceNone       presetSource = iota
	presetSourceAnnotation              // /kube.preset annotation
	presetSourceMatch                   // first matching match[] rule
	presetSourceIgnore                  // matching match[] rule with ignore: true
)

// resolvePreset picks a preset for the given (ingress, host) pair.
// Order: explicit annotation → first matching match[] rule.
// A match rule with no when conditions acts as the wildcard
// fallback (validation ensures it's the last rule). Returns
// ("", 0, -1) when nothing applies.
//
// matchIdx is the index of the winning match[] rule when source is
// presetSourceMatch, otherwise -1.
func (m *Materializer) resolvePreset(ing *networkingv1.Ingress, host string) (slug string, source presetSource, matchIdx int) {
	if s := ing.Annotations[m.annDomain+"/kube.preset"]; s != "" {
		return s, presetSourceAnnotation, -1
	}
	for i, r := range m.match {
		if r.When.Namespace != "" && !matchGlob(r.When.Namespace, ing.Namespace) {
			continue
		}
		if r.When.Host != "" && !matchGlob(r.When.Host, host) {
			continue
		}
		if r.Ignore {
			return "", presetSourceIgnore, i
		}
		return r.Preset, presetSourceMatch, i
	}
	return "", presetSourceNone, -1
}

// Materialize implements kube.Materializer. The row's Annotations
// field carries the raw ingress annotation set so the auto-discovery
// UI can render it. When the row's Status is "added" or "kube-paused",
// MonitorSlug is also populated to link the snapshot to the monitor.
func (m *Materializer) Materialize(ctx context.Context, ing *networkingv1.Ingress, host string) (store.DiscoverySnapshotRow, error) {
	base := store.DiscoverySnapshotRow{
		Namespace:   ing.Namespace,
		IngressName: ing.Name,
		Host:        host,
		Annotations: copyAnnotations(ing.Annotations),
	}

	// kube.pause matches by host (with glob). Pause wins over preset
	// presence.
	if pauseReason, paused := m.matchPause(host); paused {
		monSlug, slugErr := slug.SanitizeKubeDiscovered(ing.Namespace, ing.Name, host)
		if slugErr != nil {
			reason := "slug generation failed: " + slugErr.Error()
			base.Status, base.Reason = "kube-invalid", &reason
			return base, nil
		}
		// Reconcile a kube-paused monitor so history sticks around.
		if err := m.store.ReconcileMonitor(ctx, store.MonitorSpec{
			Slug:         monSlug,
			FriendlyName: m.friendlyName(ing, host),
			URL:          buildURL("https", host, ""),
			GroupSlug:    "kube-discovered",
			Source:       store.SourceKube,
		}); err != nil {
			return base, fmt.Errorf("reconcile paused monitor: %w", err)
		}
		_ = m.store.MarkTemporaryPaused(ctx, monSlug) // doubles as the "paused" status setter; status text is overloaded
		reason := "kube-paused"
		if pauseReason != "" {
			reason = "kube-paused: " + pauseReason
		}
		base.Status, base.Reason, base.MonitorSlug = "kube-paused", &reason, &monSlug
		return base, nil
	}

	// Preset resolution: explicit annotation → match[] rules →
	// defaultPreset. Unknown explicit annotation still flunks as
	// kube-invalid (typos shouldn't silently fall through).
	presetSlug, presetVia, matchIdx := m.resolvePreset(ing, host)
	if presetVia == presetSourceIgnore {
		// Matched a match[] rule with ignore: true. Persist the
		// snapshot row so the operator can see *which* rule fired
		// (and filter accordingly on /discovery), but do NOT
		// reconcile a monitor or stash a scheduler plan.
		reason := formatIgnoredReason(matchIdx, m.match)
		base.Status, base.Reason = "kube-ignored", &reason
		return base, nil
	}
	if presetSlug == "" {
		reason := "no preset annotation"
		base.Status, base.Reason = "kube-invalid", &reason
		return base, nil
	}

	// config.enabled=false opt-out.
	if ing.Annotations[m.annDomain+"/config.enabled"] == "false" {
		reason := "opt-out via config.enabled=false"
		base.Status, base.Reason, base.PresetSlug = "kube-invalid", &reason, &presetSlug
		return base, nil
	}

	preset, ok := m.presets[presetSlug]
	if !ok {
		reason := fmt.Sprintf("unknown preset slug %q", presetSlug)
		base.Status, base.Reason, base.PresetSlug = "kube-invalid", &reason, &presetSlug
		return base, nil
	}

	monSlug, slugErr := slug.SanitizeKubeDiscovered(ing.Namespace, ing.Name, host)
	if slugErr != nil {
		reason := "slug generation failed: " + slugErr.Error()
		base.Status, base.Reason, base.PresetSlug = "kube-invalid", &reason, &presetSlug
		return base, nil
	}

	if _, conflict := m.staticSlugs[monSlug]; conflict {
		reason := "slug conflicts with static monitor"
		base.Status, base.Reason, base.PresetSlug = "kube-invalid", &reason, &presetSlug
		return base, nil
	}

	scheme := preset.Scheme
	if scheme == "" {
		scheme = "https"
	}
	path := preset.Path
	if override := ing.Annotations[m.annDomain+"/kube.path"]; override != "" {
		path = override
	}

	groupSlug := preset.Group
	if override := ing.Annotations[m.annDomain+"/config.group"]; override != "" {
		groupSlug = override
	}
	if groupSlug == "" {
		groupSlug = "kube-discovered"
	}

	dependsOn := preset.DependsOn
	if override := ing.Annotations[m.annDomain+"/config.dependsOn"]; override != "" {
		dependsOn = splitAndTrim(override)
	}

	if err := m.store.ReconcileMonitor(ctx, store.MonitorSpec{
		Slug:         monSlug,
		FriendlyName: m.friendlyName(ing, host),
		URL:          buildURL(scheme, host, path),
		GroupSlug:    groupSlug,
		Source:       store.SourceKube,
		DependsOn:    dependsOn,
	}); err != nil {
		return base, fmt.Errorf("reconcile kube monitor: %w", err)
	}

	// Remember the plan so the scheduler's dynamic refresh loop picks
	// it up. The `kube` tag is auto-appended per design Q5a — for now
	// we just include it in the merged notify/tag handling further
	// down; Plan itself doesn't carry tags.
	notifyMerged := mergeNotify(preset.Notify, ing.Annotations[m.annDomain+"/config.notify"])
	mentions := slack.ResolveMentions(notifyMerged, m.userMapping)
	plan := scheduler.Plan{
		Slug:                   monSlug,
		FriendlyName:           m.friendlyName(ing, host),
		URL:                    buildURL(scheme, host, path),
		HTTPMethod:             preset.HTTPMethod,
		AcceptedStatusCodes:    append([]int(nil), preset.AcceptedStatusCodes...),
		Interval:               preset.Interval.AsDuration(),
		Timeout:                preset.Timeout.AsDuration(),
		Retries:                preset.Retries,
		RetryBackoff:           preset.RetryBackoff.AsDuration(),
		FollowRedirects:        preset.FollowRedirects,
		TLSInsecureSkipVerify:  preset.TLSInsecureSkipVerify,
		ProxyDialer:            m.proxies.Get(preset.Proxy),
		Proxy:                  preset.Proxy,
		Preset:                 presetSlug,
		UserAgent:              m.userAgent,
		ReminderInterval:       preset.ReminderInterval.AsDuration(),
		ChannelSlug:            preset.Slack,
		Mentions:               mentions,
		DependsOn:              append([]string(nil), dependsOn...),
		IsHTTPS:                scheme == "https",
		SSLAlertThreshold:      preset.SSLAlertThreshold.AsDuration(),
		SSLEscalationThreshold: preset.SSLEscalationThreshold.AsDuration(),
		SSLReminderInterval:    preset.SSLReminderInterval.AsDuration(),
	}
	m.mu.Lock()
	m.kubePlans[monSlug] = planEntry{plan: plan, lastSeen: time.Now()}
	m.mu.Unlock()

	reason := formatAddedReason(presetVia, matchIdx, m.match)
	base.Status, base.Reason, base.PresetSlug, base.MonitorSlug = "added", &reason, &presetSlug, &monSlug
	return base, nil
}

// matchCond renders the when-conditions of one match rule. Empty
// when (the wildcard-fallback case) returns "fallback".
func matchCond(w config.KubeMatchWhen) string {
	switch {
	case w.Namespace != "" && w.Host != "":
		return fmt.Sprintf("namespace=%s, host=%s", w.Namespace, w.Host)
	case w.Namespace != "":
		return "namespace=" + w.Namespace
	case w.Host != "":
		return "host=" + w.Host
	default:
		return "fallback"
	}
}

// formatIgnoredReason explains *which* match[] rule caused the
// ingress to be skipped — same conditions-string shape as
// formatAddedReason so /discovery reads consistently.
func formatIgnoredReason(matchIdx int, rules []config.KubeMatch) string {
	if matchIdx < 0 || matchIdx >= len(rules) {
		return "ignored"
	}
	return fmt.Sprintf("ignored (via match[%d]: %s)", matchIdx, matchCond(rules[matchIdx].When))
}

// formatAddedReason explains *which* path picked the preset for the
// /discovery UI. Helps the operator diagnose unexpected match-rule
// assignments without cross-referencing config.yaml by eye.
func formatAddedReason(via presetSource, matchIdx int, rules []config.KubeMatch) string {
	switch via {
	case presetSourceAnnotation:
		return "added"
	case presetSourceMatch:
		if matchIdx >= 0 && matchIdx < len(rules) {
			return fmt.Sprintf("added (via match[%d]: %s)", matchIdx, matchCond(rules[matchIdx].When))
		}
		return "added (via match)"
	default:
		return "added"
	}
}

// CurrentPlans returns the scheduler plans for every currently
// materialized kube monitor. Called by lifecycle.planSource on each
// scheduler refresh.
func (m *Materializer) CurrentPlans() []scheduler.Plan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]scheduler.Plan, 0, len(m.kubePlans))
	for _, e := range m.kubePlans {
		out = append(out, e.plan)
	}
	return out
}

// Prune drops every cached plan whose lastSeen timestamp is older
// than `before`. The kube.Watcher calls this at the end of every
// reconcile pass so disappeared ingresses stop being scheduled.
func (m *Materializer) Prune(before time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for slug, e := range m.kubePlans {
		if e.lastSeen.Before(before) {
			delete(m.kubePlans, slug)
		}
	}
}

// mergeNotify is the union of preset + annotation notify entries. The
// annotation form is a comma-separated string per docs/design-decisions.md.
func mergeNotify(presetNotify []string, annotationCSV string) []string {
	out := append([]string(nil), presetNotify...)
	if annotationCSV != "" {
		out = append(out, splitAndTrim(annotationCSV)...)
	}
	return out
}

func (m *Materializer) matchPause(host string) (reason string, matched bool) {
	for _, p := range m.pause {
		if hostMatchesGlob(p.Host, host) {
			return p.Reason, true
		}
	}
	return "", false
}

// matchGlob is a permissive `*`-style matcher used by kube.match[]
// for namespace + host conditions. `*` matches any run of non-`/`
// characters (path.Match semantics) — so `betaco-core-backend-*`
// matches `betaco-core-backend-1` and `*.example.com` matches
// `alpha-2.example.com`. Intentionally more permissive than
// hostMatchesGlob below (which is segment-strict for backward-compat
// with the older kube.pause rules).
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

// hostMatchesGlob supports a simple `*` wildcard (one segment) — e.g.
// "*.staging.example.com" matches "api.staging.example.com" but not
// "staging.example.com".
func hostMatchesGlob(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	// Split into segments and match each (one * = one segment).
	pSegs := strings.Split(pattern, ".")
	hSegs := strings.Split(host, ".")
	if len(pSegs) != len(hSegs) {
		return false
	}
	for i := range pSegs {
		if pSegs[i] == "*" {
			continue
		}
		if pSegs[i] != hSegs[i] {
			return false
		}
	}
	return true
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

func splitAndTrim(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func copyAnnotations(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ResyncInterval is exposed to lifecycle so the scheduler can refresh
// its dynamic plan list on the same cadence.
var _ = time.Minute
