package cli_test

import (
	"strings"
	"testing"
)

// explainYAML is a minimal cascade config used by the explain tests.
// It carries a root rule with every required field, a namespace-glob
// branch that adds a notify entry, and a labels-selected leaf with
// final:true plus a path override. Together these exercise the rule-
// chain rendering and the union / replace branches of the merger.
const explainYAML = `
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
theme:
  defaultGroupColor: "#64748b"
httpClient:
  userAgent: "toggle-monitor/cli-test"
slack:
  bodyMaxChars: 200
  userMapping:
    alice: U01ALICE0
    bob: U01BOB000
  channels:
    - slug: ops-alerts
      channelId: C0123ABCD
      tokenEnv: SLACK_BOT_TOKEN
groups:
  - slug: kube-discovered
    friendlyName: Kube Discovered
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
        notify: [alice]
    - when: {namespace: "acme-*"}
      config:
        notify: [bob]
      nested:
        - when: {labels: {"app.kubernetes.io/name": "minio"}}
          final: true
          config:
            path: /minio/health/live
    - when: {namespace: "ignored-*"}
      ignore: true
`

func TestExplain_hypotheticalMaterialized(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	out, err := run(
		"explain",
		"--config", path,
		"--namespace", "acme-eoapi-3",
		"--labels", "app.kubernetes.io/name=minio",
		"--host", "api.example.com",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	// Identity block reflects the synthetic Ingress.
	for _, want := range []string{
		"namespace: acme-eoapi-3",
		"host: api.example.com",
		"app.kubernetes.io/name: minio",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// Rule chain matches the merger's selectorSummary formatting.
	for _, want := range []string{
		"- match[0] ()",
		"- match[1] (ns=acme-*)",
		"- match[1].nested[0] (labels.app.kubernetes.io/name=minio) [final]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing chain step %q in output:\n%s", want, out)
		}
	}
	// Resolved values: minio leaf's path wins; notify unions root + ns rule.
	for _, want := range []string{
		"path: /minio/health/live",
		"- alice",
		"- bob",
		"outcome: materialized",
		"slug: acme-eoapi-3__hypothetical__api-example-com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestExplain_hypotheticalIgnored(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	out, err := run(
		"explain",
		"--config", path,
		"--namespace", "ignored-foo",
		"--host", "x.example.com",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "outcome: ignored") {
		t.Errorf("expected ignored outcome, got:\n%s", out)
	}
	if strings.Contains(out, "resolved:") {
		t.Errorf("ignored outcome should omit resolved block, got:\n%s", out)
	}
}

func TestExplain_emptyLabelsParses(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	// Empty --labels is the common case for hypothetical lookups
	// against a namespace+host pair without a label discriminator.
	// The flag must accept "" without raising a parse error.
	out, err := run(
		"explain",
		"--config", path,
		"--namespace", "other-ns",
		"--labels", "",
		"--host", "h.example.com",
	)
	if err != nil {
		t.Fatalf("expected empty --labels to be accepted, got: %v\n%s", err, out)
	}
	if !strings.Contains(out, "outcome: materialized") {
		t.Errorf("expected materialized outcome (only root rule matches), got:\n%s", out)
	}
}

func TestExplain_mutuallyExclusiveModes(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	_, err := run(
		"explain",
		"--config", path,
		"--ingress", "ns/name",
		"--namespace", "ns",
	)
	if err == nil {
		t.Fatal("expected error when both --ingress and --namespace are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should explain the conflict, got: %v", err)
	}
}

func TestExplain_requiresAnyMode(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	_, err := run("explain", "--config", path)
	if err == nil {
		t.Fatal("expected error when neither mode is selected")
	}
}

func TestExplain_hypotheticalRequiresHost(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	_, err := run("explain", "--config", path, "--namespace", "ns")
	if err == nil {
		t.Fatal("expected error when hypothetical mode is missing --host")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("error should mention --host, got: %v", err)
	}
}

func TestExplain_labelsMalformed(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	_, err := run(
		"explain",
		"--config", path,
		"--namespace", "ns",
		"--host", "h.example.com",
		"--labels", "novalue",
	)
	if err == nil {
		t.Fatal("expected error when --labels entry has no '='")
	}
	if !strings.Contains(err.Error(), "key=value") {
		t.Errorf("error should mention key=value, got: %v", err)
	}
}

func TestExplain_invalidIngressRef(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, explainYAML)
	_, err := run("explain", "--config", path, "--ingress", "no-slash")
	if err == nil {
		t.Fatal("expected error for malformed --ingress")
	}
	if !strings.Contains(err.Error(), "<namespace>/<name>") {
		t.Errorf("error should show expected format, got: %v", err)
	}
}
