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
	Slack           Slack      `yaml:"slack"`
	Groups          []Group    `yaml:"groups"`
	Monitors        []Monitor  `yaml:"monitors"`
}

// Heartbeat is the outbound deadman heartbeat block. When nil, the
// background loop is not started.
type Heartbeat struct {
	URL                 string   `yaml:"url"`
	Interval            Duration `yaml:"interval"`
	FailOnStalledWorker bool     `yaml:"failOnStalledWorker"`
}

// Slack is the consolidated Slack-related config block. v1 supports a
// single workspace; multiple channels can be declared and referenced
// by slug from monitors.
type Slack struct {
	BodyMaxChars int               `yaml:"bodyMaxChars"`
	Channels     []SlackChannel    `yaml:"channels"`
	UserMapping  map[string]string `yaml:"userMapping,omitempty"` // slug → U... | S...
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
	ReminderInterval    Duration `yaml:"reminderInterval"`
	Slack               string   `yaml:"slack"`               // channel slug
	Notify              []string `yaml:"notify,omitempty"`    // raw <...> Slack markup; userMapping lands in Issue 13
	DependsOn           []string `yaml:"dependsOn,omitempty"` // upstream static-monitor slugs that gate this one
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
	"slack":  {},
	"groups": {}, "monitors": {},
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
		for j, dep := range m.DependsOn {
			if dep == m.Slug {
				c.errf(append(base, "dependsOn", j), "monitor cannot depend on itself")
				continue
			}
			if _, ok := seenMonitors[dep]; !ok {
				// Forward references are valid (YAML order is independent of dep order)
				// — defer that check to the global pass below.
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
