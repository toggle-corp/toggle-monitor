package config

import (
	"path"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// prometheusLabelNamePattern is the label-name grammar from the
// Prometheus data model. It is narrower than a k8s qualified name in
// one direction (no dots, dashes or slashes) and wider in another
// (a leading underscore is legal), so neither validator substitutes for
// the other.
var prometheusLabelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// alertmanagerPathPattern is the validator for endpoint.path. The
// `/webhooks/` prefix is hardcoded; the operator picks a single
// lowercase-slug suffix. See ADR-0005 §"Endpoint, auth, body cap".
var alertmanagerPathPattern = regexp.MustCompile(`^/webhooks/[a-z0-9_-]+$`)

// Documented defaults for the alertmanager block (ADR-0005). Mirrored
// from the YAML schema so consumers (handler, store, sweeper) can
// derive effective values without re-parsing.
const (
	DefaultAlertmanagerEndpointPath         = "/webhooks/alertmanager"
	DefaultAlertmanagerRetentionDays        = 180
	DefaultAlertmanagerRateLimitPerChannel  = 10
	DefaultAlertmanagerRateLimitWindow      = 30 * time.Minute
	DefaultAlertmanagerRateLimitNoticeEvery = 24 * time.Hour
)

// LabelRegexSuffix is the per-key twin convention's regex marker:
// a label-selector key suffixed `Regex` selects via Go regexp on the
// base key. See ADR-0005 §"Match tree". Exported so the cascade
// evaluator (internal/alertmanager) can share the same source of
// truth instead of duplicating the literal.
const LabelRegexSuffix = "Regex"

// AlertmanagerConfig is the optional top-level block that turns on
// the Alertmanager webhook receiver (ADR-0005). When nil, the
// receiver is not mounted and no AM tables / sweeper run.
type AlertmanagerConfig struct {
	Endpoint      AlertmanagerEndpoint    `yaml:"endpoint"`
	RetentionDays int                     `yaml:"retentionDays,omitempty"`
	RateLimit     AlertmanagerRateLimit   `yaml:"rateLimit,omitempty"`
	Match         []AlertmanagerMatchRule `yaml:"match"`
}

// AlertmanagerEndpoint configures the inbound webhook listener.
// The path is a single-segment suffix under the hardcoded `/webhooks/`
// prefix; TokenEnv names the env var holding the Bearer token the
// runtime constant-time-compares against the Authorization header.
type AlertmanagerEndpoint struct {
	Path     string `yaml:"path,omitempty"`
	TokenEnv string `yaml:"tokenEnv"`
}

// AlertmanagerRateLimit tunes the per-channel sliding-window flood
// detector. PerChannel == 0 disables it entirely; otherwise Window
// and NoticeEvery must be positive (the latter is the cooldown
// between flood-notice messages on the same channel).
type AlertmanagerRateLimit struct {
	PerChannel  int      `yaml:"perChannel,omitempty"`
	Window      Duration `yaml:"window,omitempty"`
	NoticeEvery Duration `yaml:"noticeEvery,omitempty"`
}

// AlertmanagerMatchRule is one node in the cascading alertmanager.match
// tree. Grammar mirrors KubeMatchRule (ADR-0002) — When/Config/Nested
// optional; Ignore/Final are rule-level directives.
//
// When and Config are pointers so an absent block in YAML is
// distinguishable from `{}` at use time — the cascade evaluator (next
// slice) treats nil and empty the same, but the validator needs to
// know whether the operator wrote anything at all.
type AlertmanagerMatchRule struct {
	When   *AlertmanagerMatchWhen   `yaml:"when,omitempty"`
	Ignore *bool                    `yaml:"ignore,omitempty"`
	Final  bool                     `yaml:"final,omitempty"`
	Config *AlertmanagerMatchConfig `yaml:"config,omitempty"`
	Nested []AlertmanagerMatchRule  `yaml:"nested,omitempty"`
}

// AlertmanagerMatchWhen is the selector vocabulary for an
// alertmanager.match rule. All set fields AND together. Alertname is
// a glob (path.Match); AlertnameRegex is a Go regexp auto-anchored as
// ^…$ at use time. Labels follows the per-key twin convention: a key
// `K` selects via glob on labels.K; a key suffixed `KRegex` selects
// via Go regexp on labels.K. Receiver and ExternalURL are exact
// matches against the AM webhook envelope.
//
// Labels is represented as a single map (rather than two maps
// Labels / LabelsRegex) because the YAML grammar in ADR-0005 mixes
// glob and regex value-types under a single `labels:` key — keeping
// the in-memory shape symmetrical with the operator's writing surface
// makes "which key clashed?" diagnostics line up with the source.
type AlertmanagerMatchWhen struct {
	Alertname      string            `yaml:"alertname,omitempty"`
	AlertnameRegex string            `yaml:"alertnameRegex,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty"`
	Receiver       string            `yaml:"receiver,omitempty"`
	ExternalURL    string            `yaml:"externalURL,omitempty"`
}

// AlertmanagerMatchConfig is the cascade-contributed field block.
// v1 carries only Slack (required at the root rule) and Notify (union
// with !override; same semantics as KubeConfig.Notify). No tags,
// mention, group, friendlyName — AM alerts aren't probes and the
// renderer is hardcoded per ADR-0005.
type AlertmanagerMatchConfig struct {
	Slack  string     `yaml:"slack,omitempty"`
	Notify NotifyList `yaml:"notify,omitempty"`

	// SlackFrom sources Slack from a Namespace annotation (ADR-0013).
	// Only the namespaceAnnotation: scope is accepted here — an
	// alert's own annotations are authored by the PrometheusRule that
	// emitted it, not by the workload's owner.
	SlackFrom *ValueSource `yaml:"slackFrom,omitempty"`
	// NotifyFrom unions the annotation's handles onto the cascade;
	// NotifyOverrideFrom replaces them, mirroring the !override tag on
	// the literal list.
	NotifyFrom         *ValueSource `yaml:"notifyFrom,omitempty"`
	NotifyOverrideFrom *ValueSource `yaml:"notifyOverrideFrom,omitempty"`
}

// AlertmanagerValueKind distinguishes how a *From block's resolved
// value joins the merge: as a scalar (deepest layer that set the field
// wins) or as an entry list (union, or replace-the-baseline for the
// Override twin).
type AlertmanagerValueKind int

const (
	// AlertmanagerValueScalar merges like slack.
	AlertmanagerValueScalar AlertmanagerValueKind = iota
	// AlertmanagerValueList merges like notify.
	AlertmanagerValueList
)

// AlertmanagerValueSource pairs one set *From block with the literal
// field it supplies and the merge semantics it inherits from that
// field. Mirrors KubeValueSource (ADR-0009) for the AM field set.
type AlertmanagerValueSource struct {
	// Key is the YAML key, e.g. "slackFrom".
	Key string
	// Field is the literal field it supplies, e.g. "slack".
	Field string
	Kind  AlertmanagerValueKind
	// Override marks the replace-the-baseline twin, mirroring the
	// !override YAML tag on the literal list.
	Override bool
	Source   *ValueSource
}

// ValueSources returns every *From block set on this config block,
// paired with the field it supplies. Fixed order so error messages and
// provenance lines are reproducible.
//
// This table is the single place the AM keys are enumerated: the
// validator and the cascade's lowering pass both read it rather than
// re-deriving the mapping.
func (a *AlertmanagerMatchConfig) ValueSources() []AlertmanagerValueSource {
	if a == nil {
		return nil
	}
	all := []AlertmanagerValueSource{
		{Key: "slackFrom", Field: "slack", Kind: AlertmanagerValueScalar, Source: a.SlackFrom},
		{Key: "notifyFrom", Field: "notify", Kind: AlertmanagerValueList, Source: a.NotifyFrom},
		{Key: "notifyOverrideFrom", Field: "notify", Kind: AlertmanagerValueList, Override: true, Source: a.NotifyOverrideFrom},
	}
	out := make([]AlertmanagerValueSource, 0, len(all))
	for _, vs := range all {
		if vs.Source != nil {
			out = append(out, vs)
		}
	}
	return out
}

// applyAlertmanagerDefaults fills in the documented defaults on c.Alertmanager
// in place. Called by Load before validation so the validator sees
// the same values the runtime will. No-op when the block is nil.
func applyAlertmanagerDefaults(c *Config) {
	if c.Alertmanager == nil {
		return
	}
	am := c.Alertmanager
	if am.Endpoint.Path == "" {
		am.Endpoint.Path = DefaultAlertmanagerEndpointPath
	}
	if am.RetentionDays == 0 {
		am.RetentionDays = DefaultAlertmanagerRetentionDays
	}
	// Apply rate-limit defaults only when the block is wholly absent
	// (all three sub-fields zero). Once an operator has set any of
	// them — including `perChannel: 0` to disable the detector — we
	// must not silently rewrite the others, or "perChannel: 5 with
	// window: 0s" would slip through validation.
	if am.RateLimit.PerChannel == 0 && am.RateLimit.Window.AsDuration() == 0 && am.RateLimit.NoticeEvery.AsDuration() == 0 {
		am.RateLimit.PerChannel = DefaultAlertmanagerRateLimitPerChannel
		am.RateLimit.Window = Duration(DefaultAlertmanagerRateLimitWindow)
		am.RateLimit.NoticeEvery = Duration(DefaultAlertmanagerRateLimitNoticeEvery)
	}
}

// validateAlertmanager enforces the structural rules in ADR-0005
// §"Validation". It is a no-op when cfg.Alertmanager is nil.
//
// Slug references inside config blocks (slack channels, notify
// userMapping entries) are resolved against the pre-built sets the
// caller passes in — same plumbing as validateKube.
func (c *checker) validateAlertmanager(cfg *Config, slackChannels map[string]struct{}) {
	if cfg.Alertmanager == nil {
		return
	}
	am := cfg.Alertmanager
	base := []any{"alertmanager"}

	if !alertmanagerPathPattern.MatchString(am.Endpoint.Path) {
		c.errf(append(append([]any{}, base...), "endpoint", "path"),
			"%q must match %s", am.Endpoint.Path, alertmanagerPathPattern.String())
	}
	if am.Endpoint.TokenEnv == "" {
		c.errf(append(append([]any{}, base...), "endpoint", "tokenEnv"),
			"required (names the env var holding the Alertmanager Bearer token)")
	} else if !envVarNamePattern.MatchString(am.Endpoint.TokenEnv) {
		c.errf(append(append([]any{}, base...), "endpoint", "tokenEnv"),
			"%q must match %s (do not interpolate ${...} into this field)",
			am.Endpoint.TokenEnv, envVarNamePattern.String())
	}

	if am.RetentionDays < 0 {
		c.errf(append(append([]any{}, base...), "retentionDays"),
			"must be >= 0, got %d", am.RetentionDays)
	}

	rlBase := append(append([]any{}, base...), "rateLimit")
	if am.RateLimit.PerChannel < 0 {
		c.errf(append(append([]any{}, rlBase...), "perChannel"),
			"must be >= 0, got %d (0 disables the flood detector)", am.RateLimit.PerChannel)
	}
	if am.RateLimit.PerChannel > 0 {
		if am.RateLimit.Window.AsDuration() <= 0 {
			c.errf(append(append([]any{}, rlBase...), "window"),
				"must be > 0 when perChannel > 0, got %s", am.RateLimit.Window)
		}
		if am.RateLimit.NoticeEvery.AsDuration() <= 0 {
			c.errf(append(append([]any{}, rlBase...), "noticeEvery"),
				"must be > 0 when perChannel > 0, got %s", am.RateLimit.NoticeEvery)
		}
	}

	if len(am.Match) == 0 {
		c.errf(append(append([]any{}, base...), "match"),
			"at least one rule is required; the first rule must have an empty when: and set config.slack")
		return
	}
	root := am.Match[0]
	if !alertmanagerWhenIsEmpty(root.When) {
		c.errf(append(append([]any{}, base...), "match", 0, "when"),
			"the first rule must have an empty when: (the mandatory root baseline that sets config.slack for every alert)")
	}
	switch {
	case root.Config == nil:
		c.errf(append(append([]any{}, base...), "match", 0, "config", "slack"),
			"required at the root rule (every alert inherits this channel)")
	case root.Config.Slack == "" && root.Config.SlackFrom == nil:
		c.errf(append(append([]any{}, base...), "match", 0, "config", "slack"),
			"required at the root rule (every alert inherits this channel)")
	case root.Config.Slack == "" && !root.Config.SlackFrom.HasDefault:
		// A root slackFrom is only a baseline if it always yields a
		// channel; without default: a namespace that omits the annotation
		// resolves to none at all.
		c.errf(append(append([]any{}, base...), "match", 0, "config", "slackFrom", "default"),
			"required when slackFrom stands in for the root's slack: (an unannotated namespace would resolve to no channel)")
	}

	for i := range am.Match {
		c.validateAlertmanagerRule(&am.Match[i],
			append(append([]any{}, base...), "match", i),
			slackChannels, cfg.Slack.UserMapping, cfg.Kube != nil)
	}
}

// alertmanagerWhenIsEmpty reports whether a When block is absent or
// has every selector field unset. Used to identify the mandatory root
// baseline.
func alertmanagerWhenIsEmpty(w *AlertmanagerMatchWhen) bool {
	if w == nil {
		return true
	}
	return w.Alertname == "" && w.AlertnameRegex == "" &&
		len(w.Labels) == 0 && w.Receiver == "" && w.ExternalURL == ""
}

// validateAlertmanagerRule applies every per-rule structural check at
// base (the path slice to this rule), then recurses into Nested.
func (c *checker) validateAlertmanagerRule(
	r *AlertmanagerMatchRule,
	base []any,
	slackChannels map[string]struct{},
	userMapping map[string]string,
	kubeConfigured bool,
) {
	if r.When != nil {
		c.validateAlertmanagerWhen(r.When, append(append([]any{}, base...), "when"))
	}

	if r.Final && alertmanagerWhenIsEmpty(r.When) {
		c.errf(append(append([]any{}, base...), "final"),
			"final: true requires at least one selector in when: (otherwise the tree walk halts on the first alert)")
	}

	if r.Config != nil {
		cbase := append(append([]any{}, base...), "config")
		c.validateAlertmanagerConfig(r.Config, cbase, slackChannels, userMapping)
		c.validateAlertmanagerValueSources(r.Config, cbase, slackChannels, userMapping, kubeConfigured)
	}

	for i := range r.Nested {
		c.validateAlertmanagerRule(&r.Nested[i],
			append(append([]any{}, base...), "nested", i),
			slackChannels, userMapping, kubeConfigured)
	}
}

// validateAlertmanagerValueSources enforces the structural rules for
// ADR-0013 `*From` blocks on one config block: only the
// namespaceAnnotation: scope is accepted, that scope needs a kube:
// block to read through, a *From and the literal it supplies are
// mutually exclusive, so are notifyFrom and its Override twin, and the
// `default:` (reviewed config, unlike the annotation value) must satisfy
// the same constraints as the literal field.
func (c *checker) validateAlertmanagerValueSources(
	a *AlertmanagerMatchConfig,
	base []any,
	slackChannels map[string]struct{},
	userMapping map[string]string,
	kubeConfigured bool,
) {
	for _, vs := range a.ValueSources() {
		vbase := append(append([]any{}, base...), vs.Key)

		if a.isSetLiterally(vs.Field) {
			c.errf(vbase, "cannot be combined with %q in the same config block — set the field either literally or from an annotation", vs.Field)
		}

		if vs.Source.Annotation != "" {
			c.errf(vbase, "annotation: is not accepted here — only namespaceAnnotation: is, because an alert's own annotations are written by the alerting rule rather than by the workload's owner")
		}
		if vs.Source.NamespaceAnnotation == "" {
			c.errf(vbase, "requires namespaceAnnotation:")
		} else {
			if errs := validation.IsQualifiedName(vs.Source.NamespaceAnnotation); len(errs) > 0 {
				c.errf(vbase, "invalid k8s annotation key %q: %s",
					vs.Source.NamespaceAnnotation, strings.Join(errs, "; "))
			}
			if !kubeConfigured {
				c.errf(vbase, "namespaceAnnotation: requires a kube: block — the Namespace informer it reads through belongs to the kube watcher")
			}
		}

		if vs.Source.NamespaceLabel != "" && !prometheusLabelNamePattern.MatchString(vs.Source.NamespaceLabel) {
			c.errf(append(append([]any{}, vbase...), "namespaceLabel"),
				"%q is not a valid Prometheus label name (must match %s) — this names a label on the alert, not a k8s object key",
				vs.Source.NamespaceLabel, prometheusLabelNamePattern.String())
		}

		// An annotation may not supply raw <…> markup, so a notify source
		// with no roster to select from can never contribute a value.
		if vs.Field == "notify" && len(userMapping) == 0 {
			c.errf(vbase, "requires a non-empty slack.userMapping — an annotation may only select handles from it, never set raw Slack markup")
		}

		if vs.Source.HasDefault {
			c.validateAlertmanagerValueSourceDefault(vs, vbase, slackChannels, userMapping)
		}
	}

	// notifyFrom + notifyOverrideFrom in one block would have the same
	// layer both union and replace the baseline; the merge order between
	// them is undefined.
	if a.NotifyFrom != nil && a.NotifyOverrideFrom != nil {
		c.errf(append(append([]any{}, base...), "notifyOverrideFrom"),
			"cannot be combined with notifyFrom in the same config block")
	}
}

// isSetLiterally reports whether the block carries field as a literal.
// AlertmanagerMatchConfig has no presence map, so this mirrors what
// resolveStack treats as "set": a non-empty slack, and a notify list
// that has entries or is explicitly !override.
//
// This is value-emptiness where KubeConfig.IsSet is key-presence, so
// `slack: ""` alongside slackFrom: passes here and the equivalent under
// kube.match does not. Both resolve to the sourced value, since
// resolveStack skips an empty literal.
func (a *AlertmanagerMatchConfig) isSetLiterally(field string) bool {
	switch field {
	case "slack":
		return a.Slack != ""
	case "notify":
		return len(a.Notify.Values) > 0 || a.Notify.Override
	}
	return false
}

// validateAlertmanagerValueSourceDefault applies the literal field's own
// load-time constraints to a *From block's `default:`. Defaults are
// reviewed config, so they are hard errors here — unlike annotation
// values, which degrade to a warning at evaluation time.
func (c *checker) validateAlertmanagerValueSourceDefault(
	vs AlertmanagerValueSource,
	vbase []any,
	slackChannels map[string]struct{},
	userMapping map[string]string,
) {
	dbase := append(append([]any{}, vbase...), "default")
	// A sequence default on a scalar field leaves DefaultScalar empty,
	// which would otherwise surface as `unknown channel slug ""`.
	if vs.Kind == AlertmanagerValueScalar {
		if _, isList := vs.Source.Default.([]string); isList {
			c.errf(dbase, "must be a single value for %q, not a list", vs.Field)
			return
		}
	}
	switch vs.Field {
	case "slack":
		if _, ok := slackChannels[vs.Source.DefaultScalar]; !ok {
			c.errf(dbase, "unknown channel slug %q", vs.Source.DefaultScalar)
		}
	case "notify":
		for i, n := range vs.Source.DefaultList {
			if !c.isValidNotifyEntry(userMapping, n) {
				c.errf(append(append([]any{}, dbase...), i),
					"%q must be a userMapping slug or raw Slack markup wrapped in <…>", n)
			}
		}
	}
}

// validateAlertmanagerWhen enforces selector-level rules: alertname /
// alertnameRegex mutual exclusion, glob / regex parse, per-key twin
// uniqueness inside labels (K vs KRegex), label-key syntax, and
// regex compilability for any KRegex value.
func (c *checker) validateAlertmanagerWhen(w *AlertmanagerMatchWhen, base []any) {
	if w.Alertname != "" && w.AlertnameRegex != "" {
		c.errf(base,
			"alertname and alertnameRegex are mutually exclusive in the same when:")
	}
	if w.Alertname != "" {
		if _, err := path.Match(w.Alertname, ""); err != nil {
			c.errf(append(append([]any{}, base...), "alertname"),
				"invalid glob %q: %v", w.Alertname, err)
		}
	}
	if w.AlertnameRegex != "" {
		if _, err := regexp.Compile(w.AlertnameRegex); err != nil {
			c.errf(append(append([]any{}, base...), "alertnameRegex"),
				"invalid regex: %v", err)
		}
	}

	// Labels: collect base keys vs regex twins so we can flag
	// collisions on the same base key, then validate each value.
	baseKeys := map[string]bool{}
	regexKeys := map[string]bool{}
	for k := range w.Labels {
		if strings.HasSuffix(k, LabelRegexSuffix) && k != LabelRegexSuffix {
			regexKeys[strings.TrimSuffix(k, LabelRegexSuffix)] = true
		} else {
			baseKeys[k] = true
		}
	}
	for bk := range baseKeys {
		if regexKeys[bk] {
			c.errf(append(append([]any{}, base...), "labels"),
				"label key %q is set as both glob (%q) and regex (%q) — pick one",
				bk, bk, bk+LabelRegexSuffix)
		}
	}
	for k, v := range w.Labels {
		isRegex := strings.HasSuffix(k, LabelRegexSuffix) && k != LabelRegexSuffix
		validatedKey := k
		if isRegex {
			validatedKey = strings.TrimSuffix(k, LabelRegexSuffix)
		}
		if errs := validation.IsQualifiedName(validatedKey); len(errs) > 0 {
			c.errf(append(append([]any{}, base...), "labels", k),
				"invalid k8s label key %q: %s", validatedKey, strings.Join(errs, "; "))
		}
		if isRegex {
			if _, err := regexp.Compile(v); err != nil {
				c.errf(append(append([]any{}, base...), "labels", k),
					"invalid regex %q: %v", v, err)
			}
		} else if v != "" {
			if _, err := path.Match(v, ""); err != nil {
				c.errf(append(append([]any{}, base...), "labels", k),
					"invalid glob %q: %v", v, err)
			}
		}
	}
}

// validateAlertmanagerConfig runs cross-reference checks on a single
// config block (anywhere in the tree). Required-at-root presence is
// handled at the call site (only the root must set slack); this only
// validates the fields actually set.
func (c *checker) validateAlertmanagerConfig(
	cfg *AlertmanagerMatchConfig,
	base []any,
	slackChannels map[string]struct{},
	userMapping map[string]string,
) {
	if cfg.Slack != "" {
		if _, ok := slackChannels[cfg.Slack]; !ok {
			c.errf(append(append([]any{}, base...), "slack"),
				"unknown channel slug %q", cfg.Slack)
		}
	}
	for i, n := range cfg.Notify.Values {
		if !c.isValidNotifyEntry(userMapping, n) {
			c.errf(append(append([]any{}, base...), "notify", i),
				"%q must be a userMapping slug or raw Slack markup wrapped in <…>", n)
		}
	}
}
