package merger

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/slug"
)

// Annotation scopes a resolved value can come from. The first two name
// the object the annotation was read off; ScopeDefault means the
// annotation was absent and the rule's own `default:` supplied the
// value.
const (
	ScopeIngress   = "annotation"
	ScopeNamespace = "namespaceAnnotation"
	ScopeDefault   = "default"
)

// Env carries what a cascade walk needs beyond the Ingress itself: the
// namespace-scoped annotations `*From` blocks may read, and the
// reviewed-config rosters annotation values are checked against.
//
// A nil UserMapping or SlackChannels disables the corresponding
// membership check rather than rejecting everything — `explain`'s
// hypothetical mode resolves against a config that may not carry a
// roster, and refusing every value there would make the tool lie about
// what the daemon would do.
type Env struct {
	NamespaceAnnotations map[string]string
	UserMapping          map[string]string
	SlackChannels        map[string]struct{}
}

// Provenance records that one field's value came from an annotation
// rather than from a literal in the rule tree. Without it, ADR-0002's
// single-source-of-truth debugging story ("why does this monitor have
// these settings?") has an unanswerable second input.
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

func (p Provenance) String() string {
	if p.Scope == ScopeDefault {
		return fmt.Sprintf("%s=%s ← default (%s absent)", p.Field, p.Value, p.Key)
	}
	return fmt.Sprintf("%s=%s ← %s %s", p.Field, p.Value, p.Scope, p.Key)
}

// Warning is one rejected annotation value. Annotation values are
// unreviewed runtime input written by app teams, so a bad one degrades
// to the cascade value and reports here — it never blocks the monitor
// from materializing, because a typo must not cost availability
// monitoring.
type Warning struct {
	Rule  string
	Field string
	Key   string
	Scope string
	// Value is the specific rejected entry, or the whole annotation
	// value for scalar fields.
	Value  string
	Reason string
}

func (w Warning) String() string {
	return fmt.Sprintf("%s %s: %q %s", w.Scope, w.Key, w.Value, w.Reason)
}

// valueResolver lowers `*From` blocks into the literal fields the merge
// stack already knows how to combine, for one (Ingress, Namespace)
// annotation pair. Lowering per rule — rather than post-processing the
// resolved config — is what keeps the merge semantics identical to the
// literal field's: the value lands at that rule's position, so union,
// deepest-wins and positional override all fall out of the existing
// cascade with no new merge concept.
type valueResolver struct {
	ingress   map[string]string
	namespace map[string]string
	env       Env

	provenance []Provenance
	warnings   []Warning
}

func newValueResolver(ingressAnnotations map[string]string, env Env) *valueResolver {
	return &valueResolver{
		ingress:   ingressAnnotations,
		namespace: env.NamespaceAnnotations,
		env:       env,
	}
}

// apply returns the config block as the merge stack should see it: the
// original when the rule declares no `*From` block, otherwise a copy
// with each resolvable source lowered to its literal field.
func (r *valueResolver) apply(label string, c config.KubeConfig) config.KubeConfig {
	sources := c.ValueSources()
	if len(sources) == 0 {
		return c
	}
	out := c.Clone()
	for _, vs := range sources {
		r.lower(label, &out, vs)
	}
	return out
}

// lower resolves one `*From` block and writes it onto out. A source
// that resolves to nothing simply doesn't contribute, leaving the
// cascade value in place.
func (r *valueResolver) lower(label string, out *config.KubeConfig, vs config.KubeValueSource) {
	raw, scope, key := r.read(vs.Source)

	// An empty or whitespace-only annotation is absent, not "set to
	// nothing". These values are Helm-templated and an empty list
	// renders ""; reading that as "notify nobody" would silently
	// orphan a monitor on a values typo.
	if strings.TrimSpace(raw) == "" {
		if !vs.Source.HasDefault {
			return
		}
		r.writeDefault(label, out, vs, key)
		return
	}

	switch vs.Kind {
	case config.KubeValueScalar:
		r.lowerScalar(label, out, vs, raw, scope, key)
	case config.KubeValueList:
		r.lowerList(label, out, vs, raw, scope, key)
	case config.KubeValueStatusCodes:
		r.lowerStatusCodes(label, out, vs, raw, scope, key)
	}
}

// read returns the annotation value for the source's scope, along with
// the scope and key names for provenance and warnings.
func (r *valueResolver) read(src *config.ValueSource) (raw, scope, key string) {
	if src.NamespaceAnnotation != "" {
		return r.namespace[src.NamespaceAnnotation], ScopeNamespace, src.NamespaceAnnotation
	}
	return r.ingress[src.Annotation], ScopeIngress, src.Annotation
}

// writeDefault applies the rule's `default:`. Defaults live in reviewed
// config and were validated at load, so they are not re-checked here.
func (r *valueResolver) writeDefault(label string, out *config.KubeConfig, vs config.KubeValueSource, key string) {
	switch vs.Kind {
	case config.KubeValueScalar:
		r.setScalar(out, vs.Field, vs.Source.DefaultScalar)
		r.note(label, vs.Field, key, ScopeDefault, vs.Source.DefaultScalar)
	case config.KubeValueList:
		if len(vs.Source.DefaultList) == 0 {
			return
		}
		r.setList(out, vs, vs.Source.DefaultList)
		r.note(label, vs.Field, key, ScopeDefault, formatList(vs.Source.DefaultList))
	case config.KubeValueStatusCodes:
		codes, _ := parseStatusCodes(vs.Source.DefaultList)
		if len(codes) == 0 {
			return
		}
		out.AcceptedStatusCodes = codes
		out.MarkSet(vs.Field)
		r.note(label, vs.Field, key, ScopeDefault, formatList(vs.Source.DefaultList))
	}
}

// lowerStatusCodes resolves an acceptedStatusCodes annotation. The
// field is replace-by-default across the cascade, so a usable value
// here discards the inherited list rather than adding to it — which is
// also why an all-invalid value must contribute nothing: replacing a
// working list with an empty one fails checkResolved and costs the
// monitor entirely.
func (r *valueResolver) lowerStatusCodes(label string, out *config.KubeConfig, vs config.KubeValueSource, raw, scope, key string) {
	entries := config.SplitAnnotationList(raw)
	codes, rejected := parseStatusCodes(entries)
	for _, bad := range rejected {
		r.warn(label, vs.Field, key, scope, bad.value, bad.reason)
	}
	if len(codes) == 0 {
		if vs.Source.HasDefault {
			r.writeDefault(label, out, vs, key)
		}
		return
	}
	out.AcceptedStatusCodes = codes
	out.MarkSet(vs.Field)
	r.note(label, vs.Field, key, scope, formatCodes(codes))
}

// rejectedCode pairs an unusable entry with why it was dropped.
type rejectedCode struct {
	value  string
	reason string
}

// parseStatusCodes converts annotation entries to status codes,
// returning the usable ones and a reason per rejection. Partial
// validity is preserved, as for every other list-valued source.
func parseStatusCodes(entries []string) (config.StatusCodeList, []rejectedCode) {
	var codes config.StatusCodeList
	var rejected []rejectedCode
	for _, entry := range entries {
		code, err := strconv.Atoi(entry)
		if err != nil {
			rejected = append(rejected, rejectedCode{entry, "is not a number"})
			continue
		}
		if code < 100 || code > 599 {
			rejected = append(rejected, rejectedCode{entry, "is not a valid HTTP status code (100..599)"})
			continue
		}
		codes = append(codes, code)
	}
	return codes, rejected
}

// formatCodes renders resolved status codes for a provenance line.
func formatCodes(codes config.StatusCodeList) string {
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = strconv.Itoa(c)
	}
	return formatList(parts)
}

func (r *valueResolver) lowerScalar(label string, out *config.KubeConfig, vs config.KubeValueSource, raw, scope, key string) {
	value := strings.TrimSpace(raw)
	if reason := r.checkScalar(vs.Field, value); reason != "" {
		r.warn(label, vs.Field, key, scope, value, reason)
		// Fall back to the default when the rule carries one — a
		// garbage annotation should not also discard the reviewed
		// fallback the operator wrote for the absent case.
		if vs.Source.HasDefault {
			r.writeDefault(label, out, vs, key)
		}
		return
	}
	r.setScalar(out, vs.Field, value)
	r.note(label, vs.Field, key, scope, value)
}

// checkScalar returns why value is unusable for field, or "" when it
// is fine.
func (r *valueResolver) checkScalar(field, value string) string {
	switch field {
	case "path":
		if !strings.HasPrefix(value, "/") {
			return "must start with '/'"
		}
	case "slack":
		if r.env.SlackChannels == nil {
			return ""
		}
		if _, ok := r.env.SlackChannels[value]; !ok {
			return "is not a configured slack.channels[].slug"
		}
	}
	return ""
}

func (r *valueResolver) lowerList(label string, out *config.KubeConfig, vs config.KubeValueSource, raw, scope, key string) {
	var kept []string
	for _, entry := range config.SplitAnnotationList(raw) {
		if reason := r.checkListEntry(vs.Field, entry); reason != "" {
			r.warn(label, vs.Field, key, scope, entry, reason)
			continue
		}
		kept = append(kept, entry)
	}
	// Partial validity is preserved: one bad entry alongside a good one
	// keeps the good one. But a source that yields nothing usable must
	// not contribute — for an Override twin especially, replacing real
	// recipients with an empty list would silently orphan the monitor.
	if len(kept) == 0 {
		return
	}
	r.setList(out, vs, kept)
	r.note(label, vs.Field, key, scope, formatList(kept))
}

// checkListEntry returns why entry is unusable for field, or "" when it
// is fine.
func (r *valueResolver) checkListEntry(field, entry string) string {
	switch field {
	case "notify":
		// Raw <…> Slack markup is rejected from annotations by design:
		// the roster of who can be paged stays in reviewed config, and
		// an annotation may only select from it.
		if strings.HasPrefix(entry, "<") && strings.HasSuffix(entry, ">") {
			return "is raw Slack markup, which annotations may not set — use a slack.userMapping slug"
		}
		if r.env.UserMapping == nil {
			return ""
		}
		if _, ok := r.env.UserMapping[entry]; !ok {
			return "is not a slack.userMapping slug"
		}
	case "tags":
		if err := slug.ValidateTag(entry); err != nil {
			return "is not a valid tag: " + err.Error()
		}
	}
	return ""
}

func (r *valueResolver) setScalar(out *config.KubeConfig, field, value string) {
	switch field {
	case "path":
		out.Path = value
	case "slack":
		out.Slack = value
	}
	out.MarkSet(field)
}

func (r *valueResolver) setList(out *config.KubeConfig, vs config.KubeValueSource, values []string) {
	switch vs.Field {
	case "notify":
		out.Notify = config.NotifyList{Values: values, Override: vs.Override}
	case "tags":
		out.Tags = config.TagList{Values: values, Override: vs.Override}
	}
	out.MarkSet(vs.Field)
}

func (r *valueResolver) note(label, field, key, scope, value string) {
	r.provenance = append(r.provenance, Provenance{
		Rule: label, Field: field, Key: key, Scope: scope, Value: value,
	})
}

func (r *valueResolver) warn(label, field, key, scope, value, reason string) {
	r.warnings = append(r.warnings, Warning{
		Rule: label, Field: field, Key: key, Scope: scope, Value: value, Reason: reason,
	})
}

// formatList renders a resolved list for a provenance line.
func formatList(values []string) string {
	return "[" + strings.Join(values, ",") + "]"
}
