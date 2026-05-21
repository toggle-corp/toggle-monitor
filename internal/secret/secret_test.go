package secret_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/internal/secret"
)

func TestSecretString_LogValue_longValue(t *testing.T) {
	s := secret.SecretString("SUPER_STRONG_PASSWORD")
	v := s.LogValue().String()
	if v != "SU****RD" {
		t.Errorf("LogValue: got %q, want %q", v, "SU****RD")
	}
}

func TestSecretString_LogValue_shortValue(t *testing.T) {
	cases := []string{"x", "abc", "1234567"} // every length < 8
	for _, in := range cases {
		v := secret.SecretString(in).LogValue().String()
		if v != "****" {
			t.Errorf("LogValue(%q): got %q, want ****", in, v)
		}
	}
}

func TestSecretString_LogValue_emptyValue(t *testing.T) {
	if got := secret.SecretString("").LogValue().String(); got != "****" {
		t.Errorf("LogValue(empty): got %q, want ****", got)
	}
}

func TestSecretString_LogValue_exactlyEight(t *testing.T) {
	// Length-8 is the boundary: per spec, length >= 8 uses the
	// first2/****/last2 form.
	got := secret.SecretString("12345678").LogValue().String()
	if got != "12****78" {
		t.Errorf("LogValue(8 chars): got %q, want %q", got, "12****78")
	}
}

func TestSecretString_doesNotLeakViaSlog(t *testing.T) {
	// Real test of intent: when a SecretString lands in an slog
	// attribute, the underlying string must never appear in the
	// rendered output.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{}))
	logger.Info("connecting", "password", secret.SecretString("SUPER_STRONG_PASSWORD"))
	out := buf.String()
	if strings.Contains(out, "SUPER_STRONG_PASSWORD") {
		t.Errorf("slog output leaked the secret: %s", out)
	}
	if !strings.Contains(out, "SU****RD") {
		t.Errorf("slog output should contain the masked form, got: %s", out)
	}
}

func TestSecretString_Reveal_returnsUnderlying(t *testing.T) {
	const raw = "SUPER_STRONG_PASSWORD"
	if got := secret.SecretString(raw).Reveal(); got != raw {
		t.Errorf("Reveal: got %q, want %q", got, raw)
	}
}
