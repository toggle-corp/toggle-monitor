package cli

import (
	"io"

	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the monitor service (default action)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.OutOrStdout())
		},
	}
}

func runServe(w io.Writer) error {
	return notImplemented(w, "serve")
}
