package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/toggle-corp/toggle-monitor/internal/config"
)

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
			cfgPath, _ := cmd.Flags().GetString("config")
			only, _ := cmd.Flags().GetString("monitor")
			return runConfigShowCLI(cfgPath, only, cmd.OutOrStdout())
		},
	}
	show.Flags().String("config", "/etc/toggle-monitor/config.yaml", "path to the YAML config")
	show.Flags().String("monitor", "", "show only the given monitor slug")
	return show
}

// runConfigShowCLI loads the config, validates it, and prints the
// fully resolved monitor block(s) as YAML. If --monitor is supplied
// and missing, exits non-zero with a clear error.
func runConfigShowCLI(path, only string, out io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := config.Load(data)
	if err != nil {
		return err
	}

	if only != "" {
		for _, m := range cfg.Monitors {
			if m.Slug == only {
				return writeMonitorYAML(out, m)
			}
		}
		return fmt.Errorf("monitor %q not found in config", only)
	}

	for i, m := range cfg.Monitors {
		if i > 0 {
			_, _ = io.WriteString(out, "---\n")
		}
		if err := writeMonitorYAML(out, m); err != nil {
			return err
		}
	}
	return nil
}

func writeMonitorYAML(out io.Writer, m config.Monitor) error {
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	defer enc.Close()
	return enc.Encode(m)
}
