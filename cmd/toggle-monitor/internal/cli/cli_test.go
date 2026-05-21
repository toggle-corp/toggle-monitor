package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/cmd/toggle-monitor/internal/cli"
)

// run executes the root command with the given args and returns
// stdout/stderr plus the exit error, mirroring what `main` does.
func run(args ...string) (string, error) {
	root := cli.NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// validYAML returns a minimal YAML config string for the CLI tests.
// Mirrors internal/config/config_test.go's validMinimal but lives
// here so the cli package doesn't need an internal-test dependency.
const validYAML = `
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
  channels:
    - slug: ops-alerts
      channelId: C0123ABCD
      tokenEnv: SLACK_BOT_TOKEN
groups:
  - slug: kube-discovered
    friendlyName: Kube Discovered
  - slug: gw
    friendlyName: GW
monitors:
  - slug: bastion
    friendlyName: Bastion
    url: http://bastion.local/health
    group: gw
    httpMethod: GET
    acceptedStatusCodes: [200]
    interval: 5m
    timeout: 10s
    retries: 2
    retryBackoff: 5s
    followRedirects: false
    reminderInterval: 3d
    slack: ops-alerts
`

func writeTempYAML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "toggle-monitor.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return path
}

func TestValidate_silentOnValidConfig(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, validYAML)
	out, err := run("validate", path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected no output on valid config, got: %q", out)
	}
}

func TestValidate_emitsErrorWithLineNumberOnInvalidConfig(t *testing.T) {
	t.Parallel()
	bad := strings.Replace(validYAML, "group: gw", "group: nope", 1)
	path := writeTempYAML(t, bad)
	_, err := run("validate", path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown group") {
		t.Errorf("error should describe the violation, got: %v", err)
	}
	if !strings.Contains(msg, "line ") {
		t.Errorf("error should include a line number, got: %v", err)
	}
}

func TestConfigShow_printsAllMonitors(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, validYAML)
	out, err := run("config", "show", "--config", path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"slug: bastion", "url: http://bastion.local/health"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestConfigShow_filtersBySlug(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, validYAML)
	out, err := run("config", "show", "--config", path, "--monitor", "bastion")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "slug: bastion") {
		t.Errorf("expected bastion in output, got:\n%s", out)
	}
}

func TestConfigShow_errorsOnUnknownSlug(t *testing.T) {
	t.Parallel()
	path := writeTempYAML(t, validYAML)
	_, err := run("config", "show", "--config", path, "--monitor", "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown slug")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing slug, got: %v", err)
	}
}

// TestRealSubcommandsFailGracefullyOnBogusConfig covers serve, migrate
// (--check), and validate when the config file doesn't exist.
func TestRealSubcommandsFailGracefullyOnBogusConfig(t *testing.T) {
	t.Parallel()
	cases := []struct{ name string; args []string }{
		{"serve", []string{"serve", "--config", "/nonexistent/toggle-monitor.yaml"}},
		{"migrate", []string{"migrate", "--config", "/nonexistent/toggle-monitor.yaml"}},
		{"migrate --check", []string{"migrate", "--config", "/nonexistent/toggle-monitor.yaml", "--check"}},
		{"validate", []string{"validate", "/nonexistent/toggle-monitor.yaml"}},
		{"config show", []string{"config", "show", "--config", "/nonexistent/toggle-monitor.yaml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := run(tc.args...)
			if err == nil {
				t.Fatalf("expected an error for missing config, got nil")
			}
			if !strings.Contains(err.Error(), "read config") && !strings.Contains(err.Error(), "no such file") {
				t.Fatalf("expected a 'read config' error, got: %v", err)
			}
		})
	}
}

// TestHelpListsAllSubcommands guards the Issue 1 acceptance criterion
// that --help mentions every documented subcommand.
func TestHelpListsAllSubcommands(t *testing.T) {
	t.Parallel()
	out, err := run("--help")
	if err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	for _, name := range []string{"serve", "validate", "config", "migrate"} {
		if !strings.Contains(out, name) {
			t.Errorf("--help output missing subcommand %q; got:\n%s", name, out)
		}
	}
}
