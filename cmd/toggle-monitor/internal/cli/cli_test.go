package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/cmd/toggle-monitor/internal/cli"
)

// run executes the root command with the given args and returns whatever
// it wrote to stdout/stderr and the exit error (if any). Mirrors what
// `main` does in production.
func run(args ...string) (string, error) {
	root := cli.NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestStubSubcommandsPrintNotYetImplemented covers the CLI surfaces
// that are still placeholders (validate, config show). Once their
// real implementations land (Issue 6), these cases shift to the
// "real-but-fails-gracefully" group below.
func TestStubSubcommandsPrintNotYetImplemented(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"validate", []string{"validate", "config.yaml"}, "validate: not yet implemented"},
		{"config show", []string{"config", "show"}, "config show: not yet implemented"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := run(tc.args...)
			if err != nil {
				t.Fatalf("execute %v: unexpected error: %v", tc.args, err)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("execute %v: output %q does not contain %q", tc.args, out, tc.want)
			}
		})
	}
}

// TestRealSubcommandsFailGracefullyOnBogusConfig covers surfaces that
// now wire real behavior (serve, migrate). They expect a config file
// and should error cleanly when one isn't supplied — not panic, not
// hang.
func TestRealSubcommandsFailGracefullyOnBogusConfig(t *testing.T) {
	t.Parallel()
	cases := []struct{ name string; args []string }{
		{"serve", []string{"serve", "--config", "/nonexistent/toggle-monitor.yaml"}},
		{"migrate", []string{"migrate", "--config", "/nonexistent/toggle-monitor.yaml"}},
		{"migrate --check", []string{"migrate", "--config", "/nonexistent/toggle-monitor.yaml", "--check"}},
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
