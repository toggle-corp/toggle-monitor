// Package config loads, validates, and merges the toggle-monitor YAML
// configuration. See docs/config-schema.md.
package config

// yaml.v3 is the parser used by the config loader. Imported here so the
// dependency is locked in go.mod before the loader lands in Issue 2.
import _ "gopkg.in/yaml.v3"
