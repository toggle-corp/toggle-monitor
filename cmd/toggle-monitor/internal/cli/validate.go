package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a config file (pre-push CI check)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runValidateCLI(args[0])
		},
	}
}

// runValidateCLI loads and validates the YAML at path. Silent + exit 0
// on success; multi-error on failure (with line numbers) is returned
// up to cobra so the process exits non-zero.
func runValidateCLI(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	if _, err := config.Load(data); err != nil {
		return err
	}
	return nil
}
