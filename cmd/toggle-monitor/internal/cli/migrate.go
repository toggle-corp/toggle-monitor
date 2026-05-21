package cli

import "github.com/spf13/cobra"

func newMigrateCmd() *cobra.Command {
	m := &cobra.Command{
		Use:   "migrate",
		Short: "Apply schema migrations (or verify with --check)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			check, _ := cmd.Flags().GetBool("check")
			if check {
				return notImplemented(cmd.OutOrStdout(), "migrate --check")
			}
			return notImplemented(cmd.OutOrStdout(), "migrate")
		},
	}
	m.Flags().Bool("check", false, "verify pending migrations without applying")
	return m
}
