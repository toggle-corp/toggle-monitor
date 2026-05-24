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
// ingress namespace, name, and host per ADR-0002 §Identity. The format
// is `<namespace>__<ingress-name>__<host>`; the double-underscore
// separator avoids collision with hyphen-bearing content in any of the
// three parts (typical k8s names use single hyphens, hosts use dots
// that we lower to single hyphens).
//
// Each part is independently sanitized: lower-cased, any character
// outside [a-z0-9-] becomes '-', consecutive hyphens collapse, and
// leading/trailing hyphens are trimmed. The three sanitized parts are
// joined with `__`. An empty result (every part collapsed to nothing,
// or only the separators left) is an error — the caller records the
// ingress as kube-invalid with reason "slug generation failed".
//
// Output is NOT subject to Validate() — that regex is the rule for
// human-authored slugs (lowercase, hyphen-separated, no `_`). Kube
// slugs intentionally carry `__` to keep their structure parseable
// back to (namespace, name, host).
func SanitizeKubeDiscovered(namespace, name, host string) (string, error) {
	ns := trimAndCollapseHyphens(sanitizePart(namespace))
	nm := trimAndCollapseHyphens(sanitizePart(name))
	ho := trimAndCollapseHyphens(sanitizePart(host))
	if ns == "" && nm == "" && ho == "" {
		return "", fmt.Errorf("slug generation failed: namespace, name, and host all sanitized to empty")
	}
	// At least one of the three parts must be non-empty for the slug to
	// be useful as identity; allow individual parts to be empty (e.g. a
	// rare cluster-scoped ingress with no namespace) and render them as
	// adjacent separators so the structure stays positional.
	out := ns + "__" + nm + "__" + ho
	// Sanity: a slug consisting purely of separators (every part empty
	// except for the one we already rejected above) shouldn't get
	// through. The Validate-like character check is intentional but
	// loose — `_`, `-`, lowercase alnum only.
	for i := 0; i < len(out); i++ {
		c := out[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return "", fmt.Errorf("slug generation failed: unexpected character %q in %q", c, out)
		}
	}
	if len(out) > MaxLen {
		return "", fmt.Errorf("slug generation failed: %q exceeds max length %d", out, MaxLen)
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
