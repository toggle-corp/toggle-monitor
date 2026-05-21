// Package secret provides SecretString, a slog-safe wrapper that masks
// values in log output. The masking rules come from docs/config-schema.md:
//
//   - length >= 8: <first2>****<last2>   e.g. "SUPER_STRONG_PASSWORD" → "SU****RD"
//   - length < 8 : ****                  (too short to safely reveal any chars)
//
// The asterisk count is fixed at 4 regardless of the underlying length
// so logs never leak the true secret length.
package secret

import "log/slog"

// SecretString is a string that masks itself when written through slog.
// Use Reveal() explicitly when you need the underlying value (e.g.
// when building a DSN or Slack API call).
type SecretString string

// LogValue implements slog.LogValuer.
func (s SecretString) LogValue() slog.Value {
	return slog.StringValue(s.masked())
}

// String mirrors LogValue so fmt.Print et al. also avoid leaking.
func (s SecretString) String() string {
	return s.masked()
}

// Reveal returns the underlying string. Callers must take care not to
// log the result. Reserved for explicit hand-off to libraries that
// need the raw value (DSN builder, HTTP Authorization header, etc.).
func (s SecretString) Reveal() string {
	return string(s)
}

// masked applies the documented rules.
func (s SecretString) masked() string {
	if len(s) < 8 {
		return "****"
	}
	return string(s[:2]) + "****" + string(s[len(s)-2:])
}
