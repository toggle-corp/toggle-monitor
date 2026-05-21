// Package merger applies the preset + ingress-annotation merge rules
// for kube-discovered monitors. It implements kube.Materializer so the
// watcher can stay free of the heavy plumbing.
package merger

import (
	"context"
	"fmt"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toggle-corp/toggle-monitor/internal/config"
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
type Materializer struct {
	store       MonitorStore
	presets     map[string]config.KubePreset
	annDomain   string
	pause       []config.KubePause
	staticSlugs map[string]struct{}
}

// New builds a Materializer from the loaded YAML. staticSlugs is the
// set of slugs declared in config.Monitors — used to detect kube ↔
// static collisions.
func New(s MonitorStore, kc *config.Kube, staticMonitors []config.Monitor) *Materializer {
	if kc == nil {
		return nil
	}
	presets := make(map[string]config.KubePreset, len(kc.Presets))
	for _, p := range kc.Presets {
		presets[p.Slug] = p
	}
	statics := make(map[string]struct{}, len(staticMonitors))
	for _, m := range staticMonitors {
		statics[m.Slug] = struct{}{}
	}
	return &Materializer{
		store:       s,
		presets:     presets,
		annDomain:   kc.AnnotationDomain,
		pause:       kc.Pause,
		staticSlugs: statics,
	}
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
			FriendlyName: defaultFriendlyName(ing, host),
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

	// kube.preset annotation drives opt-in.
	presetSlug := ing.Annotations[m.annDomain+"/kube.preset"]
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
		FriendlyName: defaultFriendlyName(ing, host),
		URL:          buildURL(scheme, host, path),
		GroupSlug:    groupSlug,
		Source:       store.SourceKube,
		DependsOn:    dependsOn,
	}); err != nil {
		return base, fmt.Errorf("reconcile kube monitor: %w", err)
	}

	reason := "added"
	base.Status, base.Reason, base.PresetSlug, base.MonitorSlug = "added", &reason, &presetSlug, &monSlug
	return base, nil
}

func (m *Materializer) matchPause(host string) (reason string, matched bool) {
	for _, p := range m.pause {
		if hostMatchesGlob(p.Host, host) {
			return p.Reason, true
		}
	}
	return "", false
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

func defaultFriendlyName(ing *networkingv1.Ingress, host string) string {
	return ing.Namespace + "/" + ing.Name + " (" + host + ")"
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