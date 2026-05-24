package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration extends time.Duration with support for the `d` (days)
// suffix per docs/config-schema.md. Acceptable forms include `30s`,
// `5m`, `1h`, `3d`, `30d`, `1h30m`, `2d6h`, etc.
type Duration time.Duration

// AsDuration returns the underlying time.Duration.
func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalYAML implements yaml.Marshaler so the Duration round-trips as
// a human-readable scalar (e.g., "5m0s") instead of a raw nanosecond
// integer. Used by `toggle-monitor config show` and `toggle-monitor
// explain` to keep the printed config legible.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler so YAML scalar strings get
// converted into Duration. Numbers are rejected (operator must use a
// suffixed string for clarity).
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar, got %v at line %d", node.Tag, node.Line)
	}
	parsed, err := parseExtendedDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q at line %d: %w", node.Value, node.Line, err)
	}
	*d = Duration(parsed)
	return nil
}

// parseExtendedDuration accepts time.ParseDuration syntax plus a `d`
// (days) suffix that may appear at the start of the string, possibly
// followed by a normal Go-style duration tail (e.g., "2d6h", "1d", "3d30m").
func parseExtendedDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	rest := s
	var days time.Duration
	if idx := strings.Index(s, "d"); idx >= 0 {
		// Ensure everything before `d` is a positive integer literal —
		// if it contains another unit suffix we treat the whole string
		// as a normal Go duration (no leading `d`).
		head := s[:idx]
		if n, err := strconv.Atoi(head); err == nil && n >= 0 {
			days = time.Duration(n) * 24 * time.Hour
			rest = s[idx+1:]
		}
	}
	if rest == "" {
		return days, nil
	}
	tail, err := time.ParseDuration(rest)
	if err != nil {
		return 0, err
	}
	return days + tail, nil
}
