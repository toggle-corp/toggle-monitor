package merger

import "testing"

// TestFormatFriendlyName covers every style enum value with examples
// drawn from a real cluster so the rendered output stays anchored to
// what an operator will actually see.
func TestFormatFriendlyName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		style     string
		ns, ing   string
		host      string
		multiHost bool
		want      string
	}{
		// ── plain ─────────────────────────────────────────────────────
		{"plain/argocd", "plain", "argocd", "argocd-server", "argocd.example.com", false,
			"(argocd) argocd-server"},
		{"plain/gamma", "plain", "gamma-web-app-2", "gamma-web-app-2-ingress",
			"web-app-2.example.com", false,
			"(gamma-web-app-2) gamma-web-app-2-ingress"},

		// ── compact (default) ─────────────────────────────────────────
		{"compact/argocd", "compact", "argocd", "argocd-server", "argocd.example.com", false,
			"(argocd) argocd-server"},
		{"compact/gamma", "compact", "gamma-web-app-2", "gamma-web-app-2-ingress",
			"web-app-2.example.com", false,
			"(gamma-web-app-2) gamma-web-app-2"},
		{"compact/redirect", "compact", "default", "argocd-redirect-dev-to-local-ingress",
			"argocd.example.com", false,
			"(default) argocd-redirect-dev-to-local"},
		{"compact/empty-suffix-fallback", "compact", "ns", "-ingress", "h.example.com", false,
			"(ns) -ingress"},

		// Empty string defaults to compact.
		{"default-style-is-compact", "", "argocd", "argocd-server-ingress",
			"argocd.example.com", false, "(argocd) argocd-server"},

		// ── dedupe ────────────────────────────────────────────────────
		{"dedupe/argocd", "dedupe", "argocd", "argocd-server", "argocd.example.com", false,
			"(argocd) server"},
		{"dedupe/core-token-strip", "dedupe", "betaco-core-backend-1", "core-backend-1-api",
			"core-api-1.example.com", false,
			"(betaco-core-backend-1) api"}, // leading core-backend-1 tokens all appear in ns
		{"dedupe/full-strip-falls-empty", "dedupe", "gamma-web-app-2", "gamma-web-app-2-ingress",
			"web-app-2.example.com", false,
			"(gamma-web-app-2)"}, // entire name was redundant
		{"dedupe/no-prefix-match", "dedupe", "acme-api-1", "go-minio-api",
			"minio.example.com", false,
			"(acme-api-1) go-minio-api"},

		// ── title ─────────────────────────────────────────────────────
		{"title/argocd", "title", "argocd", "argocd-server", "argocd.example.com", false,
			"(Argocd) Server"},
		{"title/core", "title", "betaco-core-backend-1", "core-backend-1-api",
			"core-api-1.example.com", false,
			"(Betaco Core Backend 1) Api"},
		{"title/redirect", "title", "default", "argocd-redirect-dev-to-local-ingress",
			"argocd.example.com", false,
			"(Default) Argocd Redirect Dev To Local"},

		// ── multi-host disambiguator ──────────────────────────────────
		{"compact/multi-host", "compact", "betaco-core", "core-backend-1-api",
			"core-api-1.betaco.example.com", true,
			"(betaco-core) core-backend-1-api · core-api-1"},
		{"title/multi-host", "title", "betaco-core", "core-backend-1-api",
			"core-api-1.betaco.example.com", true,
			"(Betaco Core) Backend 1 Api · Core Api 1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatFriendlyName(tc.style, tc.ns, tc.ing, tc.host, tc.multiHost)
			if got != tc.want {
				t.Errorf("style=%s ns=%s ing=%s host=%s multi=%v\n  got:  %q\n  want: %q",
					tc.style, tc.ns, tc.ing, tc.host, tc.multiHost, got, tc.want)
			}
		})
	}
}
