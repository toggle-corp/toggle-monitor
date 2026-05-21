package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/toggle-corp/toggle-monitor/cmd/toggle-monitor/internal/cli"
)

// run executes the root command with the given args and returns whatever
// it wrote to stdout and the exit error (if any). It mirrors what `main`
// does in production.
func run(args ...string) (string, error) {
	root := cli.NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestSubcommandsStubbed verifies every documented CLI surface from
// Issue 1 prints "not yet implemented" and exits cleanly.
func TestSubcommandsStubbed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"default (serve)", []string{}, "serve: not yet implemented"},
		{"serve", []string{"serve"}, "serve: not yet implemented"},
		{"validate", []string{"validate", "config.yaml"}, "validate: not yet implemented"},
		{"config show", []string{"config", "show"}, "config show: not yet implemented"},
		{"migrate", []string{"migrate"}, "migrate: not yet implemented"},
		{"migrate --check", []string{"migrate", "--check"}, "migrate --check: not yet implemented"},
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
