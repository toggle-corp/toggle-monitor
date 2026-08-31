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
	"net/url"
	"path"
	"regexp"
	"strconv"
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

// colorHexPattern matches a 6-digit hex color literal (#rrggbb). 3-digit
// shorthand and named colours are rejected — see ADR-0003.
var colorHexPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Config is the typed, validated representation of the toggle-monitor
// YAML config.
type Config struct {
	DisplayTimezone string              `yaml:"displayTimezone"`
	PublicBaseURL   string              `yaml:"publicBaseURL,omitempty"`
	DBBodyMaxChars  int                 `yaml:"dbBodyMaxChars"`
	Database        Database            `yaml:"database"`
	UI              UI                  `yaml:"ui"`
	HTTPClient      HTTPClient          `yaml:"httpClient"`
	Heartbeat       *Heartbeat          `yaml:"heartbeat,omitempty"` // optional; nil disables the deadman loop
	Kube            *Kube               `yaml:"kube,omitempty"`      // optional; nil disables auto-discovery
	Sentry          *Sentry             `yaml:"sentry,omitempty"`    // optional; nil disables Sentry forwarding
	Slack           Slack               `yaml:"slack"`
	SelfHealth      *SelfHealth         `yaml:"selfHealth,omitempty"`   // optional; nil disables self-health degraded mode (ADR-0008)
	Alertmanager    *AlertmanagerConfig `yaml:"alertmanager,omitempty"` // optional; nil disables the AM webhook receiver (ADR-0005)
	Proxies         []Proxy             `yaml:"proxies,omitempty"`
	Monitors        []Monitor           `yaml:"monitors"`
	SMTPMonitors    []SMTPMonitor       `yaml:"smtpMonitors,omitempty"` // optional; static-only SMTP probes
	StatusPages     []StatusPage        `yaml:"statusPages,omitempty"`  // optional; empty → /status renders empty listing
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

// Sentry is the optional error-tracking block. When nil (or
// Enabled=false), the binary's slog→Sentry bridge is a no-op and
// nothing is shipped. When Enabled=true, the env var named by DSNEnv
// must resolve to a non-empty DSN at startup (checked in cli/serve.go
// alongside the database/Slack env vars).
type Sentry struct {
	Enabled          bool    `yaml:"enabled"`
	DSNEnv           string  `yaml:"dsnEnv"`
	Environment      string  `yaml:"environment,omitempty"`      // default: "production"
	SampleRate       float64 `yaml:"sampleRate,omitempty"`       // default: 1.0; [0.0..1.0]
	TracesSampleRate float64 `yaml:"tracesSampleRate,omitempty"` // default: 0.0; [0.0..1.0]
	ServerName       string  `yaml:"serverName,omitempty"`       // default: os.Hostname()
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

	// WatchDebounce is how long the watcher waits after the first Ingress
	// event of a burst before reconciling, so a removed Ingress is torn
	// down within seconds rather than at the next resyncInterval — before
	// the burst dispatcher's pendingWait elapses and reports the resulting
	// 404 as a fresh incident. It also caps the added reconcile rate at
	// one pass per window during churn (a rolling deploy), and acts as a
	// settle window: a delete followed by a recreate inside the window
	// changes nothing.
	//
	// A pointer so an explicit `0s` (disable watch-driven reconciles;
	// resyncInterval becomes the only trigger) is distinguishable from an
	// omitted key (DefaultKubeWatchDebounce).
	WatchDebounce *Duration `yaml:"watchDebounce,omitempty"`

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

// Documented bounds and default for kube.watchDebounce. The default sits
// well inside the dispatcher's default pendingWait (30s), which is the
// point of the knob: teardown has to land before the burst dispatcher
// decides what to say about the resulting 404. The bounds are only sanity
// rails — a window at the upper end outlives a default pendingWait and
// gives up that ordering, while the lower one stops a config typo turning
// cluster churn into a reconcile hot loop.
const (
	DefaultKubeWatchDebounce = 5 * time.Second
	MinKubeWatchDebounce     = time.Second
	MaxKubeWatchDebounce     = time.Minute
)

// EffectiveWatchDebounce returns the configured watchDebounce, or
// DefaultKubeWatchDebounce when the key is omitted. An explicit `0s`
// is returned as zero, which disables watch-driven reconciles.
func (k Kube) EffectiveWatchDebounce() time.Duration {
	if k.WatchDebounce == nil {
		return DefaultKubeWatchDebounce
	}
	return k.WatchDebounce.AsDuration()
}

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

	// Annotations and NamespaceAnnotations select on the two scopes
	// ADR-0009 named: the Ingress itself and its Namespace. Matching is
	// the same as Labels — every pair must be present and equal — which
	// is what lets an app team's own annotation mean whatever the
	// operator's rule says it means, and nothing more (ADR-0014).
	Annotations          map[string]string `yaml:"annotations,omitempty"`
	NamespaceAnnotations map[string]string `yaml:"namespaceAnnotations,omitempty"`
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

	// *From blocks source a field's value from an Ingress or Namespace
	// annotation instead of a literal (ADR-0009). Each one contributes
	// at this rule's position in the cascade with exactly the merge
	// semantics of the literal field it supplies, so the tree stays
	// authoritative over *which* field is set where.
	PathFrom           *ValueSource `yaml:"pathFrom,omitempty"`
	SlackFrom          *ValueSource `yaml:"slackFrom,omitempty"`
	NotifyFrom         *ValueSource `yaml:"notifyFrom,omitempty"`
	NotifyOverrideFrom *ValueSource `yaml:"notifyOverrideFrom,omitempty"`
	TagsFrom           *ValueSource `yaml:"tagsFrom,omitempty"`
	TagsOverrideFrom   *ValueSource `yaml:"tagsOverrideFrom,omitempty"`

	// AcceptedStatusCodesFrom has no Override twin: acceptedStatusCodes
	// is replace-by-default across the cascade, so every layer that sets
	// it already replaces the previous one.
	AcceptedStatusCodesFrom *ValueSource `yaml:"acceptedStatusCodesFrom,omitempty"`

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
//
// YAML merge keys (`<<: *anchor`) are expanded recursively when
// populating setFields so anchor-side keys are recorded as "set".
// Without this, `config: { <<: *defaults }` would decode correctly
// (the struct fields land via yaml.v3's merge handling at Decode
// time) but `IsSet("path")` would return false because the raw AST
// only carries the `<<` key — and the validator would then complain
// the root rule is missing every required-at-root field.
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
	k.setFields = make(map[string]bool)
	collectMappingKeys(node, k.setFields)
	return nil
}

// collectMappingKeys walks a mapping node and records every present
// key in set. YAML merge keys (`<<:`) are followed recursively —
// either a single alias (`<<: *a`), a list of aliases (`<<: [*a,*b]`),
// or an inline mapping — so the resulting set reflects the same
// fields yaml.v3 would populate at Decode time.
func collectMappingKeys(node *yaml.Node, set map[string]bool) {
	if node == nil {
		return
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		collectMappingKeys(node.Alias, set)
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		val := node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if key.Value == "<<" {
			switch val.Kind {
			case yaml.AliasNode, yaml.MappingNode:
				collectMappingKeys(val, set)
			case yaml.SequenceNode:
				for _, child := range val.Content {
					collectMappingKeys(child, set)
				}
			}
			continue
		}
		set[key.Value] = true
	}
}

// ValueSource points one kube config field at an annotation instead of
// a literal value (ADR-0009). Exactly one of Annotation (read from the
// Ingress) or NamespaceAnnotation (read from the Ingress's Namespace
// object) is set; Default supplies the value when the annotation is
// absent, empty, or whitespace-only.
//
// Annotation values are unreviewed runtime input, so they are validated
// at materialize time rather than here — a rejected value degrades to
// the cascade value and warns; it never costs availability monitoring.
// The Default, by contrast, lives in reviewed config and is validated
// at load.
type ValueSource struct {
	Annotation          string `yaml:"annotation,omitempty"`
	NamespaceAnnotation string `yaml:"namespaceAnnotation,omitempty"`

	// NamespaceLabel names the alert label carrying the namespace name,
	// for alertmanager.match sources (ADR-0013). Empty means the
	// alertmanager.DefaultNamespaceLabel. Rejected under kube.match,
	// where the namespace comes from the Ingress itself.
	NamespaceLabel string `yaml:"namespaceLabel,omitempty"`

	// Default carries the value as written — a string or a []string.
	// UnmarshalYAML parses it into the two typed views below; this
	// field exists so the reflection-driven unknown-key walker admits
	// `default:` and so `explain` can echo it back.
	Default any `yaml:"default,omitempty"`

	// DefaultScalar is the default as written, for the scalar fields
	// (pathFrom, slackFrom).
	DefaultScalar string `yaml:"-"`
	// DefaultList is the default for the list fields (notifyFrom,
	// tagsFrom and their Override twins): a YAML sequence entry-wise,
	// or a scalar split on commas.
	DefaultList []string `yaml:"-"`
	// HasDefault distinguishes "default: """ from an absent default.
	HasDefault bool `yaml:"-"`
}

// UnmarshalYAML decodes a *From block. `default:` accepts either a
// scalar or a sequence: a chart emitting `notify: {{ .Values.notify |
// join "," }}` and an operator writing `default: [alice, bob]` should
// both work, and the field's kind decides which form is read.
func (v *ValueSource) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a *From value source must be a mapping", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		switch key.Value {
		case "annotation":
			v.Annotation = val.Value
		case "namespaceAnnotation":
			v.NamespaceAnnotation = val.Value
		case "namespaceLabel":
			v.NamespaceLabel = val.Value
		case "default":
			v.HasDefault = true
			switch val.Kind {
			case yaml.ScalarNode:
				v.DefaultScalar = val.Value
				v.DefaultList = SplitAnnotationList(val.Value)
				v.Default = val.Value
			case yaml.SequenceNode:
				for j, child := range val.Content {
					if child.Kind != yaml.ScalarNode {
						return fmt.Errorf("line %d: default entry %d must be a scalar string", child.Line, j)
					}
					v.DefaultList = append(v.DefaultList, child.Value)
				}
				v.Default = append([]string(nil), v.DefaultList...)
			default:
				return fmt.Errorf("line %d: default must be a scalar or a sequence", val.Line)
			}
		default:
			return fmt.Errorf("line %d: unknown key %q in a *From value source (want annotation, namespaceAnnotation, namespaceLabel, default)", key.Line, key.Value)
		}
	}
	return nil
}

// KubeValueKind distinguishes how a *From block's resolved value joins
// the merge: as a scalar (the deepest layer that set the field wins) or
// as an entry list (union, or replace-the-baseline for the Override
// twins).
type KubeValueKind int

const (
	// KubeValueScalar merges like path / slack.
	KubeValueScalar KubeValueKind = iota
	// KubeValueList merges like notify / tags.
	KubeValueList
	// KubeValueStatusCodes merges like acceptedStatusCodes: an entry
	// list that replaces the previous layer's rather than unioning, and
	// whose entries are HTTP status codes rather than free strings.
	KubeValueStatusCodes
)

// KubeValueSource pairs one set *From block with the literal field it
// supplies and the merge semantics it inherits from that field.
type KubeValueSource struct {
	// Key is the YAML key, e.g. "notifyOverrideFrom".
	Key string
	// Field is the literal field it supplies, e.g. "notify".
	Field string
	Kind  KubeValueKind
	// Override marks the replace-the-baseline-at-this-position twins,
	// mirroring the !override YAML tag on the literal list.
	Override bool
	Source   *ValueSource
}

// ValueSources returns every *From block set on this config block,
// paired with the field it supplies. The order is fixed so error
// messages and provenance lines are reproducible.
//
// This table is the single place the six keys are enumerated: the
// validator and the merger both read it rather than re-deriving the
// mapping, so a seventh key cannot land in one and be forgotten in the
// other.
func (k *KubeConfig) ValueSources() []KubeValueSource {
	if k == nil {
		return nil
	}
	all := []KubeValueSource{
		{Key: "pathFrom", Field: "path", Kind: KubeValueScalar, Source: k.PathFrom},
		{Key: "slackFrom", Field: "slack", Kind: KubeValueScalar, Source: k.SlackFrom},
		{Key: "notifyFrom", Field: "notify", Kind: KubeValueList, Source: k.NotifyFrom},
		{Key: "notifyOverrideFrom", Field: "notify", Kind: KubeValueList, Override: true, Source: k.NotifyOverrideFrom},
		{Key: "tagsFrom", Field: "tags", Kind: KubeValueList, Source: k.TagsFrom},
		{Key: "tagsOverrideFrom", Field: "tags", Kind: KubeValueList, Override: true, Source: k.TagsOverrideFrom},
		{Key: "acceptedStatusCodesFrom", Field: "acceptedStatusCodes", Kind: KubeValueStatusCodes, Source: k.AcceptedStatusCodesFrom},
	}
	out := make([]KubeValueSource, 0, len(all))
	for _, vs := range all {
		if vs.Source != nil {
			out = append(out, vs)
		}
	}
	return out
}

// Clone returns a copy of k whose setFields map is independent of the
// original's. The merger lowers *From blocks into literal fields on a
// per-(Ingress, host) copy; without the deep copy those writes would
// leak across every host that walks the same rule.
func (k KubeConfig) Clone() KubeConfig {
	out := k
	out.setFields = make(map[string]bool, len(k.setFields))
	for key, v := range k.setFields {
		out.setFields[key] = v
	}
	return out
}

// MarkSet records field as present, as if the source YAML had carried
// that key. Used by the merger when a *From value source supplies the
// field at merge time.
func (k *KubeConfig) MarkSet(field string) {
	if k.setFields == nil {
		k.setFields = map[string]bool{}
	}
	k.setFields[field] = true
}

// SplitAnnotationList parses a comma-separated annotation value into
// entries, trimming surrounding whitespace and dropping empties.
// Shared by the config loader (scalar defaults) and the merger
// (annotation values) so both read a chart's `join ","` output the
// same way.
func SplitAnnotationList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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

// MarshalYAML emits the list as a plain YAML sequence. The Override
// flag is intentionally not round-tripped: by the time anything
// re-emits a NotifyList the merger has already consumed the override
// semantics, so the resolved Values are the only meaningful payload.
// Used by `toggle-monitor explain` to keep the resolved config block
// readable.
func (l NotifyList) MarshalYAML() (any, error) {
	return l.Values, nil
}

// UnmarshalYAML implements yaml.Unmarshaler. Sets Override=true when
// the YAML sequence carries the !override custom tag.
func (l *TagList) UnmarshalYAML(node *yaml.Node) error {
	return decodeOverridableStringList(node, &l.Values, &l.Override)
}

// MarshalYAML emits the list as a plain YAML sequence; see
// NotifyList.MarshalYAML for the rationale.
func (l TagList) MarshalYAML() (any, error) {
	return l.Values, nil
}

// UnmarshalYAML implements yaml.Unmarshaler. Sets Override=true when
// the YAML sequence carries the !override custom tag.
func (l *DependsOnList) UnmarshalYAML(node *yaml.Node) error {
	return decodeOverridableStringList(node, &l.Values, &l.Override)
}

// MarshalYAML emits the list as a plain YAML sequence; see
// NotifyList.MarshalYAML for the rationale.
func (l DependsOnList) MarshalYAML() (any, error) {
	return l.Values, nil
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
	Coalesce          Coalesce          `yaml:"coalesce,omitempty"`
}

// Coalesce tunes the per-channel burst dispatcher (ADR-0004). The
// dispatcher runs each channel through three modes: individual (post
// every failure immediately), pending (a wait window pools failures
// before deciding), and group (a living digest message).
//
// PendingWait is the wait window before the dispatcher decides whether
// a pool of failures becomes N individual messages (count <
// BurstThreshold) or one group digest (count ≥ BurstThreshold). At
// expiry, the dispatcher also fires an on-demand probe of any
// dependsOn parent referenced by ≥2 pool entries, with
// OnDemandProbeTimeout as its budget; if the parent probes down, real
// push-propagation drains the children from the pool, leaving the
// parent as the named root cause. GroupInterval is the digest heartbeat;
// RepeatInterval is the still-down reminder cadence in group-mode;
// GroupMention controls the broadcast mention on group open/reminder.
// Zero values fall back to the documented defaults.
//
// GroupWait is accepted as a deprecated alias for PendingWait for one
// release; setting both is a validation error.
type Coalesce struct {
	// PendingWait is the dispatcher's burst-collection window. The first
	// failure in a channel starts the timer; at expiry, the pool's size
	// vs BurstThreshold decides individual-vs-group dispatch. Default 30s.
	PendingWait Duration `yaml:"pendingWait,omitempty"`
	// GroupWait is the deprecated alias for PendingWait. It is consulted
	// only when PendingWait is unset; setting both is a validation error.
	GroupWait Duration `yaml:"groupWait,omitempty"`
	// GroupInterval is the digest heartbeat in group-mode: accrued
	// joins/recoveries/flaps flush as one edit + threaded reply per
	// interval. Also the resolve-debounce window (a recovery must hold
	// this long before it renders, dampening flap chatter). Default 5m.
	GroupInterval Duration `yaml:"groupInterval,omitempty"`
	// RepeatInterval is the still-down reminder cadence in group-mode.
	// Default 10m (groups exist only when a burst tripped — louder
	// cadence is desired). Individual-mode reminders use the per-monitor
	// reminderInterval instead; this knob does not affect them.
	RepeatInterval Duration `yaml:"repeatInterval,omitempty"`
	// BurstThreshold is the number of monitors a channel must have
	// concurrently down inside BurstWindow for the dispatcher to promote
	// to a group digest instead of paging each one individually. Must be
	// 0 (disables group-mode entirely) or ≥ 2 (1 is pathological — would
	// trip on any single failure). Pointer so "explicitly 0" (disable) is
	// distinguishable from "unset" (default 5).
	BurstThreshold *int `yaml:"burstThreshold,omitempty"`
	// BurstWindow is the rolling window the burst count is measured over.
	// It must be wide enough to span a whole outage's worth of probe
	// ticks: the scheduler jitters each monitor's first tick across its
	// full interval, so a cluster-wide outage reaches the dispatcher as a
	// trickle spread over roughly one probe interval, not as a burst
	// inside one PendingWait. Set it comfortably above the widest
	// monitors[].interval in play. Default 5m; must be ≥ PendingWait.
	BurstWindow Duration `yaml:"burstWindow,omitempty"`
	// GroupMention controls the broadcast mention on group open and on
	// each still-down reminder. One of "channel" (default), "here", or
	// "none". Edits (heartbeat deltas) never re-mention regardless.
	GroupMention string `yaml:"groupMention,omitempty"`
	// OnDemandProbeTimeout is the per-probe budget for the hot-parent
	// probe pass at PendingWait expiry. Default 5s. If a probe exceeds
	// this budget, treat as inconclusive and proceed with the unredacted
	// pool — the parent's regular tick will pick it up later.
	OnDemandProbeTimeout Duration `yaml:"onDemandProbeTimeout,omitempty"`
}

// Documented defaults for the burst dispatcher (ADR-0004). Mirrored
// from the comments on Coalesce so callers (lifecycle, manager) can
// derive effective values without re-parsing.
const (
	DefaultPendingWait          = 30 * time.Second
	DefaultGroupInterval        = 5 * time.Minute
	DefaultRepeatInterval       = 10 * time.Minute
	DefaultBurstThreshold       = 5
	DefaultBurstWindow          = 5 * time.Minute
	DefaultGroupMention         = "channel"
	DefaultOnDemandProbeTimeout = 5 * time.Second
)

// EffectivePendingWait returns the configured PendingWait, falling back
// to the deprecated GroupWait alias, then to DefaultPendingWait.
func (c Coalesce) EffectivePendingWait() time.Duration {
	if d := c.PendingWait.AsDuration(); d > 0 {
		return d
	}
	if d := c.GroupWait.AsDuration(); d > 0 {
		return d
	}
	return DefaultPendingWait
}

// EffectiveGroupInterval returns the configured GroupInterval, or the
// default if unset.
func (c Coalesce) EffectiveGroupInterval() time.Duration {
	if d := c.GroupInterval.AsDuration(); d > 0 {
		return d
	}
	return DefaultGroupInterval
}

// EffectiveRepeatInterval returns the configured RepeatInterval, or the
// default if unset.
func (c Coalesce) EffectiveRepeatInterval() time.Duration {
	if d := c.RepeatInterval.AsDuration(); d > 0 {
		return d
	}
	return DefaultRepeatInterval
}

// EffectiveBurstThreshold returns the configured BurstThreshold, or the
// default if unset. Returns 0 verbatim when explicitly disabled.
func (c Coalesce) EffectiveBurstThreshold() int {
	if c.BurstThreshold == nil {
		return DefaultBurstThreshold
	}
	return *c.BurstThreshold
}

// EffectiveBurstWindow returns the configured BurstWindow, or the
// default if unset. The floor at PendingWait keeps the window from
// being narrower than the pool it counts.
func (c Coalesce) EffectiveBurstWindow() time.Duration {
	w := c.BurstWindow.AsDuration()
	if w <= 0 {
		w = DefaultBurstWindow
	}
	if pw := c.EffectivePendingWait(); w < pw {
		return pw
	}
	return w
}

// EffectiveGroupMention returns the configured mention policy, or the
// default if unset.
func (c Coalesce) EffectiveGroupMention() string {
	if c.GroupMention == "" {
		return DefaultGroupMention
	}
	return c.GroupMention
}

// EffectiveOnDemandProbeTimeout returns the configured probe timeout,
// or the default if unset.
func (c Coalesce) EffectiveOnDemandProbeTimeout() time.Duration {
	if d := c.OnDemandProbeTimeout.AsDuration(); d > 0 {
		return d
	}
	return DefaultOnDemandProbeTimeout
}

// SelfHealth configures the self-health degraded-mode detector
// (ADR-0008). When the block is present, a burst of DNS-resolution
// failures across ≥ MinMonitors distinct monitors (with zero successes)
// within Window is treated as "the monitor went blind" and suppressed
// into a single self-health notice instead of N phantom service
// outages. Omitting the whole block disables the feature entirely:
// individual DNS failures commit immediately, as before.
type SelfHealth struct {
	// Window is W: the rolling detection/decision window. Default 90s.
	Window Duration `yaml:"window,omitempty"`
	// MinMonitors is N_min: the number of distinct DNS-failing monitors
	// required to trip degraded mode. Must be ≥ 2 (a 1–2 monitor
	// deployment inferring global blindness off a single flaky lookup is
	// pathological). Default 3.
	MinMonitors int `yaml:"minMonitors,omitempty"`
	// Channel is the Slack channel slug the self-health incident posts
	// to. Empty → metric + log only (no Slack). Must resolve to a
	// configured slack channel when set.
	Channel string `yaml:"channel,omitempty"`
	// Mention is optional raw on-call escalation markup added to the
	// degraded notice.
	Mention string `yaml:"mention,omitempty"`
}

// Documented defaults for self-health degraded mode (ADR-0008).
const (
	DefaultSelfHealthWindow      = 90 * time.Second
	DefaultSelfHealthMinMonitors = 3
)

// EffectiveWindow returns the configured Window or the default.
func (s SelfHealth) EffectiveWindow() time.Duration {
	if d := s.Window.AsDuration(); d > 0 {
		return d
	}
	return DefaultSelfHealthWindow
}

// EffectiveMinMonitors returns the configured MinMonitors or the default.
func (s SelfHealth) EffectiveMinMonitors() int {
	if s.MinMonitors > 0 {
		return s.MinMonitors
	}
	return DefaultSelfHealthMinMonitors
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

type HTTPClient struct {
	UserAgent string `yaml:"userAgent"`
}

// StatusPage is one collection-view entity. Each entry in statusPages
// gets its own /status/<slug> URL; /status itself lists every
// configured page in the order they appear in this slice (operators
// control the surface by ordering the YAML). Slug is required and
// must be unique across the list.
//
// Membership is tag-driven: each Section's Match predicate evaluates
// against a monitor's tag set and host, and a monitor can appear on
// many pages and many sections (N:M). See ADR-0003.
type StatusPage struct {
	Slug         string              `yaml:"slug"`
	FriendlyName string              `yaml:"friendlyName"`
	Description  string              `yaml:"description,omitempty"`
	LogoURL      string              `yaml:"logoUrl,omitempty"`
	Color        string              `yaml:"color,omitempty"` // ^#[0-9a-fA-F]{6}$
	Sections     []StatusPageSection `yaml:"sections"`
}

// StatusPageSection is one named block on the status page. Match is a
// single predicate node (leaf or branch) — see SectionMatch.
type StatusPageSection struct {
	Title string       `yaml:"title"`
	Match SectionMatch `yaml:"match"`
}

// SectionMatch is one node in the section predicate tree. A node is
// either a *leaf* (carries Tags and/or HostRegex) or a *branch*
// (carries exactly one of Any/All). Mixing leaf and branch fields,
// mixing Any with All, or leaving every field empty is a validation
// error.
//
// Leaf semantics:
//   - Tags: monitor's tag set must include every listed tag (AND).
//   - HostRegex: Go regexp, auto-anchored ^...$ at use time, matched
//     against the monitor's URL host.
//   - When both are present, both must match (AND).
//
// Branch semantics:
//   - Any: at least one child node must match (OR).
//   - All: every child node must match (AND).
//
// See ADR-0003.
type SectionMatch struct {
	Tags      []string       `yaml:"tags,omitempty"`
	HostRegex string         `yaml:"hostRegex,omitempty"`
	Any       []SectionMatch `yaml:"any,omitempty"`
	All       []SectionMatch `yaml:"all,omitempty"`
}

// IsBranch reports whether m is a branch node (Any or All set).
func (m SectionMatch) IsBranch() bool {
	return len(m.Any) > 0 || len(m.All) > 0
}

// IsLeaf reports whether m is a leaf node (Tags or HostRegex set).
func (m SectionMatch) IsLeaf() bool {
	return len(m.Tags) > 0 || m.HostRegex != ""
}

// IsEmpty reports whether m has no fields set — a validation error.
func (m SectionMatch) IsEmpty() bool {
	return !m.IsBranch() && !m.IsLeaf()
}

// Matches evaluates the predicate against a monitor's tag set and
// host. The caller is responsible for compiling tags into a set
// (TagSet) and passing the bare host (no scheme, no port, no path).
// HostRegex is compiled per call; v1 traffic does not justify caching.
func (m SectionMatch) Matches(tagSet map[string]bool, host string) bool {
	if len(m.Any) > 0 {
		for _, child := range m.Any {
			if child.Matches(tagSet, host) {
				return true
			}
		}
		return false
	}
	if len(m.All) > 0 {
		for _, child := range m.All {
			if !child.Matches(tagSet, host) {
				return false
			}
		}
		return true
	}
	// Leaf. Empty leaves are filtered out at validation time; if one
	// arrives here, treat as no-match to fail closed.
	if !m.IsLeaf() {
		return false
	}
	for _, required := range m.Tags {
		if !tagSet[required] {
			return false
		}
	}
	if m.HostRegex != "" {
		re, err := regexp.Compile("^(?:" + m.HostRegex + ")$")
		if err != nil {
			return false
		}
		if !re.MatchString(host) {
			return false
		}
	}
	return true
}

// TagSet builds a presence map from a slice of tags.
func TagSet(tags []string) map[string]bool {
	set := make(map[string]bool, len(tags))
	for _, t := range tags {
		set[t] = true
	}
	return set
}

type Monitor struct {
	Slug                string   `yaml:"slug"`
	FriendlyName        string   `yaml:"friendlyName"`
	URL                 string   `yaml:"url"`
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
	Tags                  []string `yaml:"tags,omitempty"`      // slug-segmented labels (a/b allowed); consumed by statusPages[].sections[].match tag leaves
	// Critical opts the monitor out of alert coalescing: its uptime
	// alerts post immediately as individually-paged per-monitor
	// messages instead of joining the per-channel digest. dependsOn
	// pause still wins. Default false → coalesced.
	Critical bool `yaml:"critical,omitempty"`

	// SSL thresholds — required when URL is HTTPS, allowed but
	// ignored for HTTP URLs (so anchored defaults can be shared).
	SSLAlertThreshold      Duration `yaml:"sslAlertThreshold,omitempty"`
	SSLEscalationThreshold Duration `yaml:"sslEscalationThreshold,omitempty"`
	SSLReminderInterval    Duration `yaml:"sslReminderInterval,omitempty"`
}

// SMTP TLS mode constants — the allowed values of smtpMonitors[].tls.
// Empty defaults to starttls.
const (
	SMTPTLSStartTLS = "starttls"
	SMTPTLSImplicit = "implicit"
	SMTPTLSNone     = "none"
)

// SMTPTLSModes is the canonical, declaration-ordered list of allowed
// smtpMonitors[].tls values (the validator's error message reads from
// it so the two can't drift).
var SMTPTLSModes = []string{SMTPTLSStartTLS, SMTPTLSImplicit, SMTPTLSNone}

// SMTPMonitor is one statically-declared SMTP probe. It reuses the HTTP
// monitor's scheduling / alerting / routing fields verbatim and adds
// SMTP-specifics (host/port/tls/ehloName). SMTP monitors are
// static-only — kube discovery never produces them. See the SMTP
// monitoring design.
type SMTPMonitor struct {
	Slug         string `yaml:"slug"`
	FriendlyName string `yaml:"friendlyName"`
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	// TLS is starttls (default) | implicit | none. starttls/implicit
	// capture the cert for the SSL state machine; none skips SSL.
	TLS      string `yaml:"tls,omitempty"`
	EHLOName string `yaml:"ehloName,omitempty"` // default "toggle-monitor"

	Interval     Duration `yaml:"interval"`
	Timeout      Duration `yaml:"timeout"`
	Retries      int      `yaml:"retries"`
	RetryBackoff Duration `yaml:"retryBackoff"`
	// TLSInsecureSkipVerify skips cert chain verification and forces SSL
	// state to ssl-skipped (private-CA / self-signed relays).
	TLSInsecureSkipVerify bool     `yaml:"tlsInsecureSkipVerify,omitempty"`
	Proxy                 string   `yaml:"proxy,omitempty"`
	ReminderInterval      Duration `yaml:"reminderInterval"`
	Slack                 string   `yaml:"slack"`
	Notify                []string `yaml:"notify,omitempty"`
	DependsOn             []string `yaml:"dependsOn,omitempty"`
	Tags                  []string `yaml:"tags,omitempty"`
	Critical              bool     `yaml:"critical,omitempty"` // opt out of coalescing; see Monitor.Critical

	// SSL thresholds — required when tls != none && !tlsInsecureSkipVerify,
	// forbidden/ignored otherwise (parallels the HTTPS rule).
	SSLAlertThreshold      Duration `yaml:"sslAlertThreshold,omitempty"`
	SSLEscalationThreshold Duration `yaml:"sslEscalationThreshold,omitempty"`
	SSLReminderInterval    Duration `yaml:"sslReminderInterval,omitempty"`
}

// TLSMode returns the effective TLS mode, defaulting empty to starttls.
func (m SMTPMonitor) TLSMode() string {
	if m.TLS == "" {
		return SMTPTLSStartTLS
	}
	return m.TLS
}

// URL returns the synthesized smtp://host:port identity persisted in
// the monitors.url column so URL-keyed features keep working.
func (m SMTPMonitor) URL() string {
	return fmt.Sprintf("smtp://%s:%d", m.Host, m.Port)
}

// TracksSSL reports whether this monitor's cert expiry feeds the SSL
// state machine (TLS negotiated and not skip-verified).
func (m SMTPMonitor) TracksSSL() bool {
	return m.TLSMode() != SMTPTLSNone && !m.TLSInsecureSkipVerify
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

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}
	applyAlertmanagerDefaults(&cfg)
	c := &checker{root: &root}
	c.validate(&cfg)
	if err := c.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checker accumulates validation errors with line numbers resolved
// against the original YAML node tree.
type checker struct {
	root *yaml.Node
	errs []error
}

func (c *checker) errf(path []any, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line, col := 0, 0
	if n := nodeAt(c.root, path...); n != nil {
		line, col = n.Line, n.Column
	}
	p := pathStr(path)
	switch {
	case line > 0 && col > 0:
		c.errs = append(c.errs, fmt.Errorf("line %d, col %d: %s: %s", line, col, p, msg))
	case line > 0:
		c.errs = append(c.errs, fmt.Errorf("line %d: %s: %s", line, p, msg))
	default:
		c.errs = append(c.errs, fmt.Errorf("%s: %s", p, msg))
	}
}

// errfNode is the variant used when the caller already holds the
// offending yaml.Node (typically the key node, so the column points at
// the unknown key itself, not at the value).
func (c *checker) errfNode(node *yaml.Node, path []any, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p := pathStr(path)
	if node != nil && node.Line > 0 {
		c.errs = append(c.errs, fmt.Errorf("line %d, col %d: %s: %s", node.Line, node.Column, p, msg))
		return
	}
	c.errs = append(c.errs, fmt.Errorf("%s: %s", p, msg))
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
	// Unknown-key check first — typos cascade into otherwise-confusing
	// downstream errors (missing required-at-root fields, unknown
	// references, etc.). Reporting the typo before those keeps the
	// error feed actionable.
	c.validateUnknownKeys()

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

	if cfg.Sentry != nil {
		if cfg.Sentry.Enabled {
			if !envVarNamePattern.MatchString(cfg.Sentry.DSNEnv) {
				c.errf([]any{"sentry", "dsnEnv"},
					"%q must match ^[A-Z][A-Z0-9_]*$ (do not interpolate ${...} into this field)", cfg.Sentry.DSNEnv)
			}
		}
		if cfg.Sentry.SampleRate < 0 || cfg.Sentry.SampleRate > 1 {
			c.errf([]any{"sentry", "sampleRate"},
				"must be in [0.0, 1.0], got %v", cfg.Sentry.SampleRate)
		}
		if cfg.Sentry.TracesSampleRate < 0 || cfg.Sentry.TracesSampleRate > 1 {
			c.errf([]any{"sentry", "tracesSampleRate"},
				"must be in [0.0, 1.0], got %v", cfg.Sentry.TracesSampleRate)
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

	c.validateCoalesce(cfg.Slack.Coalesce)

	// selfHealth.* — see ADR-0008. minMonitors < 2 is pathological; the
	// channel slug (if set) must resolve to a configured slack channel.
	// Runs after the channel set is populated above.
	if cfg.SelfHealth != nil {
		c.validateSelfHealth(*cfg.SelfHealth, seenSlackChannels)
	}

	// kube.match validation: see ADR-0002 §Validation. Structural
	// errors only — resolved-value errors (interval/timeout, SSL
	// thresholds, root-required-field-overridden-empty) live in the
	// merger and surface at materialization time as kube-invalid
	// discovery rows. Slug references inside config blocks need the
	// slack channel / proxy / userMapping sets, so this runs after
	// those have been populated above.
	c.validateKube(cfg, seenSlackChannels, seenProxies)

	// alertmanager.* — see ADR-0005. Slug references inside config
	// blocks need the slack channel set; userMapping is consumed from
	// cfg.Slack.UserMapping directly inside the validator.
	c.validateAlertmanager(cfg, seenSlackChannels)

	// Monitor validation.
	seenMonitors := map[string]struct{}{}
	for i, m := range cfg.Monitors {
		base := []any{"monitors", i}
		if err := slug.Validate(m.Slug); err != nil {
			c.errf(append(base, "slug"), "%v", err)
		}
		if strings.HasPrefix(m.Slug, slug.KubeSlugPrefix) {
			c.errf(append(base, "slug"),
				"monitor slug %q must not start with %q — that prefix is reserved for kube-discovered monitors",
				m.Slug, slug.KubeSlugPrefix)
		}
		if _, dup := seenMonitors[m.Slug]; dup {
			c.errf(append(base, "slug"), "duplicate slug %q", m.Slug)
		}
		seenMonitors[m.Slug] = struct{}{}
		for j, tag := range m.Tags {
			if err := slug.ValidateTag(tag); err != nil {
				c.errf(append(base, "tags", j), "%v", err)
			}
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

	// SMTP monitors share the slug namespace, the dependsOn graph, and
	// the scheduling/routing validators with HTTP monitors. Validated
	// after the HTTP loop so seenMonitors carries the full HTTP slug set
	// for cross-kind duplicate detection.
	c.validateSMTPMonitors(cfg, seenMonitors, seenProxies, seenSlackChannels)

	// Global dependsOn pass: every reference resolves to a known
	// static monitor (HTTP or SMTP), and the graph has no cycles. Done
	// after the per-monitor passes so we have the full slug set.
	depNodes := collectDepNodes(cfg)
	knownSlugs := map[string]struct{}{}
	for _, n := range depNodes {
		knownSlugs[n.slug] = struct{}{}
	}
	for i, m := range cfg.Monitors {
		base := []any{"monitors", i}
		for j, dep := range m.DependsOn {
			if dep == m.Slug {
				continue // already reported above
			}
			if _, ok := knownSlugs[dep]; !ok {
				c.errf(append(base, "dependsOn", j), "unknown monitor slug %q (parents must be declared static monitors)", dep)
			}
		}
	}
	for i, m := range cfg.SMTPMonitors {
		base := []any{"smtpMonitors", i}
		for j, dep := range m.DependsOn {
			if dep == m.Slug {
				continue // already reported above
			}
			if _, ok := knownSlugs[dep]; !ok {
				c.errf(append(base, "dependsOn", j), "unknown monitor slug %q (parents must be declared static monitors)", dep)
			}
		}
	}
	if cycle := detectDependsOnCycle(depNodes); cycle != "" {
		c.errf([]any{"monitors"}, "dependsOn graph contains a cycle: %s", cycle)
	}
	// monitorByIdx feeds the kube dependsOn resolver below: kube parents
	// may reference any declared static monitor, HTTP or SMTP.
	monitorByIdx := map[string]int{}
	for i, n := range depNodes {
		monitorByIdx[n.slug] = i
	}

	// Kube dependsOn resolution: every entry in a config.dependsOn
	// list (anywhere in the tree) must resolve to a declared static
	// monitor — the schema explicitly disallows kube-discovered
	// parents because their slugs aren't known until reconcile time.
	if cfg.Kube != nil {
		c.validateKubeDependsOnRefs(cfg.Kube.Match, monitorByIdx, []any{"kube", "match"})
	}

	// statusPages validation. Per page: slug required + unique across
	// the list, friendlyName required, color/logoUrl syntax if set, at
	// least one section, each section has a title and a non-empty
	// predicate tree (any/all branches or tags/hostRegex leaves). See
	// ADR-0003.
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
		if strings.TrimSpace(page.FriendlyName) == "" {
			c.errf(append(pbase, "friendlyName"), "required")
		}
		if page.Color != "" && !colorHexPattern.MatchString(page.Color) {
			c.errf(append(pbase, "color"), "%q must match %s (6-digit hex, e.g. #f5333f)", page.Color, colorHexPattern.String())
		}
		if page.LogoURL != "" {
			if u, err := url.Parse(page.LogoURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				c.errf(append(pbase, "logoUrl"), "must be an http or https URL, got %q", page.LogoURL)
			}
		}
		if len(page.Sections) == 0 {
			c.errf(append(pbase, "sections"), "at least one section is required")
		}
		for i, sec := range page.Sections {
			sbase := append(append([]any{}, pbase...), "sections", i)
			if strings.TrimSpace(sec.Title) == "" {
				c.errf(append(sbase, "title"), "required")
			}
			c.validateSectionMatch(&sec.Match, append(sbase, "match"))
		}
	}
}

// validSMTPTLSModes is the allowed tls enum for smtpMonitors (empty
// defaults to starttls at use time).
var validSMTPTLSModes = map[string]struct{}{
	SMTPTLSStartTLS: {}, SMTPTLSImplicit: {}, SMTPTLSNone: {},
}

// validateSMTPMonitors validates the smtpMonitors block. SMTP monitors
// share the slug namespace (seenMonitors carries the HTTP slugs already)
// and reuse the HTTP scheduling/routing validators; SMTP-specifics
// (host/port/tls) and the TLS-conditional SSL-threshold rule are
// enforced here. dependsOn cross-references + cycles are resolved in the
// shared global pass in validate().
func (c *checker) validateSMTPMonitors(cfg *Config, seenMonitors, seenProxies, seenSlackChannels map[string]struct{}) {
	for i, m := range cfg.SMTPMonitors {
		base := []any{"smtpMonitors", i}
		if err := slug.Validate(m.Slug); err != nil {
			c.errf(append(base, "slug"), "%v", err)
		}
		if strings.HasPrefix(m.Slug, slug.KubeSlugPrefix) {
			c.errf(append(base, "slug"),
				"monitor slug %q must not start with %q — that prefix is reserved for kube-discovered monitors",
				m.Slug, slug.KubeSlugPrefix)
		}
		if _, dup := seenMonitors[m.Slug]; dup {
			c.errf(append(base, "slug"), "duplicate slug %q", m.Slug)
		}
		seenMonitors[m.Slug] = struct{}{}

		if strings.TrimSpace(m.Host) == "" {
			c.errf(append(base, "host"), "required")
		}
		if m.Port < 1 || m.Port > 65535 {
			c.errf(append(base, "port"), "must be in 1..65535, got %d", m.Port)
		}
		if m.TLS != "" {
			if _, ok := validSMTPTLSModes[m.TLS]; !ok {
				c.errf(append(base, "tls"), "%q is not one of %v", m.TLS, SMTPTLSModes)
			}
		}
		for j, tag := range m.Tags {
			if err := slug.ValidateTag(tag); err != nil {
				c.errf(append(base, "tags", j), "%v", err)
			}
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
		retryWindow := time.Duration(m.Retries) * (timeout + backoff)
		if retryWindow >= interval {
			c.errf(base, "retries × (timeout + retryBackoff) = %s must be less than interval (%s)", retryWindow, interval)
		}
		for j, dep := range m.DependsOn {
			if dep == m.Slug {
				c.errf(append(base, "dependsOn", j), "monitor cannot depend on itself")
			}
		}

		// SSL thresholds: required when TLS is negotiated and the cert is
		// actually verified; forbidden/ignored otherwise (parallels the
		// HTTPS rule). When required and present, alert > escalation > 0.
		if m.TracksSSL() {
			if m.SSLAlertThreshold.AsDuration() <= 0 {
				c.errf(append(base, "sslAlertThreshold"), "required when tls is %q or %q", SMTPTLSStartTLS, SMTPTLSImplicit)
			}
			if m.SSLEscalationThreshold.AsDuration() <= 0 {
				c.errf(append(base, "sslEscalationThreshold"), "required when tls is %q or %q", SMTPTLSStartTLS, SMTPTLSImplicit)
			}
			if m.SSLReminderInterval.AsDuration() <= 0 {
				c.errf(append(base, "sslReminderInterval"), "required when tls is %q or %q", SMTPTLSStartTLS, SMTPTLSImplicit)
			}
			if m.SSLAlertThreshold.AsDuration() > 0 && m.SSLEscalationThreshold.AsDuration() > 0 &&
				m.SSLAlertThreshold.AsDuration() <= m.SSLEscalationThreshold.AsDuration() {
				c.errf(append(base, "sslAlertThreshold"),
					"must be strictly greater than sslEscalationThreshold (%s)",
					m.SSLEscalationThreshold.AsDuration())
			}
		}
	}
}

// validateSectionMatch recurses through the predicate tree rooted at
// m, enforcing the rules in ADR-0003 §Validation: exactly one of
// any/all on a branch (with non-empty children), at least one of
// tags/hostRegex on a leaf, no mixing of branch and leaf fields, no
// empty nodes, and hostRegex must compile + tags must be non-empty
// kebab-case strings.
func (c *checker) validateSectionMatch(m *SectionMatch, base []any) {
	hasBranch := m.IsBranch()
	hasLeaf := m.IsLeaf()
	if m.IsEmpty() {
		c.errf(base, "predicate node is empty; use any:/all: for a branch or tags:/hostRegex: for a leaf")
		return
	}
	if hasBranch && hasLeaf {
		c.errf(base, "predicate node cannot mix branch (any/all) and leaf (tags/hostRegex) fields")
		return
	}
	if len(m.Any) > 0 && len(m.All) > 0 {
		c.errf(base, "any: and all: are mutually exclusive on the same predicate node")
		return
	}
	if hasBranch {
		var children []SectionMatch
		var key string
		if len(m.Any) > 0 {
			children = m.Any
			key = "any"
		} else {
			children = m.All
			key = "all"
		}
		if len(children) == 0 {
			c.errf(append(append([]any{}, base...), key), "must have at least one child predicate")
			return
		}
		for i := range children {
			c.validateSectionMatch(&children[i], append(append([]any{}, base...), key, i))
		}
		return
	}
	// Leaf.
	if len(m.Tags) > 0 {
		for i, tag := range m.Tags {
			if err := slug.ValidateTag(tag); err != nil {
				c.errf(append(append([]any{}, base...), "tags", i), "%v", err)
			}
		}
	}
	if m.HostRegex != "" {
		if _, err := regexp.Compile(m.HostRegex); err != nil {
			c.errf(append(append([]any{}, base...), "hostRegex"), "invalid regex: %v", err)
		}
	}
}

// depNode is one vertex in the combined (HTTP + SMTP) dependsOn graph.
type depNode struct {
	slug string
	deps []string
}

// collectDepNodes gathers every static monitor (HTTP then SMTP) as a
// dependsOn graph vertex, in declaration order.
func collectDepNodes(cfg *Config) []depNode {
	out := make([]depNode, 0, len(cfg.Monitors)+len(cfg.SMTPMonitors))
	for _, m := range cfg.Monitors {
		out = append(out, depNode{slug: m.Slug, deps: m.DependsOn})
	}
	for _, m := range cfg.SMTPMonitors {
		out = append(out, depNode{slug: m.Slug, deps: m.DependsOn})
	}
	return out
}

// detectDependsOnCycle runs a DFS over the monitor dependency graph
// and returns a human-readable description of the first cycle found,
// or "" if the graph is acyclic.
func detectDependsOnCycle(nodes []depNode) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	parents := map[string][]string{}
	for _, n := range nodes {
		color[n.slug] = white
		parents[n.slug] = n.deps
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
	for _, n := range nodes {
		if color[n.slug] == white {
			if c := dfs(n.slug); c != "" {
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
func (c *checker) validateKube(cfg *Config, slackChannels, proxies map[string]struct{}) {
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

	// watchDebounce bounds. Zero is the documented "disabled" value, so
	// only non-zero values are range-checked.
	if k.WatchDebounce != nil {
		if d := k.WatchDebounce.AsDuration(); d != 0 && (d < MinKubeWatchDebounce || d > MaxKubeWatchDebounce) {
			c.errf([]any{"kube", "watchDebounce"},
				"must be 0s (disabled) or between %s and %s, got %s",
				MinKubeWatchDebounce, MaxKubeWatchDebounce, *k.WatchDebounce)
		}
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
		c.checkKubeRequiredAtRoot(&root.Config, []any{"kube", "match", 0, "config"})
	}

	// Walk every rule (including the root) for selector / config
	// validity. The root already had its required-field check above;
	// validateKubeRule does the remaining per-rule checks (selectors,
	// label keys, slug references inside config, final/ignore
	// invariants, HTTPMethod / scheme enums when set).
	for i := range k.Match {
		c.validateKubeRule(&k.Match[i], []any{"kube", "match", i}, slackChannels, proxies, cfg.Slack.UserMapping)
	}
}

// kubeWhenIsEmpty reports whether every selector field on w is unset
// (zero / nil). Used to detect the mandatory root baseline.
func kubeWhenIsEmpty(w KubeMatchWhen) bool {
	return w.Namespace == "" && w.NamespaceRegex == "" &&
		w.Host == "" && w.HostRegex == "" && len(w.Labels) == 0 &&
		len(w.Annotations) == 0 && len(w.NamespaceAnnotations) == 0
}

// checkKubeRequiredAtRoot verifies the root rule's Config sets every
// field marked "Req at root" in docs/config-schema.md. IsSet
// distinguishes "explicitly set" from "unset / zero-value" — needed
// for fields like followRedirects where the zero value (false) is a
// legitimate explicit choice.
func (c *checker) checkKubeRequiredAtRoot(k *KubeConfig, base []any) {
	// A *From block carrying a default satisfies the requirement: every
	// host whose annotation is absent still inherits a usable value
	// (ADR-0009). A defaultless one does not — it would leave hosts
	// without the annotation with nothing at all.
	defaulted := map[string]bool{}
	for _, vs := range k.ValueSources() {
		if vs.Source.HasDefault {
			defaulted[vs.Field] = true
		}
	}

	for _, key := range KubeRequiredAtRoot {
		if !k.IsSet(key) {
			if defaulted[key] {
				continue
			}
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
	slackChannels, proxies map[string]struct{},
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

	c.validateKubeConfig(&r.Config, append(append([]any{}, base...), "config"), slackChannels, proxies, userMapping)

	for i := range r.Nested {
		c.validateKubeRule(&r.Nested[i],
			append(append([]any{}, base...), "nested", i),
			slackChannels, proxies, userMapping)
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
	// Annotation values are unconstrained by k8s, so only the keys are
	// checked — an annotation may legally hold any string.
	for field, m := range map[string]map[string]string{
		"annotations":          w.Annotations,
		"namespaceAnnotations": w.NamespaceAnnotations,
	} {
		for key := range m {
			if errs := validation.IsQualifiedName(key); len(errs) > 0 {
				c.errf(append(append([]any{}, base...), field, key),
					"invalid k8s annotation key %q: %s", key, strings.Join(errs, "; "))
			}
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
	slackChannels, proxies map[string]struct{},
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
			if err := slug.ValidateTag(tag); err != nil {
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
	c.validateKubeValueSources(k, base, slackChannels, userMapping)

	// dependsOn cross-references with static monitors are resolved in
	// a second pass once the monitor slug set is fully built — see
	// validateKubeDependsOnRefs called from validate().
}

// validateKubeValueSources enforces the structural rules for ADR-0009
// `*From` blocks on one config block: a *From and the literal it
// supplies are mutually exclusive, so are a list's From/OverrideFrom
// pair, exactly one annotation scope is required, and the `default:`
// (reviewed config, unlike the annotation value) must satisfy the same
// constraints as the literal field.
func (c *checker) validateKubeValueSources(
	k *KubeConfig,
	base []any,
	slackChannels map[string]struct{},
	userMapping map[string]string,
) {
	for _, vs := range k.ValueSources() {
		vbase := append(append([]any{}, base...), vs.Key)

		if k.IsSet(vs.Field) {
			c.errf(vbase, "cannot be combined with %q in the same config block — set the field either literally or from an annotation", vs.Field)
		}

		switch {
		case vs.Source.Annotation == "" && vs.Source.NamespaceAnnotation == "":
			c.errf(vbase, "requires exactly one of annotation: or namespaceAnnotation:")
		case vs.Source.Annotation != "" && vs.Source.NamespaceAnnotation != "":
			c.errf(vbase, "annotation: and namespaceAnnotation: are mutually exclusive — one *From block reads one scope")
		}

		for _, key := range []string{vs.Source.Annotation, vs.Source.NamespaceAnnotation} {
			if key == "" {
				continue
			}
			if errs := validation.IsQualifiedName(key); len(errs) > 0 {
				c.errf(vbase, "invalid k8s annotation key %q: %s", key, strings.Join(errs, "; "))
			}
		}

		// namespaceLabel: names the alert label an alertmanager.match
		// source reads the namespace from (ADR-0013). Under kube.match the
		// namespace is the Ingress's own, so the key has no meaning here.
		if vs.Source.NamespaceLabel != "" {
			c.errf(append(append([]any{}, vbase...), "namespaceLabel"),
				"is only valid under alertmanager.match; a kube.match source reads the Ingress's own namespace")
		}

		if vs.Source.HasDefault {
			c.validateValueSourceDefault(vs, vbase, slackChannels, userMapping)
		}
	}

	// notifyFrom + notifyOverrideFrom in one block would have the same
	// layer both union and replace the baseline; the merge order
	// between them is undefined by design rather than merely unstated.
	if k.NotifyFrom != nil && k.NotifyOverrideFrom != nil {
		c.errf(append(append([]any{}, base...), "notifyOverrideFrom"),
			"cannot be combined with notifyFrom in the same config block")
	}
	if k.TagsFrom != nil && k.TagsOverrideFrom != nil {
		c.errf(append(append([]any{}, base...), "tagsOverrideFrom"),
			"cannot be combined with tagsFrom in the same config block")
	}
}

// validateValueSourceDefault applies the literal field's own load-time
// constraints to a *From block's `default:`. Defaults are reviewed
// config, so they are hard errors here — unlike annotation values,
// which degrade to a warning at materialize time.
func (c *checker) validateValueSourceDefault(
	vs KubeValueSource,
	vbase []any,
	slackChannels map[string]struct{},
	userMapping map[string]string,
) {
	dbase := append(append([]any{}, vbase...), "default")
	switch vs.Field {
	case "path":
		if !strings.HasPrefix(vs.Source.DefaultScalar, "/") {
			c.errf(dbase, "must start with %q, got %q", "/", vs.Source.DefaultScalar)
		}
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
	case "tags":
		for i, tag := range vs.Source.DefaultList {
			if err := slug.ValidateTag(tag); err != nil {
				c.errf(append(append([]any{}, dbase...), i), "%v", err)
			}
		}
	case "acceptedStatusCodes":
		if len(vs.Source.DefaultList) == 0 {
			c.errf(dbase, "must be a non-empty list of HTTP status codes")
		}
		for i, raw := range vs.Source.DefaultList {
			code, err := strconv.Atoi(raw)
			if err != nil {
				c.errf(append(append([]any{}, dbase...), i),
					"%q is not a number; acceptedStatusCodes entries are HTTP status codes", raw)
				continue
			}
			if code < 100 || code > 599 {
				c.errf(append(append([]any{}, dbase...), i),
					"%d is not a valid HTTP status code (100..599)", code)
			}
		}
	}
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

// validateCoalesce enforces the burst-dispatcher rules from ADR-0004:
// pendingWait and the deprecated groupWait are mutually exclusive;
// burstThreshold is either 0 (disable group-mode) or ≥ 2 (1 would trip
// on any single failure); groupMention is one of channel/here/none.
// Duration knobs may be zero (defaulted) but cannot be negative — the
// Duration unmarshaler already rejects negative values, so this only
// guards positive-when-set semantics where the design needs them.
func (c *checker) validateCoalesce(co Coalesce) {
	base := []any{"slack", "coalesce"}
	if co.PendingWait.AsDuration() > 0 && co.GroupWait.AsDuration() > 0 {
		c.errf(append(base, "pendingWait"),
			"cannot set both pendingWait and the deprecated groupWait alias — use pendingWait")
	}
	if co.BurstThreshold != nil {
		switch n := *co.BurstThreshold; {
		case n < 0:
			c.errf(append(base, "burstThreshold"),
				"%d must be >= 0 (0 disables group-mode)", n)
		case n == 1:
			c.errf(append(base, "burstThreshold"),
				"1 is pathological — trips on any single failure. Use 0 to disable group-mode or >= 2")
		}
	}
	if w := co.BurstWindow.AsDuration(); w > 0 && w < co.EffectivePendingWait() {
		c.errf(append(base, "burstWindow"),
			"%s must be >= pendingWait (%s) — a window narrower than the pending pool cannot count it",
			w, co.EffectivePendingWait())
	}
	if co.GroupMention != "" {
		switch co.GroupMention {
		case "channel", "here", "none":
		default:
			c.errf(append(base, "groupMention"),
				"%q must be one of channel | here | none", co.GroupMention)
		}
	}
}

// validateSelfHealth enforces the ADR-0008 rules: minMonitors, when
// set, must be >= 2 (1 trips on any single flaky lookup; a 1–2 monitor
// deployment cannot meaningfully infer global blindness). The channel
// slug, when set, must resolve to a configured slack channel; empty
// channel is valid (metric + log only). Window may be zero (defaulted);
// the Duration unmarshaler already rejects negatives.
func (c *checker) validateSelfHealth(sh SelfHealth, channels map[string]struct{}) {
	base := []any{"selfHealth"}
	if sh.MinMonitors != 0 && sh.MinMonitors < 2 {
		c.errf(append(base, "minMonitors"),
			"%d is pathological — a 1–2 monitor deployment cannot infer global blindness. Use >= 2",
			sh.MinMonitors)
	}
	if sh.Channel != "" {
		if _, ok := channels[sh.Channel]; !ok {
			c.errf(append(base, "channel"),
				"unknown channel slug %q", sh.Channel)
		}
	}
}
