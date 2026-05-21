package cli

import "github.com/spf13/cobra"

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a config file (pre-push CI check)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "validate")
		},
	}
}
