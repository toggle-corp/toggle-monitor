// Package cli wires the cobra command tree for the toggle-monitor
// binary. Each subcommand is exposed as a constructor so tests can
// invoke them with an injected output writer.
package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// NewRootCmd returns the configured root command. Default action (no
// subcommand) is "serve".
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "toggle-monitor",
		Short: "Kubernetes-native uptime and SSL monitor",
		Long: "toggle-monitor watches a configured set of HTTP endpoints " +
			"and every Ingress in the cluster, posting Slack alerts on " +
			"state changes and surfacing current state in a read-only UI.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Default action (no subcommand) is "serve".
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.OutOrStdout())
		},
	}
	root.AddCommand(newServeCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newMigrateCmd())
	return root
}

// notImplemented writes the standard placeholder message and returns
// nil so the subcommand exits 0 (per Issue 1 acceptance criteria).
func notImplemented(w io.Writer, name string) error {
	_, err := io.WriteString(w, name+": not yet implemented\n")
	return err
}
