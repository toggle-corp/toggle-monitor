// Package cli wires the cobra command tree for the toggle-monitor
// binary. Each subcommand is exposed as a constructor so tests can
// invoke them with an injected output writer.
package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// NewRootCmd returns the configured root command. The default action
// (no subcommand) delegates to `serve`.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "toggle-monitor",
		Short: "Kubernetes-native uptime and SSL monitor",
		Long: "toggle-monitor watches a configured set of HTTP endpoints " +
			"and every Ingress in the cluster, posting Slack alerts on " +
			"state changes and surfacing current state in a read-only UI.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	serve := newServeCmd()
	// Mirror serve's RunE so `toggle-monitor` with no subcommand starts
	// the service (matching the design's "default action is serve").
	root.RunE = serve.RunE
	root.Flags().AddFlagSet(serve.Flags())

	root.AddCommand(serve)
	root.AddCommand(newValidateCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newMigrateCmd())
	return root
}

// notImplemented writes the standard placeholder message and returns
// nil so a stub subcommand exits 0. Used by subcommands still on the
// roadmap (validate, config show land in Issue 6).
func notImplemented(w io.Writer, name string) error {
	_, err := io.WriteString(w, name+": not yet implemented\n")
	return err
}
