package slug_test

import (
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/slug"
)

func TestValidate_acceptsSimpleLowercaseSlug(t *testing.T) {
	if err := slug.Validate("ops-alerts"); err != nil {
		t.Fatalf("expected ops-alerts to be valid, got %v", err)
	}
}

func TestValidate_rejectsEmpty(t *testing.T) {
	if err := slug.Validate(""); err == nil {
		t.Fatal("expected empty string to be rejected")
	}
}

func TestValidate_rejectsStartsWithDigit(t *testing.T) {
	if err := slug.Validate("1abc"); err == nil {
		t.Fatal("expected 1abc to be rejected (must start with letter)")
	}
}

// TestValidate_rules pins the documented slug regex against the rule set
// in docs/config-schema.md. Each case exercises observable validation
// behavior — accept/reject — not implementation details.
func TestValidate_rules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		slug  string
		valid bool
	}{
		// Accepted forms.
		{"single letter", "a", true},
		{"letter then digit", "a1", true},
		{"long valid", "kube-default-foo-example-com", true},
		{"max length", string(append([]byte{'a'}, bytes('b', slug.MaxLen-1)...)), true},

		// Rejected forms.
		{"uppercase", "OpsAlerts", false},
		{"leading hyphen", "-foo", false},
		{"trailing hyphen", "foo-", false},
		{"consecutive hyphens", "foo--bar", false},
		{"underscore", "foo_bar", false},
		{"space", "foo bar", false},
		{"dot", "foo.bar", false},
		{"unicode letter", "fëo", false},
		{"over max length", string(append([]byte{'a'}, bytes('b', slug.MaxLen)...)), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := slug.Validate(tc.slug)
			if tc.valid && err != nil {
				t.Errorf("Validate(%q) = %v; want nil", tc.slug, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("Validate(%q) = nil; want non-nil error", tc.slug)
			}
		})
	}
}

// bytes returns a slice of n copies of c.
func bytes(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}

func TestSanitizeKubeDiscovered_happyPath(t *testing.T) {
	got, err := slug.SanitizeKubeDiscovered("default", "foo", "foo.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "default__foo__foo-example-com"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestSanitizeKubeDiscovered_rules pins the per-part sanitization
// rules from ADR-0002 §Identity. Each (namespace, ingress-name, host)
// part is independently sanitized — lowercase, non-alnum → '-',
// consecutive hyphens collapse, leading/trailing trim — and then the
// three sanitized parts are joined with `__`.
func TestSanitizeKubeDiscovered_rules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		ns, ing, h string
		want       string // empty string means "expect error"
	}{
		{"basic single-segment host", "default", "foo", "bar", "default__foo__bar"},
		{"host with dots", "default", "foo", "foo.example.com", "default__foo__foo-example-com"},
		{"uppercase normalized", "ProdNS", "MyApp", "API.Example.COM", "prodns__myapp__api-example-com"},
		{"underscores in inputs become hyphens", "kube_system", "my_app", "api.example.com", "kube-system__my-app__api-example-com"},
		{"consecutive invalid collapsed", "ns", "name", "a..b", "ns__name__a-b"},
		{"trailing dot stripped", "ns", "name", "host.", "ns__name__host"},
		{"empty namespace still ok", "", "name", "host", "__name__host"},
		{"all-invalid inputs fail", "...", "...", "...", ""},
		{"empty everything fails", "", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := slug.SanitizeKubeDiscovered(tc.ns, tc.ing, tc.h)
			if tc.want == "" {
				if err == nil {
					t.Errorf("Sanitize(%q,%q,%q) = %q, nil; want error", tc.ns, tc.ing, tc.h, got)
				}
				return
			}
			if err != nil {
				t.Errorf("Sanitize(%q,%q,%q) returned error %v; want %q", tc.ns, tc.ing, tc.h, err, tc.want)
				return
			}
			if got != tc.want {
				t.Errorf("Sanitize(%q,%q,%q) = %q; want %q", tc.ns, tc.ing, tc.h, got, tc.want)
			}
		})
	}
}
