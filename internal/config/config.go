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
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

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
	StatusPages     []StatusPage `yaml:"statusPages,omitempty"` // optional; empty → /status renders empty placeholder
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
//
// Every monitor materialized from a discovered Ingress is the merge of
// one or more rules in the Match tree. See ADR-0002 and
// docs/config-schema.md §"Kube auto-discovery (`kube.match` cascade)".
type Kube struct {
	ResyncInterval Duration        `yaml:"resyncInterval"`
	Match          []KubeMatchRule `yaml:"match,omitempty"`

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

// KubeMatchRule is one node in the cascading kube.match tree. See
// ADR-0002 for the full semantics. `When` is the selector; `Config`
// contributes to the merge stack when this rule matches; `Nested`
// holds child rules traversed only when this rule matches; `Ignore`
// and `Final` are rule-level directives that cascade like scalars.
//
// `When`, `Config`, and `Nested` may all be absent (e.g. a marker
// rule that only carries `ignore: true`).
type KubeMatchRule struct {
	When   KubeMatchWhen   `yaml:"when,omitempty"`
	Ignore *bool           `yaml:"ignore,omitempty"`
	Final  bool            `yaml:"final,omitempty"`
	Config KubeConfig      `yaml:"config,omitempty"`
	Nested []KubeMatchRule `yaml:"nested,omitempty"`
}

// KubeMatchWhen is the selector vocabulary for a kube.match rule.
// All set fields AND together. Glob fields use path.Match semantics;
// regex fields are Go regexp values auto-anchored as ^…$ at
// validation / use time.
//
// Setting both glob and regex for the same dimension (namespace +
// namespaceRegex, or host + hostRegex) is a validation error.
type KubeMatchWhen struct {
	Namespace      string            `yaml:"namespace,omitempty"`
	NamespaceRegex string            `yaml:"namespaceRegex,omitempty"`
	Host           string            `yaml:"host,omitempty"`
	HostRegex      string            `yaml:"hostRegex,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty"`
}

// KubeConfig is the monitor-field block inside a kube.match rule.
// Every field is optional at the YAML level — the cascade lets
// descendants override only what they care about. The root rule
// (top-level rule with empty `when:`) must carry every required
// field; that constraint is enforced by validation, not by the type.
//
// Fields are tracked via setFields so the merger can distinguish
// "unset" from "explicitly set to the zero value". Use
// KubeConfig.IsSet(fieldName) to check; the field name matches the
// YAML key (e.g. "path", "httpMethod", "acceptedStatusCodes").
type KubeConfig struct {
	Scheme                 string         `yaml:"scheme,omitempty"`
	Path                   string         `yaml:"path,omitempty"`
	HTTPMethod             string         `yaml:"httpMethod,omitempty"`
	AcceptedStatusCodes    StatusCodeList `yaml:"acceptedStatusCodes,omitempty"`
	Interval               Duration       `yaml:"interval,omitempty"`
	Timeout                Duration       `yaml:"timeout,omitempty"`
	Retries                int            `yaml:"retries,omitempty"`
	RetryBackoff           Duration       `yaml:"retryBackoff,omitempty"`
	FollowRedirects        bool           `yaml:"followRedirects,omitempty"`
	TLSInsecureSkipVerify  bool           `yaml:"tlsInsecureSkipVerify,omitempty"`
	Proxy                  string         `yaml:"proxy,omitempty"`
	ReminderInterval       Duration       `yaml:"reminderInterval,omitempty"`
	SSLAlertThreshold      Duration       `yaml:"sslAlertThreshold,omitempty"`
	SSLEscalationThreshold Duration       `yaml:"sslEscalationThreshold,omitempty"`
	SSLReminderInterval    Duration       `yaml:"sslReminderInterval,omitempty"`
	Slack                  string         `yaml:"slack,omitempty"`
	Notify                 NotifyList     `yaml:"notify,omitempty"`
	Tags                   TagList        `yaml:"tags,omitempty"`
	DependsOn              DependsOnList  `yaml:"dependsOn,omitempty"`
	Group                  string         `yaml:"group,omitempty"`

	// setFields records which YAML keys were present in the input.
	// Populated by UnmarshalYAML; consumed by the merger to tell
	// "unset" apart from "explicitly set to the zero value".
	setFields map[string]bool `yaml:"-"`
}

// IsSet reports whether the given YAML key was present on this
// KubeConfig block in the source YAML. Keys are the lower-camel-case
// names matching the yaml struct tags (e.g. "path", "httpMethod").
func (k *KubeConfig) IsSet(field string) bool {
	if k == nil {
		return false
	}
	return k.setFields[field]
}

// UnmarshalYAML decodes the KubeConfig and records which keys were
// present so the merger can distinguish unset fields from
// zero-valued ones.
func (k *KubeConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: kube.match[].config must be a mapping", node.Line)
	}
	// Decode the value into a shadow type that has no UnmarshalYAML,
	// to avoid infinite recursion.
	type kubeConfigShadow KubeConfig
	var shadow kubeConfigShadow
	if err := node.Decode(&shadow); err != nil {
		return err
	}
	*k = KubeConfig(shadow)
	k.setFields = make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind == yaml.ScalarNode {
			k.setFields[key.Value] = true
		}
	}
	return nil
}

// StatusCodeList is the list type for acceptedStatusCodes. It's a
// distinct named type (rather than a bare []int) so the parsing
// surface stays symmetrical with the other typed list wrappers
// (NotifyList, TagList, DependsOnList). Unlike those, it has no
// Override flag — acceptedStatusCodes is replace-by-default across
// the cascade (see ADR-0002 §"Merge rules").
type StatusCodeList []int

// NotifyList is the !override-aware list type for kube config
// notify entries. When Override is true the deeper rule's values
// replace ancestors' values; otherwise the merger unions the lists.
type NotifyList struct {
	Values   []string
	Override bool
}

// TagList — same shape as NotifyList, for tags.
type TagList struct {
	Values   []string
	Override bool
}

// DependsOnList — same shape as NotifyList, for dependsOn.
type DependsOnList struct {
	Values   []string
	Override bool
}

// overrideTag is the YAML custom tag that flips a list from
// union-by-default to replace.
const overrideTag = "!override"

// decodeOverridableStringList is the shared UnmarshalYAML body for
// NotifyList, TagList, and DependsOnList. It accepts a YAML sequence,
// optionally tagged !override, and decodes its content into a string
// slice.
func decodeOverridableStringList(node *yaml.Node, values *[]string, override *bool) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: expected a sequence, got %s", node.Line, nodeKindName(node.Kind))
	}
	if node.Tag == overrideTag {
		*override = true
	}
	// Decode the sequence content directly — yaml.v3 keeps the tag on
	// the parent node but the children decode as plain strings.
	out := make([]string, 0, len(node.Content))
	for i, child := range node.Content {
		if child.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: list entry %d must be a scalar string", child.Line, i)
		}
		out = append(out, child.Value)
	}
	*values = out
	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler. Sets Override=true when
// the YAML sequence carries the !override custom tag.
func (l *NotifyList) UnmarshalYAML(node *yaml.Node) error {
	return decodeOverridableStringList(node, &l.Values, &l.Override)
}

// UnmarshalYAML implements yaml.Unmarshaler. Sets Override=true when
// the YAML sequence carries the !override custom tag.
func (l *TagList) UnmarshalYAML(node *yaml.Node) error {
	return decodeOverridableStringList(node, &l.Values, &l.Override)
}

// UnmarshalYAML implements yaml.Unmarshaler. Sets Override=true when
// the YAML sequence carries the !override custom tag.
func (l *DependsOnList) UnmarshalYAML(node *yaml.Node) error {
	return decodeOverridableStringList(node, &l.Values, &l.Override)
}

func nodeKindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
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

// StatusPage is one public status page. Each entry in statusPages
// gets its own /status/<slug> URL; /status itself lists every
// configured page. Slug is required and must be unique across the
// list.
type StatusPage struct {
	Slug          string              `yaml:"slug"`
	Title         string              `yaml:"title,omitempty"`
	ShowSections  *bool               `yaml:"showSections,omitempty"`  // default true
	ShowIncidents *bool               `yaml:"showIncidents,omitempty"` // default false (privacy)
	Sections      []StatusPageSection `yaml:"sections,omitempty"`
}

// StatusPageSection is one named block on the status page. The match
// list is OR: a monitor lands in the section if any one selector
// matches. Within a selector the fields AND together.
type StatusPageSection struct {
	Title string            `yaml:"title"`
	Match []StatusPageMatch `yaml:"match"`
}

// StatusPageMatch is the per-monitor selector. host is a glob
// (path.Match, same as kube.match[].when.host); group is an exact
// slug match while groupRegex is a Go regexp matched against the
// monitor's group slug; tags matches if the monitor carries any of
// the listed labels. group and groupRegex are mutually exclusive
// per selector.
type StatusPageMatch struct {
	Host       string   `yaml:"host,omitempty"`
	Group      string   `yaml:"group,omitempty"`
	GroupRegex string   `yaml:"groupRegex,omitempty"`
	Tags       []string `yaml:"tags,omitempty"`
}

// ShowSectionsEnabled returns the flag value, defaulting to true.
func (s StatusPage) ShowSectionsEnabled() bool {
	if s.ShowSections == nil {
		return true
	}
	return *s.ShowSections
}

// ShowIncidentsEnabled returns the flag value, defaulting to false —
// the public page errs on the side of less data exposure unless the
// operator explicitly opts in.
func (s StatusPage) ShowIncidentsEnabled() bool {
	if s.ShowIncidents == nil {
		return false
	}
	return *s.ShowIncidents
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
	Tags                  []string `yaml:"tags,omitempty"`      // free-form labels; consumed by status[].match[].tags selectors

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
	"statusPages": {},
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

	// Group validation: kube-discovered required; slugs unique and valid.
	// Done before kube + monitors so both can validate the slug references
	// they make into the groups set.
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

	// kube.match validation: see ADR-0002 §Validation. Structural
	// errors only — resolved-value errors (interval/timeout, SSL
	// thresholds, root-required-field-overridden-empty) live in the
	// merger and surface at materialization time as kube-invalid
	// discovery rows. Slug references inside config blocks need the
	// slack channel / group / proxy / userMapping sets, so this runs
	// after those have been populated above.
	c.validateKube(cfg, seenSlackChannels, seenGroups, seenProxies)

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

	// Kube dependsOn resolution: every entry in a config.dependsOn
	// list (anywhere in the tree) must resolve to a declared static
	// monitor — the schema explicitly disallows kube-discovered
	// parents because their slugs aren't known until reconcile time.
	if cfg.Kube != nil {
		c.validateKubeDependsOnRefs(cfg.Kube.Match, monitorByIdx, []any{"kube", "match"})
	}

	// statusPages validation. Per page: slug required + unique across
	// the list. Per section: title + non-empty match; each selector
	// must specify at least one of host/group/tags, and any referenced
	// group must exist.
	seenStatusSlugs := map[string]int{}
	for pi, page := range cfg.StatusPages {
		pbase := []any{"statusPages", pi}
		if err := slug.Validate(page.Slug); err != nil {
			c.errf(append(pbase, "slug"), "%v", err)
		}
		if prev, dup := seenStatusSlugs[page.Slug]; dup && page.Slug != "" {
			c.errf(append(pbase, "slug"), "duplicate slug %q (also at statusPages[%d])", page.Slug, prev)
		}
		if page.Slug != "" {
			seenStatusSlugs[page.Slug] = pi
		}
		if len(page.Sections) == 0 {
			c.errf(append(pbase, "sections"), "at least one section is required")
		}
		for i, sec := range page.Sections {
			sbase := append(pbase, "sections", i)
			if strings.TrimSpace(sec.Title) == "" {
				c.errf(append(sbase, "title"), "required")
			}
			if len(sec.Match) == 0 {
				c.errf(append(sbase, "match"), "at least one selector is required")
			}
			for j, sel := range sec.Match {
				mbase := append(sbase, "match", j)
				if sel.Host == "" && sel.Group == "" && sel.GroupRegex == "" && len(sel.Tags) == 0 {
					c.errf(mbase, "at least one of host, group, groupRegex, or tags is required")
				}
				if sel.Group != "" && sel.GroupRegex != "" {
					c.errf(mbase, "group and groupRegex are mutually exclusive")
				}
				if sel.Group != "" {
					if _, ok := seenGroups[sel.Group]; !ok {
						c.errf(append(mbase, "group"), "unknown group %q", sel.Group)
					}
				}
				if sel.GroupRegex != "" {
					if _, err := regexp.Compile(sel.GroupRegex); err != nil {
						c.errf(append(mbase, "groupRegex"), "invalid regex: %v", err)
					}
				}
			}
		}
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

// validKubeHTTPMethods is the allowed httpMethod enum for kube
// monitor configs (see docs/config-schema.md §"kube.match[].config").
var validKubeHTTPMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "DELETE": {},
}

// validKubeSchemes is the allowed scheme enum.
var validKubeSchemes = map[string]struct{}{
	"http": {}, "https": {},
}

// KubeRequiredAtRoot is the set of KubeConfig YAML keys the root rule
// (top-level rule with empty when:) must explicitly set. See
// docs/config-schema.md §"kube.match[].config fields" — the column
// "Req at root". Ordered for stable error-message ordering.
//
// Exported so the merger can re-check the same set against the
// resolved KubeConfig after the merge stack collapses — children
// must not have overridden a required-at-root field to an
// empty/invalid value.
var KubeRequiredAtRoot = []string{
	"path",
	"httpMethod",
	"acceptedStatusCodes",
	"interval",
	"timeout",
	"retries",
	"retryBackoff",
	"followRedirects",
	"reminderInterval",
	"sslAlertThreshold",
	"sslEscalationThreshold",
	"sslReminderInterval",
	"slack",
}

// validateKube enforces the structural rules in ADR-0002 §Validation
// against the kube block. It is a no-op when cfg.Kube is nil.
//
// Structural errors enforced here block startup. Resolved-value
// errors (interval >= timeout, SSL alert > escalation > 0, a
// required-at-root field overridden to invalid deeper in the tree)
// are deferred to the merger — they depend on which ingresses
// actually exist and surface as kube-invalid discovery rows.
//
// TODO(warnings): the ADR also defines two structural *warnings*
// (empty when: deeper than root; ignore:true at a leaf with a
// non-empty config block). The config layer has no warning channel
// today; wiring one touches every validator, so warnings are
// deliberately deferred to a separate task. The ADR explicitly
// distinguishes errors from warnings — skipping warnings does not
// regress behaviour.
func (c *checker) validateKube(cfg *Config, slackChannels, groups, proxies map[string]struct{}) {
	if cfg.Kube == nil {
		return
	}
	k := cfg.Kube

	// friendlyName: empty falls back to "compact" at use time, so an
	// unset value is fine; if set it must be one of the documented
	// styles.
	if k.FriendlyName != "" {
		ok := false
		for _, s := range KubeFriendlyNameStyles {
			if s == k.FriendlyName {
				ok = true
				break
			}
		}
		if !ok {
			c.errf([]any{"kube", "friendlyName"},
				"%q is not one of %v", k.FriendlyName, KubeFriendlyNameStyles)
		}
	}

	// resyncInterval lower bound — schema §1 / §"kube.* field reference".
	if k.ResyncInterval.AsDuration() < time.Minute {
		c.errf([]any{"kube", "resyncInterval"},
			"must be >= 1m, got %s", k.ResyncInterval)
	}

	// Match list must be non-empty and the first entry must be the
	// root baseline (empty when:).
	if len(k.Match) == 0 {
		c.errf([]any{"kube", "match"}, "at least one rule is required; the first rule must have an empty when: and carry every required-at-root field")
		return
	}
	root := k.Match[0]
	if !kubeWhenIsEmpty(root.When) {
		c.errf([]any{"kube", "match", 0, "when"},
			"the first rule must have an empty when: (the mandatory root baseline carrying every required-at-root field)")
	} else {
		// Only check required-at-root fields when the rule really is
		// the root. If it isn't, the missing-fields error would be
		// noise on top of the (more actionable) "root must be empty"
		// error.
		c.checkKubeRequiredAtRoot(&root.Config, slackChannels, []any{"kube", "match", 0, "config"})
	}

	// Walk every rule (including the root) for selector / config
	// validity. The root already had its required-field check above;
	// validateKubeRule does the remaining per-rule checks (selectors,
	// label keys, slug references inside config, final/ignore
	// invariants, HTTPMethod / scheme enums when set).
	for i := range k.Match {
		c.validateKubeRule(&k.Match[i], []any{"kube", "match", i}, slackChannels, groups, proxies, cfg.Slack.UserMapping)
	}
}

// kubeWhenIsEmpty reports whether every selector field on w is unset
// (zero / nil). Used to detect the mandatory root baseline.
func kubeWhenIsEmpty(w KubeMatchWhen) bool {
	return w.Namespace == "" && w.NamespaceRegex == "" &&
		w.Host == "" && w.HostRegex == "" && len(w.Labels) == 0
}

// checkKubeRequiredAtRoot verifies the root rule's Config sets every
// field marked "Req at root" in docs/config-schema.md. IsSet
// distinguishes "explicitly set" from "unset / zero-value" — needed
// for fields like followRedirects where the zero value (false) is a
// legitimate explicit choice.
func (c *checker) checkKubeRequiredAtRoot(k *KubeConfig, slackChannels map[string]struct{}, base []any) {
	for _, key := range KubeRequiredAtRoot {
		if !k.IsSet(key) {
			c.errf(append(append([]any{}, base...), key),
				"required at the root rule (every materialized monitor inherits this)")
			continue
		}
		// Beyond mere presence, every required-at-root field has a
		// sanity floor: empty strings, empty lists, and non-positive
		// numerics at the *root* are useless — descendants would have
		// nothing to inherit. Resolved-value error surface (Task 4)
		// covers the same checks again post-merge, but catching them
		// at the root early gives a much better error.
		switch key {
		case "path":
			if k.Path == "" || !strings.HasPrefix(k.Path, "/") {
				c.errf(append(append([]any{}, base...), key),
					"must start with %q, got %q", "/", k.Path)
			}
		case "httpMethod":
			if _, ok := validKubeHTTPMethods[k.HTTPMethod]; !ok {
				c.errf(append(append([]any{}, base...), key),
					"must be one of GET, HEAD, POST, PUT, DELETE, got %q", k.HTTPMethod)
			}
		case "acceptedStatusCodes":
			if len(k.AcceptedStatusCodes) == 0 {
				c.errf(append(append([]any{}, base...), key), "must be a non-empty list of HTTP status codes")
			}
			for i, code := range k.AcceptedStatusCodes {
				if code < 100 || code > 599 {
					c.errf(append(append([]any{}, base...), key, i),
						"%d is not a valid HTTP status code (100..599)", code)
				}
			}
		case "interval":
			if k.Interval.AsDuration() < 30*time.Second {
				c.errf(append(append([]any{}, base...), key), "must be >= 30s, got %s", k.Interval)
			}
		case "timeout":
			if k.Timeout.AsDuration() <= 0 {
				c.errf(append(append([]any{}, base...), key), "must be > 0, got %s", k.Timeout)
			}
		case "retries":
			if k.Retries < 0 {
				c.errf(append(append([]any{}, base...), key), "must be >= 0, got %d", k.Retries)
			}
		case "retryBackoff":
			if k.RetryBackoff.AsDuration() < time.Second {
				c.errf(append(append([]any{}, base...), key), "must be >= 1s, got %s", k.RetryBackoff)
			}
		case "followRedirects":
			// Presence-only check (already covered by IsSet above);
			// false is a perfectly valid value.
		case "reminderInterval":
			if k.ReminderInterval.AsDuration() < time.Hour {
				c.errf(append(append([]any{}, base...), key), "must be >= 1h, got %s", k.ReminderInterval)
			}
		case "sslAlertThreshold":
			if k.SSLAlertThreshold.AsDuration() <= 0 {
				c.errf(append(append([]any{}, base...), key), "must be > 0, got %s", k.SSLAlertThreshold)
			}
		case "sslEscalationThreshold":
			if k.SSLEscalationThreshold.AsDuration() <= 0 {
				c.errf(append(append([]any{}, base...), key), "must be > 0, got %s", k.SSLEscalationThreshold)
			}
		case "sslReminderInterval":
			if k.SSLReminderInterval.AsDuration() < time.Hour {
				c.errf(append(append([]any{}, base...), key), "must be >= 1h, got %s", k.SSLReminderInterval)
			}
		case "slack":
			// Slug-reference resolution is shared with descendants and
			// runs inside validateKubeRule; the IsSet check above is
			// what makes "required at root" meaningful.
		}
	}
}

// validateKubeRule applies every per-rule structural check at base
// (the path slice to this rule), then recurses into Nested with
// updated paths.
func (c *checker) validateKubeRule(
	r *KubeMatchRule,
	base []any,
	slackChannels, groups, proxies map[string]struct{},
	userMapping map[string]string,
) {
	c.validateKubeWhen(&r.When, append(append([]any{}, base...), "when"))

	// final: true with an empty when: would halt the tree walk on the
	// first ingress encountered — almost certainly a typo. The root
	// rule's missing-selector error already fires above; this one
	// catches the same shape anywhere else in the tree.
	if r.Final && kubeWhenIsEmpty(r.When) {
		c.errf(append(append([]any{}, base...), "final"),
			"final: true requires at least one selector in when: (otherwise the tree walk halts on the first ingress)")
	}

	c.validateKubeConfig(&r.Config, append(append([]any{}, base...), "config"), slackChannels, groups, proxies, userMapping)

	for i := range r.Nested {
		c.validateKubeRule(&r.Nested[i],
			append(append([]any{}, base...), "nested", i),
			slackChannels, groups, proxies, userMapping)
	}
}

// validateKubeWhen enforces selector-level rules: glob/regex mutual
// exclusion per dimension, glob parse, regex parse, label-key syntax.
func (c *checker) validateKubeWhen(w *KubeMatchWhen, base []any) {
	if w.Namespace != "" && w.NamespaceRegex != "" {
		c.errf(base, "namespace and namespaceRegex are mutually exclusive in the same when:")
	}
	if w.Host != "" && w.HostRegex != "" {
		c.errf(base, "host and hostRegex are mutually exclusive in the same when:")
	}
	if w.Namespace != "" {
		if _, err := path.Match(w.Namespace, ""); err != nil {
			c.errf(append(append([]any{}, base...), "namespace"),
				"invalid glob %q: %v", w.Namespace, err)
		}
	}
	if w.Host != "" {
		if _, err := path.Match(w.Host, ""); err != nil {
			c.errf(append(append([]any{}, base...), "host"),
				"invalid glob %q: %v", w.Host, err)
		}
	}
	if w.NamespaceRegex != "" {
		if _, err := regexp.Compile(w.NamespaceRegex); err != nil {
			c.errf(append(append([]any{}, base...), "namespaceRegex"),
				"invalid regex: %v", err)
		}
	}
	if w.HostRegex != "" {
		if _, err := regexp.Compile(w.HostRegex); err != nil {
			c.errf(append(append([]any{}, base...), "hostRegex"),
				"invalid regex: %v", err)
		}
	}
	for key := range w.Labels {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			c.errf(append(append([]any{}, base...), "labels", key),
				"invalid k8s label key %q: %s", key, strings.Join(errs, "; "))
		}
	}
}

// validateKubeConfig runs cross-reference + enum checks on a single
// config block (anywhere in the tree). Required-at-root presence is
// handled separately in checkKubeRequiredAtRoot; this routine only
// validates fields that are actually set, so descendants stay free to
// omit anything they don't override.
func (c *checker) validateKubeConfig(
	k *KubeConfig,
	base []any,
	slackChannels, groups, proxies map[string]struct{},
	userMapping map[string]string,
) {
	if k.IsSet("httpMethod") {
		if _, ok := validKubeHTTPMethods[k.HTTPMethod]; !ok {
			c.errf(append(append([]any{}, base...), "httpMethod"),
				"must be one of GET, HEAD, POST, PUT, DELETE, got %q", k.HTTPMethod)
		}
	}
	if k.IsSet("scheme") {
		if _, ok := validKubeSchemes[k.Scheme]; !ok {
			c.errf(append(append([]any{}, base...), "scheme"),
				"must be %q or %q, got %q", "http", "https", k.Scheme)
		}
	}
	if k.IsSet("path") && !strings.HasPrefix(k.Path, "/") {
		c.errf(append(append([]any{}, base...), "path"),
			"must start with %q, got %q", "/", k.Path)
	}
	if k.IsSet("slack") && k.Slack != "" {
		if _, ok := slackChannels[k.Slack]; !ok {
			c.errf(append(append([]any{}, base...), "slack"),
				"unknown channel slug %q", k.Slack)
		}
	}
	if k.IsSet("group") && k.Group != "" {
		if _, ok := groups[k.Group]; !ok {
			c.errf(append(append([]any{}, base...), "group"),
				"unknown group %q", k.Group)
		}
	}
	if k.IsSet("proxy") && k.Proxy != "" {
		if _, ok := proxies[k.Proxy]; !ok {
			c.errf(append(append([]any{}, base...), "proxy"),
				"unknown proxy slug %q", k.Proxy)
		}
	}
	if k.IsSet("notify") {
		for i, n := range k.Notify.Values {
			if !c.isValidNotifyEntry(userMapping, n) {
				c.errf(append(append([]any{}, base...), "notify", i),
					"%q must be a userMapping slug or raw Slack markup wrapped in <…>", n)
			}
		}
	}
	if k.IsSet("tags") {
		for i, tag := range k.Tags.Values {
			if err := slug.Validate(tag); err != nil {
				c.errf(append(append([]any{}, base...), "tags", i),
					"%v", err)
			}
		}
	}
	if k.IsSet("acceptedStatusCodes") {
		for i, code := range k.AcceptedStatusCodes {
			if code < 100 || code > 599 {
				c.errf(append(append([]any{}, base...), "acceptedStatusCodes", i),
					"%d is not a valid HTTP status code (100..599)", code)
			}
		}
	}
	// dependsOn cross-references with static monitors are resolved in
	// a second pass once the monitor slug set is fully built — see
	// validateKubeDependsOnRefs called from validate().
}

// validateKubeDependsOnRefs walks the tree and verifies every entry in
// any config.dependsOn list resolves to a declared static monitor.
// Kube-discovered monitors cannot be dependsOn parents because their
// slugs are not known at config-load time.
func (c *checker) validateKubeDependsOnRefs(rules []KubeMatchRule, monitorByIdx map[string]int, base []any) {
	for i := range rules {
		r := &rules[i]
		rbase := append(append([]any{}, base...), i)
		if r.Config.IsSet("dependsOn") {
			for j, dep := range r.Config.DependsOn.Values {
				if _, ok := monitorByIdx[dep]; !ok {
					c.errf(append(append([]any{}, rbase...), "config", "dependsOn", j),
						"unknown monitor slug %q (kube dependsOn parents must be declared static monitors)", dep)
				}
			}
		}
		if len(r.Nested) > 0 {
			c.validateKubeDependsOnRefs(r.Nested, monitorByIdx, append(append([]any{}, rbase...), "nested"))
		}
	}
}
