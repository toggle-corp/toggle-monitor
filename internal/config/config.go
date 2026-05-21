// Package config loads, validates, and merges the toggle-monitor YAML
// configuration. See docs/config-schema.md.
//
// Issue 2 scope: parse and validate the minimum field set needed to
// run a static-monitor-only worker. Anchors, env interpolation,
// multi-error reporting, and the slack section land in later issues.
package config

import (
	"bytes"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/toggle-corp/toggle-monitor/internal/slug"
)

// Config is the typed, validated representation of the toggle-monitor
// YAML config.
type Config struct {
	DisplayTimezone string         `yaml:"displayTimezone"`
	PublicBaseURL   string         `yaml:"publicBaseURL,omitempty"`
	DBBodyMaxChars  int            `yaml:"dbBodyMaxChars"`
	Database        Database       `yaml:"database"`
	UI              UI             `yaml:"ui"`
	Theme           Theme          `yaml:"theme"`
	HTTPClient      HTTPClient     `yaml:"httpClient"`
	Groups          []Group        `yaml:"groups"`
	Monitors        []Monitor      `yaml:"monitors"`
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
	Slug         string `yaml:"slug"`
	FriendlyName string `yaml:"friendlyName"`
	Description  string `yaml:"description,omitempty"`
	LogoURL      string `yaml:"logoUrl,omitempty"`
	Color        string `yaml:"color,omitempty"`
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
}

// Load parses and validates the YAML config. Returns a populated
// Config on success, or a descriptive error on the first violation.
// (Multi-error reporting lands in Issue 5.)
func Load(data []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown top-level keys (Issue 5 will add x-* allow)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg *Config) error {
	// Group validation: kube-discovered required; slugs unique and valid.
	seenGroups := map[string]struct{}{}
	hasKubeDiscovered := false
	for i, g := range cfg.Groups {
		if err := slug.Validate(g.Slug); err != nil {
			return fmt.Errorf("groups[%d].slug: %w", i, err)
		}
		if _, dup := seenGroups[g.Slug]; dup {
			return fmt.Errorf("groups[%d].slug: duplicate slug %q", i, g.Slug)
		}
		seenGroups[g.Slug] = struct{}{}
		if g.Slug == "kube-discovered" {
			hasKubeDiscovered = true
		}
	}
	if !hasKubeDiscovered {
		return fmt.Errorf("groups: a group with slug %q is required", "kube-discovered")
	}

	// Monitor validation: slug valid + unique, group resolves,
	// cross-field timing rule.
	seenMonitors := map[string]struct{}{}
	for i, m := range cfg.Monitors {
		if err := slug.Validate(m.Slug); err != nil {
			return fmt.Errorf("monitors[%d].slug: %w", i, err)
		}
		if _, dup := seenMonitors[m.Slug]; dup {
			return fmt.Errorf("monitors[%d].slug: duplicate slug %q", i, m.Slug)
		}
		seenMonitors[m.Slug] = struct{}{}
		if _, ok := seenGroups[m.Group]; !ok {
			return fmt.Errorf("monitors[%d].group: unknown group %q", i, m.Group)
		}
		interval := m.Interval.AsDuration()
		timeout := m.Timeout.AsDuration()
		backoff := m.RetryBackoff.AsDuration()
		if timeout >= interval {
			return fmt.Errorf("monitors[%d]: timeout (%s) must be less than interval (%s)", i, timeout, interval)
		}
		// retries × (timeout + retryBackoff) < interval
		retryWindow := time.Duration(m.Retries) * (timeout + backoff)
		if retryWindow >= interval {
			return fmt.Errorf("monitors[%d]: retries × (timeout + retryBackoff) = %s must be less than interval (%s)", i, retryWindow, interval)
		}
	}
	return nil
}
