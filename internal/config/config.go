// Package config loads, validates, and merges the toggle-monitor YAML
// configuration. See docs/config-schema.md.
//
// Issue 2 scope: parse and validate the minimum field set needed to
// run a static-monitor-only worker. Anchors, env interpolation,
// multi-error reporting, and the slack section land in later issues.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/toggle-corp/toggle-monitor/internal/slug"
)

// channelIDPattern matches public/private channel IDs. C* = public,
// G* = private (Slack's legacy private-channel prefix). DMs (D…) are
// rejected at config-load.
var channelIDPattern = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)

// userOrSubteamIDPattern matches user IDs (U...) and subteam IDs (S...).
var userOrSubteamIDPattern = regexp.MustCompile(`^[US][A-Z0-9]{8,}$`)

// Config is the typed, validated representation of the toggle-monitor
// YAML config.
type Config struct {
	DisplayTimezone string     `yaml:"displayTimezone"`
	PublicBaseURL   string     `yaml:"publicBaseURL,omitempty"`
	DBBodyMaxChars  int        `yaml:"dbBodyMaxChars"`
	Database        Database   `yaml:"database"`
	UI              UI         `yaml:"ui"`
	Theme           Theme      `yaml:"theme"`
	HTTPClient      HTTPClient `yaml:"httpClient"`
	Heartbeat       *Heartbeat `yaml:"heartbeat,omitempty"` // optional; nil disables the deadman loop
	Kube            *Kube      `yaml:"kube,omitempty"`      // optional; nil disables auto-discovery
	Slack           Slack      `yaml:"slack"`
	Proxies         []Proxy    `yaml:"proxies,omitempty"`
	Groups          []Group    `yaml:"groups"`
	Monitors        []Monitor  `yaml:"monitors"`
}

// Proxy is one outbound proxy that monitors can route their probes
// through. v1 supports SOCKS5 only.
type Proxy struct {
	Slug        string `yaml:"slug"`
	Protocol    string `yaml:"protocol"` // "socks5" — only supported value in v1
	Server      string `yaml:"server"`
	Port        int    `yaml:"port,omitempty"`        // defaults to 1080 for socks5
	Username    string `yaml:"username,omitempty"`    // optional; if set without passwordEnv, auth is username-only
	PasswordEnv string `yaml:"passwordEnv,omitempty"` // env var name (env-resolved like every other secret); requires username
}

// Heartbeat is the outbound deadman heartbeat block. When nil, the
// background loop is not started.
type Heartbeat struct {
	URL                 string   `yaml:"url"`
	Interval            Duration `yaml:"interval"`
	FailOnStalledWorker bool     `yaml:"failOnStalledWorker"`
}

// Kube is the auto-discovery block. When nil, no informer is started
// and no kube monitors are materialized.
type Kube struct {
	AnnotationDomain string       `yaml:"annotationDomain"`
	ResyncInterval   Duration     `yaml:"resyncInterval"`
	Pause            []KubePause  `yaml:"pause,omitempty"`
	Presets          []KubePreset `yaml:"presets,omitempty"`

	// DefaultPreset is the slug of the preset used when an ingress
	// has no kube.preset annotation and no match[] rule fires. Empty
	// keeps the original "kube-invalid: no preset annotation"
	// behavior. Must reference an entry in Presets when set.
	DefaultPreset string `yaml:"defaultPreset,omitempty"`

	// Match resolves an ingress to a preset by namespace/host pattern
	// when the kube.preset annotation is absent. Rules are evaluated
	// in declaration order; the first matching rule wins. If no rule
	// matches, DefaultPreset is consulted next.
	Match []KubeMatch `yaml:"match,omitempty"`

	// FriendlyName picks the auto-generated monitor name style. One of:
	//   "plain"   — `(ns) ingress-name` (host appended)
	//   "compact" — same, with the `-ingress` suffix stripped (default)
	//   "dedupe"  — also strips the namespace prefix from the name
	//   "title"   — dedupe + title-case with spaces
	// Empty defaults to "compact".
	FriendlyName string `yaml:"friendlyName,omitempty"`
}

// KubeFriendlyNameStyle constants — the allowed values of
// kube.friendlyName. The empty string is treated as compact.
const (
	KubeFriendlyNamePlain   = "plain"
	KubeFriendlyNameCompact = "compact"
	KubeFriendlyNameDedupe  = "dedupe"
	KubeFriendlyNameTitle   = "title"
)

// KubeFriendlyNameStyles is the canonical, declaration-ordered list
// of allowed kube.friendlyName values. Both the validator and the
// validator's error message read from this slice so they can't drift.
var KubeFriendlyNameStyles = []string{
	KubeFriendlyNamePlain,
	KubeFriendlyNameCompact,
	KubeFriendlyNameDedupe,
	KubeFriendlyNameTitle,
}

// KubeMatch is one conditional rule: when `when` fires, the ingress
// is either materialized with `preset` or skipped entirely via
// `ignore: true`. Exactly one of preset/ignore must be set per rule.
// The `when` conditions AND together; both when-fields are optional
// but at least one must be set. Globs use the same `*`-per-segment
// syntax as KubePause.Host.
type KubeMatch struct {
	When   KubeMatchWhen `yaml:"when"`
	Preset string        `yaml:"preset,omitempty"`
	// Ignore=true skips the ingress entirely — no monitor is created,
	// the discovery snapshot row is recorded with status="kube-ignored"
	// so the operator can see the rule fired (and filter accordingly
	// on /discovery). Useful for namespace globs like "test-*" that
	// churn the listing without operational value.
	Ignore bool `yaml:"ignore,omitempty"`
}

// KubeMatchWhen carries the conditions checked against an ingress.
type KubeMatchWhen struct {
	Namespace string `yaml:"namespace,omitempty"`
	Host      string `yaml:"host,omitempty"`
}

// KubePause is one entry in the kube.pause list — a host or host
// glob that, when matched, materializes as a kube-paused monitor.
type KubePause struct {
	Host   string `yaml:"host"`
	Reason string `yaml:"reason,omitempty"`
}

// KubePreset is the per-preset config block referenced by ingress
// annotations to materialize a monitor with full settings.
type KubePreset struct {
	Slug                   string   `yaml:"slug"`
	Scheme                 string   `yaml:"scheme"`
	Path                   string   `yaml:"path"`
	HTTPMethod             string   `yaml:"httpMethod"`
	AcceptedStatusCodes    []int    `yaml:"acceptedStatusCodes"`
	Interval               Duration `yaml:"interval"`
	Timeout                Duration `yaml:"timeout"`
	Retries                int      `yaml:"retries"`
	RetryBackoff           Duration `yaml:"retryBackoff"`
	FollowRedirects        bool     `yaml:"followRedirects"`
	TLSInsecureSkipVerify  bool     `yaml:"tlsInsecureSkipVerify,omitempty"`
	Proxy                  string   `yaml:"proxy,omitempty"`
	ReminderInterval       Duration `yaml:"reminderInterval"`
	SSLAlertThreshold      Duration `yaml:"sslAlertThreshold"`
	SSLEscalationThreshold Duration `yaml:"sslEscalationThreshold"`
	SSLReminderInterval    Duration `yaml:"sslReminderInterval"`
	Slack                  string   `yaml:"slack"`
	Notify                 []string `yaml:"notify,omitempty"`
	Tags                   []string `yaml:"tags,omitempty"`
	DependsOn              []string `yaml:"dependsOn,omitempty"`
	Group                  string   `yaml:"group,omitempty"`
}

// Slack is the consolidated Slack-related config block. v1 supports a
// single workspace; multiple channels can be declared and referenced
// by slug from monitors.
type Slack struct {
	BodyMaxChars      int               `yaml:"bodyMaxChars"`
	DependentsNoteMax int               `yaml:"dependentsNoteMax,omitempty"` // 0 → DefaultDependentsNoteMax
	Channels          []SlackChannel    `yaml:"channels"`
	UserMapping       map[string]string `yaml:"userMapping,omitempty"` // slug → U... | S...
}

// SlackChannel is one Slack destination.
type SlackChannel struct {
	Slug      string `yaml:"slug"`
	ChannelID string `yaml:"channelId"`
	TokenEnv  string `yaml:"tokenEnv"`
}

type Database struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	User        string `yaml:"user"`
	Name        string `yaml:"name"`
	SSLMode     string `yaml:"sslMode"`
	PasswordEnv string `yaml:"passwordEnv"`
}

type UI struct {
	PageSize   PageSize `yaml:"pageSize"`
	MaxPerPage int      `yaml:"maxPerPage"`
}

type PageSize struct {
	HomepageAlerts   int `yaml:"homepageAlerts"`
	MonitorListing   int `yaml:"monitorListing"`
	MonitorHistory   int `yaml:"monitorHistory"`
	DiscoveryListing int `yaml:"discoveryListing"`
}

type Theme struct {
	DefaultGroupColor string `yaml:"defaultGroupColor"`
}

type HTTPClient struct {
	UserAgent string `yaml:"userAgent"`
}

type Group struct {
	Slug         string   `yaml:"slug"`
	FriendlyName string   `yaml:"friendlyName"`
	Description  string   `yaml:"description,omitempty"`
	LogoURL      string   `yaml:"logoUrl,omitempty"`
	Color        string   `yaml:"color,omitempty"`
	Notify       []string `yaml:"notify,omitempty"` // userMapping slugs or raw <…> markup; merged union with monitor.notify
}

type Monitor struct {
	Slug                string   `yaml:"slug"`
	FriendlyName        string   `yaml:"friendlyName"`
	URL                 string   `yaml:"url"`
	Group               string   `yaml:"group"`
	HTTPMethod          string   `yaml:"httpMethod"`
	AcceptedStatusCodes []int    `yaml:"acceptedStatusCodes"`
	Interval            Duration `yaml:"interval"`
	Timeout             Duration `yaml:"timeout"`
	Retries             int      `yaml:"retries"`
	RetryBackoff        Duration `yaml:"retryBackoff"`
	FollowRedirects     bool     `yaml:"followRedirects"`
	// TLSInsecureSkipVerify disables Go's TLS verification for this
	// monitor — needed when probing HTTPS endpoints that present a
	// self-signed cert or any chain we don't want to trust. Implies
	// "do not track SSL expiry": the monitor stays at ssl-skipped and
	// the SSL state machine is bypassed.
	TLSInsecureSkipVerify bool     `yaml:"tlsInsecureSkipVerify,omitempty"`
	Proxy                 string   `yaml:"proxy,omitempty"` // proxies[].slug; routes the probe through that proxy
	ReminderInterval      Duration `yaml:"reminderInterval"`
	Slack                 string   `yaml:"slack"`               // channel slug
	Notify                []string `yaml:"notify,omitempty"`    // raw <...> Slack markup or userMapping slug
	DependsOn             []string `yaml:"dependsOn,omitempty"` // upstream static-monitor slugs that gate this one

	// SSL thresholds — required when URL is HTTPS, allowed but
	// ignored for HTTP URLs (so anchored defaults can be shared).
	SSLAlertThreshold      Duration `yaml:"sslAlertThreshold,omitempty"`
	SSLEscalationThreshold Duration `yaml:"sslEscalationThreshold,omitempty"`
	SSLReminderInterval    Duration `yaml:"sslReminderInterval,omitempty"`
}

// Load parses and validates the YAML config. Returns a populated
// Config on success, or a descriptive error on validation failure.
//
// Top-level keys whose names start with "x-" are silently ignored
// (docker-compose convention for anchor-only hosts). All other
// unrecognized top-level keys are a hard error.
func Load(data []byte) (Config, error) {
	expanded, err := interpolate(data)
	if err != nil {
		return Config{}, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(expanded, &root); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}
	if err := checkTopLevelKeys(&root); err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}
	c := &checker{root: &root}
	c.validate(&cfg)
	if err := c.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// knownTopLevelKeys is the schema's allowlist (see
// docs/config-schema.md §6). Keys prefixed with "x-" are also
// accepted, regardless of their value, for anchor-only hosts.
var knownTopLevelKeys = map[string]struct{}{
	"displayTimezone": {}, "publicBaseURL": {}, "dbBodyMaxChars": {},
	"kube": {}, "ui": {}, "theme": {}, "httpClient": {}, "heartbeat": {}, "database": {},
	"slack":   {},
	"proxies": {},
	"groups":  {}, "monitors": {},
}

func checkTopLevelKeys(root *yaml.Node) error {
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		key := top.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		name := key.Value
		if strings.HasPrefix(name, "x-") {
			continue
		}
		if _, ok := knownTopLevelKeys[name]; !ok {
			return fmt.Errorf("line %d: unknown top-level key %q (use an x-* prefix for anchor-only blocks)", key.Line, name)
		}
	}
	return nil
}

// checker accumulates validation errors with line numbers resolved
// against the original YAML node tree.
type checker struct {
	root *yaml.Node
	errs []error
}

func (c *checker) errf(path []any, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := 0
	if n := nodeAt(c.root, path...); n != nil {
		line = n.Line
	}
	p := pathStr(path)
	if line > 0 {
		c.errs = append(c.errs, fmt.Errorf("line %d: %s: %s", line, p, msg))
	} else {
		c.errs = append(c.errs, fmt.Errorf("%s: %s", p, msg))
	}
}

func (c *checker) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return errors.Join(c.errs...)
}

// validate runs every cross-field check against cfg, accumulating
// errors with line numbers from the underlying yaml.Node tree.
func (c *checker) validate(cfg *Config) {
	// Database password env var name must match the env var regex.
	if !envVarNamePattern.MatchString(cfg.Database.PasswordEnv) {
		c.errf([]any{"database", "passwordEnv"},
			"%q must match ^[A-Z][A-Z0-9_]*$ (do not interpolate ${...} into this field)", cfg.Database.PasswordEnv)
	}

	if cfg.Heartbeat != nil {
		if cfg.Heartbeat.URL == "" {
			c.errf([]any{"heartbeat", "url"}, "required when heartbeat block is set")
		}
		if cfg.Heartbeat.Interval.AsDuration() < 30*time.Second {
			c.errf([]any{"heartbeat", "interval"}, "must be >= 30s, got %s", cfg.Heartbeat.Interval)
		}
	}

	// proxies: per-entry validation + slug uniqueness. Built early so
	// both kube presets and static monitors can reference proxy slugs.
	seenProxies := map[string]struct{}{}
	for i, p := range cfg.Proxies {
		base := []any{"proxies", i}
		if err := slug.Validate(p.Slug); err != nil {
			c.errf(append(base, "slug"), "%v", err)
		}
		if _, dup := seenProxies[p.Slug]; dup {
			c.errf(append(base, "slug"), "duplicate slug %q", p.Slug)
		}
		seenProxies[p.Slug] = struct{}{}
		if p.Protocol != "socks5" {
			c.errf(append(base, "protocol"), "must be %q (only supported value), got %q", "socks5", p.Protocol)
		}
		if p.Server == "" {
			c.errf(append(base, "server"), "required")
		}
		if p.Port < 0 || p.Port > 65535 {
			c.errf(append(base, "port"), "must be in 1..65535 (or 0 for the protocol default), got %d", p.Port)
		}
		if p.PasswordEnv != "" {
			if !envVarNamePattern.MatchString(p.PasswordEnv) {
				c.errf(append(base, "passwordEnv"),
					"%q must match ^[A-Z][A-Z0-9_]*$ (do not interpolate ${...} into this field)", p.PasswordEnv)
			}
			if p.Username == "" {
				c.errf(append(base, "passwordEnv"), "requires username to be set")
			}
		}
	}

	if cfg.Kube != nil {
		if cfg.Kube.AnnotationDomain == "" {
			c.errf([]any{"kube", "annotationDomain"}, "required when kube block is set")
		}
		if cfg.Kube.ResyncInterval.AsDuration() < time.Minute {
			c.errf([]any{"kube", "resyncInterval"}, "must be >= 1m, got %s", cfg.Kube.ResyncInterval)
		}
		seenPresets := map[string]struct{}{}
		for i, p := range cfg.Kube.Presets {
			base := []any{"kube", "presets", i}
			if err := slug.Validate(p.Slug); err != nil {
				c.errf(append(base, "slug"), "%v", err)
			}
			if _, dup := seenPresets[p.Slug]; dup {
				c.errf(append(base, "slug"), "duplicate preset slug %q", p.Slug)
			}
			seenPresets[p.Slug] = struct{}{}
			if p.Scheme != "" && p.Scheme != "http" && p.Scheme != "https" {
				c.errf(append(base, "scheme"), "must be http or https, got %q", p.Scheme)
			}
			if p.Proxy != "" {
				if _, ok := seenProxies[p.Proxy]; !ok {
					c.errf(append(base, "proxy"), "unknown proxy slug %q", p.Proxy)
				}
			}
		}
		if cfg.Kube.DefaultPreset != "" {
			if _, ok := seenPresets[cfg.Kube.DefaultPreset]; !ok {
				c.errf([]any{"kube", "defaultPreset"},
					"references unknown preset slug %q", cfg.Kube.DefaultPreset)
			}
		}
		if v := cfg.Kube.FriendlyName; v != "" {
			ok := false
			for _, allowed := range KubeFriendlyNameStyles {
				if v == allowed {
					ok = true
					break
				}
			}
			if !ok {
				c.errf([]any{"kube", "friendlyName"},
					"must be one of %s (got %q)", strings.Join(KubeFriendlyNameStyles, ", "), v)
			}
		}
		for i, r := range cfg.Kube.Match {
			base := []any{"kube", "match", i}
			switch {
			case r.Preset == "" && !r.Ignore:
				c.errf(base, "exactly one of preset or ignore is required")
			case r.Preset != "" && r.Ignore:
				c.errf(base, "preset and ignore are mutually exclusive")
			case r.Preset != "":
				if _, ok := seenPresets[r.Preset]; !ok {
					c.errf(append(base, "preset"), "references unknown preset slug %q", r.Preset)
				}
			}
			if r.When.Namespace == "" && r.When.Host == "" {
				c.errf(append(base, "when"), "at least one of namespace or host is required")
			}
		}
	}

	if cfg.DBBodyMaxChars < cfg.Slack.BodyMaxChars {
		c.errf([]any{"dbBodyMaxChars"},
			"%d must be >= slack.bodyMaxChars (%d)", cfg.DBBodyMaxChars, cfg.Slack.BodyMaxChars)
	}

	seenSlackChannels := map[string]struct{}{}
	if len(cfg.Slack.Channels) == 0 {
		c.errf([]any{"slack", "channels"}, "at least one channel is required")
	}
	for i, ch := range cfg.Slack.Channels {
		base := []any{"slack", "channels", i}
		if err := slug.Validate(ch.Slug); err != nil {
			c.errf(append(base, "slug"), "%v", err)
		}
		if _, dup := seenSlackChannels[ch.Slug]; dup {
			c.errf(append(base, "slug"), "duplicate slug %q", ch.Slug)
		}
		seenSlackChannels[ch.Slug] = struct{}{}
		if strings.HasPrefix(ch.ChannelID, "D") {
			c.errf(append(base, "channelId"), "DMs (D...) are not allowed")
		} else if !channelIDPattern.MatchString(ch.ChannelID) {
			c.errf(append(base, "channelId"), "%q does not match %s", ch.ChannelID, channelIDPattern.String())
		}
		if !envVarNamePattern.MatchString(ch.TokenEnv) {
			c.errf(append(base, "tokenEnv"),
				"%q must match ^[A-Z][A-Z0-9_]*$ (do not interpolate ${...} into this field)", ch.TokenEnv)
		}
	}

	// userMapping: slug regex + ID regex (U... user, S... subteam).
	for slugName, id := range cfg.Slack.UserMapping {
		if err := slug.Validate(slugName); err != nil {
			c.errf([]any{"slack", "userMapping", slugName}, "slug: %v", err)
		}
		if !userOrSubteamIDPattern.MatchString(id) {
			c.errf([]any{"slack", "userMapping", slugName},
				"%q must match %s (U... = user, S... = subteam)", id, userOrSubteamIDPattern.String())
		}
	}

	// Group validation: kube-discovered required; slugs unique and valid.
	seenGroups := map[string]struct{}{}
	hasKubeDiscovered := false
	for i, g := range cfg.Groups {
		base := []any{"groups", i}
		if err := slug.Validate(g.Slug); err != nil {
			c.errf(append(base, "slug"), "%v", err)
		}
		if _, dup := seenGroups[g.Slug]; dup {
			c.errf(append(base, "slug"), "duplicate slug %q", g.Slug)
		}
		seenGroups[g.Slug] = struct{}{}
		if g.Slug == "kube-discovered" {
			hasKubeDiscovered = true
		}
		for j, n := range g.Notify {
			if !c.isValidNotifyEntry(cfg.Slack.UserMapping, n) {
				c.errf(append(base, "notify", j),
					"%q must be a userMapping slug or raw Slack markup wrapped in <…>", n)
			}
		}
	}
	if !hasKubeDiscovered {
		c.errf([]any{"groups"}, "a group with slug %q is required", "kube-discovered")
	}

	// Monitor validation.
	seenMonitors := map[string]struct{}{}
	for i, m := range cfg.Monitors {
		base := []any{"monitors", i}
		if err := slug.Validate(m.Slug); err != nil {
			c.errf(append(base, "slug"), "%v", err)
		}
		if _, dup := seenMonitors[m.Slug]; dup {
			c.errf(append(base, "slug"), "duplicate slug %q", m.Slug)
		}
		seenMonitors[m.Slug] = struct{}{}
		if _, ok := seenGroups[m.Group]; !ok {
			c.errf(append(base, "group"), "unknown group %q", m.Group)
		}
		if m.Proxy != "" {
			if _, ok := seenProxies[m.Proxy]; !ok {
				c.errf(append(base, "proxy"), "unknown proxy slug %q", m.Proxy)
			}
		}
		if _, ok := seenSlackChannels[m.Slack]; !ok {
			c.errf(append(base, "slack"), "unknown channel slug %q", m.Slack)
		}
		for j, n := range m.Notify {
			if !c.isValidNotifyEntry(cfg.Slack.UserMapping, n) {
				c.errf(append(base, "notify", j),
					"%q must be a userMapping slug or raw Slack markup wrapped in <…>", n)
			}
		}
		interval := m.Interval.AsDuration()
		timeout := m.Timeout.AsDuration()
		backoff := m.RetryBackoff.AsDuration()
		if timeout >= interval {
			c.errf(base, "timeout (%s) must be less than interval (%s)", timeout, interval)
		}
		// retries × (timeout + retryBackoff) < interval
		retryWindow := time.Duration(m.Retries) * (timeout + backoff)
		if retryWindow >= interval {
			c.errf(base, "retries × (timeout + retryBackoff) = %s must be less than interval (%s)", retryWindow, interval)
		}
		// Forward references are allowed (YAML order is independent of
		// dependency order); the global pass below resolves unknown
		// slugs. Self-dependency is rejected here.
		for j, dep := range m.DependsOn {
			if dep == m.Slug {
				c.errf(append(base, "dependsOn", j), "monitor cannot depend on itself")
			}
		}

		// SSL thresholds: required for HTTPS URLs, allowed but ignored
		// for HTTP. When required and present, alert > escalation > 0.
		// tlsInsecureSkipVerify implies "don't track SSL expiry": the
		// thresholds aren't required (and would be ignored anyway since
		// the SSL state machine is bypassed for that monitor).
		if strings.HasPrefix(m.URL, "https://") && !m.TLSInsecureSkipVerify {
			if m.SSLAlertThreshold.AsDuration() <= 0 {
				c.errf(append(base, "sslAlertThreshold"), "required for HTTPS monitors")
			}
			if m.SSLEscalationThreshold.AsDuration() <= 0 {
				c.errf(append(base, "sslEscalationThreshold"), "required for HTTPS monitors")
			}
			if m.SSLReminderInterval.AsDuration() <= 0 {
				c.errf(append(base, "sslReminderInterval"), "required for HTTPS monitors")
			}
			if m.SSLAlertThreshold.AsDuration() > 0 && m.SSLEscalationThreshold.AsDuration() > 0 &&
				m.SSLAlertThreshold.AsDuration() <= m.SSLEscalationThreshold.AsDuration() {
				c.errf(append(base, "sslAlertThreshold"),
					"must be strictly greater than sslEscalationThreshold (%s)",
					m.SSLEscalationThreshold.AsDuration())
			}
		}
	}

	// Global dependsOn pass: every reference resolves to a known
	// static monitor, and the graph has no cycles. Done after the
	// per-monitor pass so we have the full slug set.
	monitorByIdx := map[string]int{}
	for i, m := range cfg.Monitors {
		monitorByIdx[m.Slug] = i
	}
	for i, m := range cfg.Monitors {
		base := []any{"monitors", i}
		for j, dep := range m.DependsOn {
			if dep == m.Slug {
				continue // already reported above
			}
			if _, ok := monitorByIdx[dep]; !ok {
				c.errf(append(base, "dependsOn", j), "unknown monitor slug %q (parents must be declared static monitors)", dep)
			}
		}
	}
	if cycle := detectDependsOnCycle(cfg.Monitors); cycle != "" {
		c.errf([]any{"monitors"}, "dependsOn graph contains a cycle: %s", cycle)
	}
}

// detectDependsOnCycle runs a DFS over the monitor dependency graph
// and returns a human-readable description of the first cycle found,
// or "" if the graph is acyclic.
func detectDependsOnCycle(monitors []Monitor) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	parents := map[string][]string{}
	for _, m := range monitors {
		color[m.Slug] = white
		parents[m.Slug] = m.DependsOn
	}
	var path []string
	var dfs func(node string) string
	dfs = func(node string) string {
		switch color[node] {
		case gray:
			// Found a back-edge — extract the cycle from path.
			for i, s := range path {
				if s == node {
					return strings.Join(append(path[i:], node), " → ")
				}
			}
			return node + " → " + node
		case black:
			return ""
		}
		color[node] = gray
		path = append(path, node)
		for _, p := range parents[node] {
			if _, known := color[p]; !known {
				continue // unknown slug — already reported separately
			}
			if c := dfs(p); c != "" {
				return c
			}
		}
		path = path[:len(path)-1]
		color[node] = black
		return ""
	}
	for _, m := range monitors {
		if color[m.Slug] == white {
			if c := dfs(m.Slug); c != "" {
				return c
			}
		}
	}
	return ""
}

// nodeAt walks the yaml.Node tree following a path of mapping keys
// (string) and sequence indices (int). Returns the value node at the
// given path, or nil if missing.
func nodeAt(root *yaml.Node, path ...any) *yaml.Node {
	if root == nil {
		return nil
	}
	cur := root
	if cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	for _, p := range path {
		if cur == nil {
			return nil
		}
		switch v := p.(type) {
		case string:
			cur = mappingValue(cur, v)
		case int:
			cur = sequenceItem(cur, v)
		default:
			return nil
		}
	}
	return cur
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func sequenceItem(node *yaml.Node, idx int) *yaml.Node {
	if node.Kind != yaml.SequenceNode || idx < 0 || idx >= len(node.Content) {
		return nil
	}
	return node.Content[idx]
}

func pathStr(path []any) string {
	var b strings.Builder
	for i, p := range path {
		switch v := p.(type) {
		case string:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		case int:
			fmt.Fprintf(&b, "[%d]", v)
		}
	}
	return b.String()
}

var envVarNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// isRawSlackMarkup reports whether s is a verbatim <…> Slack mention
// markup string (e.g. <!here>, <@U123ABC>, <!subteam^S456>).
func isRawSlackMarkup(s string) bool {
	return len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>'
}

// isValidNotifyEntry accepts either a userMapping slug or raw Slack
// markup. Other strings are rejected at config-load.
func (c *checker) isValidNotifyEntry(mapping map[string]string, s string) bool {
	if _, ok := mapping[s]; ok {
		return true
	}
	return isRawSlackMarkup(s)
}
