package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// interpolate expands ${VAR} / ${VAR:-fallback} / $$ in YAML bytes
// before they reach the parser. Mirrors docker-compose semantics.
//
// Rules (see docs/config-schema.md §"Env var interpolation"):
//   - ${VAR}             strict; error (with line number) if VAR is unset.
//                         A VAR that is set but empty is allowed (yields "").
//   - ${VAR:-fallback}   use fallback if VAR is unset OR empty.
//   - $$                 escape: emits a literal "$".
//
// Anything else after "$" passes through verbatim — including bare
// "$foo" identifiers (no shell-style expansion).
func interpolate(input []byte) ([]byte, error) {
	var errs []error
	out := make([]byte, 0, len(input))
	line := 1
	for i := 0; i < len(input); {
		c := input[i]
		if c == '\n' {
			line++
			out = append(out, '\n')
			i++
			continue
		}
		if c != '$' {
			out = append(out, c)
			i++
			continue
		}
		// c == '$'; look ahead.
		if i+1 < len(input) && input[i+1] == '$' {
			out = append(out, '$')
			i += 2
			continue
		}
		if i+1 < len(input) && input[i+1] == '{' {
			end := indexCloseBrace(input, i+2)
			if end < 0 {
				errs = append(errs, fmt.Errorf("line %d: unterminated ${...}", line))
				out = append(out, c)
				i++
				continue
			}
			expr := string(input[i+2 : end])
			val, err := resolveInterp(expr)
			if err != nil {
				errs = append(errs, fmt.Errorf("line %d: %w", line, err))
				i = end + 1
				continue
			}
			out = append(out, []byte(val)...)
			i = end + 1
			continue
		}
		// Lone '$' with no expansion follower — emit as-is.
		out = append(out, c)
		i++
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func indexCloseBrace(b []byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == '}' {
			return i
		}
		if b[i] == '\n' {
			return -1
		}
	}
	return -1
}

// resolveInterp evaluates a single ${expr} body.
func resolveInterp(expr string) (string, error) {
	if i := strings.Index(expr, ":-"); i >= 0 {
		name := expr[:i]
		fallback := expr[i+2:]
		if err := validateEnvVarName(name); err != nil {
			return "", err
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v, nil
		}
		return fallback, nil
	}
	if err := validateEnvVarName(expr); err != nil {
		return "", err
	}
	if v, ok := os.LookupEnv(expr); ok {
		return v, nil
	}
	return "", fmt.Errorf("env var %q is not set", expr)
}

func validateEnvVarName(name string) error {
	if !envVarNamePattern.MatchString(name) {
		return fmt.Errorf("invalid env var name %q (must match ^[A-Z][A-Z0-9_]*$)", name)
	}
	return nil
}
