package cli

import "github.com/spf13/cobra"

func newConfigCmd() *cobra.Command {
	cfg := &cobra.Command{
		Use:   "config",
		Short: "Config utilities",
	}
	cfg.AddCommand(newConfigShowCmd())
	return cfg
}

func newConfigShowCmd() *cobra.Command {
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the fully merged final config for one or all monitors",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "config show")
		},
	}
	show.Flags().String("monitor", "", "show only the given monitor slug")
	return show
}
