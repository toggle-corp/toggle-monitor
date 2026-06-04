package config

import (
	"path"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

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

// labelRegexSuffix is the per-key twin convention's regex marker:
// a label-selector key suffixed `Regex` selects via Go regexp on the
// base key. See ADR-0005 §"Match tree".
const labelRegexSuffix = "Regex"

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
	} else {
		if root.Config == nil || root.Config.Slack == "" {
			c.errf(append(append([]any{}, base...), "match", 0, "config", "slack"),
				"required at the root rule (every alert inherits this channel)")
		}
	}

	for i := range am.Match {
		c.validateAlertmanagerRule(&am.Match[i],
			append(append([]any{}, base...), "match", i),
			slackChannels, cfg.Slack.UserMapping)
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
) {
	if r.When != nil {
		c.validateAlertmanagerWhen(r.When, append(append([]any{}, base...), "when"))
	}

	if r.Final && alertmanagerWhenIsEmpty(r.When) {
		c.errf(append(append([]any{}, base...), "final"),
			"final: true requires at least one selector in when: (otherwise the tree walk halts on the first alert)")
	}

	if r.Config != nil {
		c.validateAlertmanagerConfig(r.Config,
			append(append([]any{}, base...), "config"),
			slackChannels, userMapping)
	}

	for i := range r.Nested {
		c.validateAlertmanagerRule(&r.Nested[i],
			append(append([]any{}, base...), "nested", i),
			slackChannels, userMapping)
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
		if strings.HasSuffix(k, labelRegexSuffix) && k != labelRegexSuffix {
			regexKeys[strings.TrimSuffix(k, labelRegexSuffix)] = true
		} else {
			baseKeys[k] = true
		}
	}
	for bk := range baseKeys {
		if regexKeys[bk] {
			c.errf(append(append([]any{}, base...), "labels"),
				"label key %q is set as both glob (%q) and regex (%q) — pick one",
				bk, bk, bk+labelRegexSuffix)
		}
	}
	for k, v := range w.Labels {
		isRegex := strings.HasSuffix(k, labelRegexSuffix) && k != labelRegexSuffix
		validatedKey := k
		if isRegex {
			validatedKey = strings.TrimSuffix(k, labelRegexSuffix)
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
