package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// TestExplain_liveMode_withFakeClientset exercises the --ingress path
// without touching a real cluster. The fake clientset is wired through
// the explainOpts.clientFor seam — the same hook lets us avoid the
// kubeconfig precedence dance that defaultClientFor performs.
//
// The Ingress fixture has two hosts so we can also assert the --host
// filter narrows to a single report.
func TestExplain_liveMode_withFakeClientset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toggle-monitor.yaml")
	if err := os.WriteFile(cfgPath, []byte(liveExplainYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "acme-eoapi-3",
			Name:      "web",
			Labels:    map[string]string{"app.kubernetes.io/name": "minio"},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{Host: "api.example.com"},
				{Host: "admin.example.com"},
			},
		},
	}
	cs := fake.NewSimpleClientset(ing)
	fakeFor := func(_ string) (kubernetes.Interface, error) { return cs, nil }

	// --- no --host: emit both hosts separated by a YAML doc marker.
	var buf bytes.Buffer
	err := runExplainCLI(context.Background(), explainOpts{
		configPath: cfgPath,
		ingressRef: "acme-eoapi-3/web",
		out:        &buf,
		clientFor:  fakeFor,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"host: api.example.com",
		"host: admin.example.com",
		"\n---\n",
		"name: web",
		"namespace: acme-eoapi-3",
		"path: /minio/health/live", // minio leaf wins for both hosts
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in multi-host output:\n%s", want, out)
		}
	}

	// --- --host filter: only one report, no doc separator.
	buf.Reset()
	err = runExplainCLI(context.Background(), explainOpts{
		configPath: cfgPath,
		ingressRef: "acme-eoapi-3/web",
		host:       "admin.example.com",
		out:        &buf,
		clientFor:  fakeFor,
	})
	if err != nil {
		t.Fatalf("--host filter errored: %v\noutput:\n%s", err, buf.String())
	}
	out = buf.String()
	if strings.Contains(out, "api.example.com") {
		t.Errorf("--host filter should drop other hosts, got:\n%s", out)
	}
	if strings.Contains(out, "\n---\n") {
		t.Errorf("single-host output should have no doc separator, got:\n%s", out)
	}
}

// TestExplain_liveMode_unknownHostErrors ensures a typo'd --host
// fails loudly with the available hosts listed, rather than producing
// silent empty output.
func TestExplain_liveMode_unknownHostErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toggle-monitor.yaml")
	if err := os.WriteFile(cfgPath, []byte(liveExplainYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cs := fake.NewSimpleClientset(&networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ing"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "real.example.com"}},
		},
	})
	err := runExplainCLI(context.Background(), explainOpts{
		configPath: cfgPath,
		ingressRef: "ns/ing",
		host:       "wrong.example.com",
		out:        &bytes.Buffer{},
		clientFor:  func(_ string) (kubernetes.Interface, error) { return cs, nil },
	})
	if err == nil {
		t.Fatal("expected error for unknown --host")
	}
	if !strings.Contains(err.Error(), "real.example.com") {
		t.Errorf("error should list available hosts, got: %v", err)
	}
}

// liveExplainYAML mirrors the public test fixture but kept in this
// internal test file so the cli package self-tests don't pull in the
// cli_test fixture (different package, no symbol sharing).
const liveExplainYAML = `
displayTimezone: Asia/Kathmandu
dbBodyMaxChars: 4000
database:
  host: pg
  port: 5432
  user: tm
  name: tm
  sslMode: require
  passwordEnv: DB_PASSWORD
ui:
  pageSize:
    homepageAlerts: 20
    monitorListing: 50
    monitorHistory: 50
    discoveryListing: 50
  maxPerPage: 200
httpClient:
  userAgent: "toggle-monitor/cli-test"
slack:
  bodyMaxChars: 200
  channels:
    - slug: ops-alerts
      channelId: C0123ABCD
      tokenEnv: SLACK_BOT_TOKEN
monitors: []
kube:
  resyncInterval: 30m
  match:
    - when: {}
      config:
        scheme: https
        path: /
        httpMethod: GET
        acceptedStatusCodes: [200]
        interval: 5m
        timeout: 30s
        retries: 2
        retryBackoff: 5s
        followRedirects: true
        reminderInterval: 3d
        sslAlertThreshold: 14d
        sslEscalationThreshold: 7d
        sslReminderInterval: 1d
        slack: ops-alerts
    - when: {labels: {"app.kubernetes.io/name": "minio"}}
      config:
        path: /minio/health/live
`
