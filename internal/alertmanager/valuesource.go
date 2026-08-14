package alertmanager

import (
	"fmt"
	"strings"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

// DefaultNamespaceLabel is the alert label a *From block reads the
// namespace name from when the source doesn't name one. Prometheus
// convention; exporters that relabel it (exported_namespace,
// kubernetes_namespace) set namespaceLabel: explicitly.
const DefaultNamespaceLabel = "namespace"

// Annotation scopes a resolved value can come from. ScopeNamespace
// names the object the annotation was read off; ScopeDefault means the
// value came from the rule's own `default:`.
const (
	ScopeNamespace = "namespaceAnnotation"
	ScopeDefault   = "default"
)

// NamespaceAnnotationSource reads the annotations off a Namespace. The
// kube watcher's informer implements it; a nil source disables
// annotation reads entirely, which is how an AM-only deployment and a
// not-yet-synced informer both behave.
type NamespaceAnnotationSource interface {
	NamespaceAnnotations(namespace string) map[string]string
}

// Env carries what lowering needs beyond the alert itself: the source
// for namespace annotations, and the membership tests an annotation
// value is held to.
//
// KnownChannel and KnownHandle are predicates rather than rosters so
// they can be the same lookups the handler already routes and mentions
// through: a value this cascade accepts is therefore one the post path
// can definitely use. A nil predicate disables its check rather than
// rejecting everything, matching merger.Env — a caller with no roster
// should not be told every value is wrong.
type Env struct {
	Namespaces   NamespaceAnnotationSource
	KnownChannel func(slug string) bool
	KnownHandle  func(slug string) bool
}

// Provenance records that one field's value came from an annotation
// rather than from a literal in the rule tree. It mirrors
// merger.Provenance without sharing it: ADR-0005 treats each cascade's
// merge rules as a per-cascade contract, and the two packages have no
// other coupling.
type Provenance struct {
	// Rule is the rule label the value landed at, e.g. "match[1]".
	Rule  string
	Field string
	// Key is the annotation key read. Scope says which object it was
	// read from; for ScopeDefault it is the key that was absent.
	Key   string
	Scope string
	Value string
}

// String renders a provenance entry for the rule-chain column.
func (p Provenance) String() string {
	if p.Scope == ScopeDefault {
		return fmt.Sprintf("%s=%s ← default (%s absent)", p.Field, p.Value, p.Key)
	}
	return fmt.Sprintf("%s=%s ← %s %s", p.Field, p.Value, p.Scope, p.Key)
}

// Warning is one rejected annotation value. Annotation values are
// unreviewed runtime input, so a bad one degrades to the cascade value
// and reports here — it never drops the alert, because a typo on a
// namespace must not cost the operator a page.
type Warning struct {
	Rule  string
	Field string
	Key   string
	// Value is the specific rejected entry, or the whole annotation
	// value for scalar fields.
	Value string
	// Code is the stable classification, drawn from the Code* set below.
	// It is what the rejection counter labels by; Reason is free text for
	// humans and may embed a namespace or label name.
	Code   string
	Reason string
}

// Warning codes. This set is a metric label value, so it is small,
// fixed, and free of anything derived from cluster state.
//
// Only conditions an operator can act on get a code. An alert with no
// namespace label (every cluster-scoped rule: Watchdog, node pressure)
// and a namespace carrying no annotations are both ordinary — they
// resolve to "no annotation", the cascade value stands, and nothing is
// reported. Counting those would pin the rejection counter permanently
// non-zero, which is exactly what makes a signal worthless.
const (
	// CodeNoSource: no namespace annotation source is wired.
	CodeNoSource = "no_annotation_source"
	// CodeValueRejected: the annotation was read but its value is not
	// usable for the field.
	CodeValueRejected = "value_rejected"
)

// valueResolver lowers `*From` blocks into the literal fields the merge
// stack already combines, for one alert. Lowering per rule — rather
// than post-processing the resolved config — is what keeps the merge
// semantics identical to the literal field's: the value lands at that
// rule's position, so deepest-wins and positional override fall out of
// the existing cascade with no new merge concept.
type valueResolver struct {
	alert Alert
	env   Env

	provenance []Provenance
	warnings   []Warning
}

func newValueResolver(alert Alert, env Env) *valueResolver {
	return &valueResolver{alert: alert, env: env}
}

// apply returns the config block as the merge stack should see it: the
// original when the rule declares no `*From` block, otherwise a copy
// with each resolvable source lowered to its literal field.
func (r *valueResolver) apply(label string, c config.AlertmanagerMatchConfig) config.AlertmanagerMatchConfig {
	sources := c.ValueSources()
	if len(sources) == 0 {
		return c
	}
	out := c
	for _, vs := range sources {
		r.lower(label, &out, vs)
	}
	return out
}

// lower resolves one `*From` block and writes it onto out. A source
// that resolves to nothing simply doesn't contribute, leaving the
// cascade value in place.
func (r *valueResolver) lower(label string, out *config.AlertmanagerMatchConfig, vs config.AlertmanagerValueSource) {
	raw, key, code, reason := r.read(vs.Source)
	if reason != "" {
		r.warn(label, vs.Field, key, "", code, reason)
		r.fallback(label, out, vs, key)
		return
	}

	// An empty or whitespace-only annotation is absent, not "set to
	// nothing". These values are Helm-templated and an empty list
	// renders ""; reading that as "notify nobody" would silently
	// silence a namespace on a values typo.
	if strings.TrimSpace(raw) == "" {
		r.fallback(label, out, vs, key)
		return
	}

	switch vs.Kind {
	case config.AlertmanagerValueScalar:
		r.lowerScalar(label, out, vs, raw, key)
	case config.AlertmanagerValueList:
		r.lowerList(label, out, vs, raw, key)
	}
}

// read returns the annotation value for the source, or a reason why it
// could not be read at all. The key is returned either way so warnings
// and provenance can name it.
//
// An alert with no namespace label reads as "no annotation", not as an
// error: cluster-scoped rules have no namespace by nature. So does a
// namespace the lister has nothing for — it returns nil both for a
// namespace it has never seen and for one that carries no annotations,
// and the ordinary case is the second.
func (r *valueResolver) read(src *config.ValueSource) (raw, key, code, reason string) {
	key = src.NamespaceAnnotation
	label := src.NamespaceLabel
	if label == "" {
		label = DefaultNamespaceLabel
	}
	namespace := r.alert.Labels[label]
	if namespace == "" {
		return "", key, "", ""
	}
	if r.env.Namespaces == nil {
		return "", key, CodeNoSource, "no namespace annotation source is wired (kube discovery disabled or not yet synced)"
	}
	return r.env.Namespaces.NamespaceAnnotations(namespace)[key], key, "", ""
}

// fallback applies the rule's `default:` when it has one. Defaults live
// in reviewed config and were validated at load, so they are not
// re-checked here. With no default the source contributes nothing and
// the cascade value stands.
func (r *valueResolver) fallback(label string, out *config.AlertmanagerMatchConfig, vs config.AlertmanagerValueSource, key string) {
	if !vs.Source.HasDefault {
		return
	}
	switch vs.Kind {
	case config.AlertmanagerValueScalar:
		r.setScalar(out, vs.Field, vs.Source.DefaultScalar)
		r.note(label, vs.Field, key, ScopeDefault, vs.Source.DefaultScalar)
	case config.AlertmanagerValueList:
		if len(vs.Source.DefaultList) == 0 {
			return
		}
		r.setList(out, vs, vs.Source.DefaultList)
		r.note(label, vs.Field, key, ScopeDefault, formatList(vs.Source.DefaultList))
	}
}

func (r *valueResolver) lowerScalar(label string, out *config.AlertmanagerMatchConfig, vs config.AlertmanagerValueSource, raw, key string) {
	value := strings.TrimSpace(raw)
	if reason := r.checkScalar(vs.Field, value); reason != "" {
		r.warn(label, vs.Field, key, value, CodeValueRejected, reason)
		// Fall back to the default when the rule carries one — a garbage
		// annotation should not also discard the reviewed fallback the
		// operator wrote for the absent case.
		r.fallback(label, out, vs, key)
		return
	}
	r.setScalar(out, vs.Field, value)
	r.note(label, vs.Field, key, ScopeNamespace, value)
}

// checkScalar returns why value is unusable for field, or "" when it is
// fine.
func (r *valueResolver) checkScalar(field, value string) string {
	if field == "slack" {
		if r.env.KnownChannel == nil {
			return ""
		}
		if !r.env.KnownChannel(value) {
			return "is not a configured slack.channels[].slug"
		}
	}
	return ""
}

func (r *valueResolver) lowerList(label string, out *config.AlertmanagerMatchConfig, vs config.AlertmanagerValueSource, raw, key string) {
	var kept []string
	for _, entry := range config.SplitAnnotationList(raw) {
		if reason := r.checkListEntry(vs.Field, entry); reason != "" {
			r.warn(label, vs.Field, key, entry, CodeValueRejected, reason)
			continue
		}
		kept = append(kept, entry)
	}
	// Partial validity is preserved: one bad entry alongside a good one
	// keeps the good one. A source that yields nothing usable must not
	// contribute — for the Override twin especially, replacing real
	// recipients with an empty list would silence the alert's mentions.
	if len(kept) == 0 {
		r.fallback(label, out, vs, key)
		return
	}
	r.setList(out, vs, kept)
	r.note(label, vs.Field, key, ScopeNamespace, formatList(kept))
}

// checkListEntry returns why entry is unusable for field, or "" when it
// is fine.
func (r *valueResolver) checkListEntry(field, entry string) string {
	if field == "notify" {
		// Raw <…> Slack markup is rejected from annotations by design:
		// the roster of who can be paged stays in reviewed config, and
		// an annotation may only select from it.
		if strings.HasPrefix(entry, "<") && strings.HasSuffix(entry, ">") {
			return "is raw Slack markup, which annotations may not set — use a slack.userMapping slug"
		}
		if r.env.KnownHandle == nil {
			return ""
		}
		if !r.env.KnownHandle(entry) {
			return "is not a slack.userMapping slug"
		}
	}
	return ""
}

func (r *valueResolver) setScalar(out *config.AlertmanagerMatchConfig, field, value string) {
	if field == "slack" {
		out.Slack = value
	}
}

func (r *valueResolver) setList(out *config.AlertmanagerMatchConfig, vs config.AlertmanagerValueSource, values []string) {
	if vs.Field == "notify" {
		out.Notify = config.NotifyList{Values: values, Override: vs.Override}
	}
}

func (r *valueResolver) note(label, field, key, scope, value string) {
	r.provenance = append(r.provenance, Provenance{
		Rule: label, Field: field, Key: key, Scope: scope, Value: value,
	})
}

func (r *valueResolver) warn(label, field, key, value, code, reason string) {
	r.warnings = append(r.warnings, Warning{
		Rule: label, Field: field, Key: key,
		Value: value, Code: code, Reason: reason,
	})
}

// formatList renders a resolved list for a provenance line.
func formatList(values []string) string {
	return "[" + strings.Join(values, ",") + "]"
}
