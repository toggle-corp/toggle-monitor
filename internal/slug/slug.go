// Package slug enforces the toggle-monitor slug regex and provides
// sanitization for kube-discovered slugs. See docs/design-decisions.md.
package slug

import (
	"errors"
	"fmt"
	"regexp"
)

// MaxLen is the maximum permitted slug length in characters.
const MaxLen = 255

// pattern is the canonical slug regex from docs/config-schema.md:
// lowercase letters/digits, hyphen-separated, must start with a letter,
// no leading or trailing hyphen, no consecutive hyphens.
var pattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Validate reports whether s satisfies the toggle-monitor slug rules.
// Returns nil if valid, otherwise an error explaining the violation.
func Validate(s string) error {
	if s == "" {
		return errors.New("slug must not be empty")
	}
	if len(s) > MaxLen {
		return fmt.Errorf("slug %q exceeds max length of %d characters", s, MaxLen)
	}
	if !pattern.MatchString(s) {
		return fmt.Errorf("slug %q must match %s", s, pattern.String())
	}
	return nil
}

// SanitizeKubeDiscovered builds a kube-discovered monitor slug from the
// ingress namespace, name, and host per docs/design-decisions.md. Invalid
// characters in any input are replaced with "-"; consecutive hyphens are
// collapsed; leading/trailing hyphens are stripped. If the result is not
// a valid toggle-monitor slug, returns an error so the caller can record
// the ingress as kube-invalid with reason "slug generation failed".
func SanitizeKubeDiscovered(namespace, name, host string) (string, error) {
	parts := sanitizePart(namespace) + "-" + sanitizePart(name) + "-" + sanitizePart(host)
	parts = trimAndCollapseHyphens(parts)
	if parts == "" {
		return "", fmt.Errorf("slug generation failed: namespace, name, and host all sanitized to empty")
	}
	out := "kube-" + parts
	if err := Validate(out); err != nil {
		return "", fmt.Errorf("slug generation failed: %w", err)
	}
	return out, nil
}

func sanitizePart(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	return string(b)
}

func trimAndCollapseHyphens(s string) string {
	b := make([]byte, 0, len(s))
	prevHyphen := false
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			if !prevHyphen {
				b = append(b, '-')
				prevHyphen = true
			}
			continue
		}
		b = append(b, s[i])
		prevHyphen = false
	}
	// Trim leading/trailing hyphens.
	start, end := 0, len(b)
	for start < end && b[start] == '-' {
		start++
	}
	for end > start && b[end-1] == '-' {
		end--
	}
	return string(b[start:end])
}
