package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/migrate"
)

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply schema migrations (or verify with --check)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			check, _ := cmd.Flags().GetBool("check")
			return runMigrateCLI(cfgPath, check)
		},
	}
	cmd.Flags().String("config", "/etc/toggle-monitor/config.yaml", "path to the YAML config")
	cmd.Flags().Bool("check", false, "verify pending migrations without applying")
	return cmd
}

func runMigrateCLI(cfgPath string, checkOnly bool) error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	cfg, err := config.Load(data)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pw := os.Getenv(cfg.Database.PasswordEnv)
	if pw == "" {
		return fmt.Errorf("env var %q (named by database.passwordEnv) is not set", cfg.Database.PasswordEnv)
	}
	dsn := db.Config{
		Host:     cfg.Database.Host,
		Port:     cfg.Database.Port,
		User:     cfg.Database.User,
		Password: pw,
		Name:     cfg.Database.Name,
		SSLMode:  cfg.Database.SSLMode,
	}.DSN()

	if checkOnly {
		if err := migrate.Check(dsn); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "schema is at the latest version")
		return nil
	}
	if err := migrate.Up(dsn); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "migrations applied")
	return nil
}
