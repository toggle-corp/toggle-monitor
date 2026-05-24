package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationMarshalYAML(t *testing.T) {
	cases := []struct {
		name string
		in   Duration
		want string
	}{
		{"three days", Duration(72 * time.Hour), "3d"},
		{"fourteen days", Duration(336 * time.Hour), "14d"},
		{"thirty days", Duration(720 * time.Hour), "30d"},
		{"sub-day minutes", Duration(5 * time.Minute), "5m0s"},
		{"sub-day hours", Duration(2 * time.Hour), "2h0m0s"},
		{"zero", Duration(0), "0s"},
		{"day plus hour falls back", Duration(25 * time.Hour), "25h0m0s"},
		{"negative falls back", Duration(-72 * time.Hour), "-72h0m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.MarshalYAML()
			if err != nil {
				t.Fatalf("MarshalYAML: unexpected error: %v", err)
			}
			s, ok := got.(string)
			if !ok {
				t.Fatalf("MarshalYAML: expected string, got %T", got)
			}
			if s != tc.want {
				t.Errorf("MarshalYAML(%v) = %q, want %q", time.Duration(tc.in), s, tc.want)
			}
		})
	}
}

func TestDurationRoundTrip(t *testing.T) {
	cases := []string{"3d", "14d", "30d", "5m0s", "2h0m0s", "0s"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			var d Duration
			node := &yaml.Node{Kind: yaml.ScalarNode, Value: in}
			if err := d.UnmarshalYAML(node); err != nil {
				t.Fatalf("UnmarshalYAML(%q): %v", in, err)
			}
			got, err := d.MarshalYAML()
			if err != nil {
				t.Fatalf("MarshalYAML: %v", err)
			}
			if got.(string) != in {
				t.Errorf("round-trip %q -> %q", in, got)
			}
		})
	}
}
