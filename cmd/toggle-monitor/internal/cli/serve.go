package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/toggle-corp/toggle-monitor/internal/config"
	"github.com/toggle-corp/toggle-monitor/internal/db"
	"github.com/toggle-corp/toggle-monitor/internal/lifecycle"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the monitor service (default action)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			listenAddr, _ := cmd.Flags().GetString("listen")
			kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
			return runServeCLI(cmd.Context(), cfgPath, listenAddr, kubeconfig)
		},
	}
	cmd.Flags().String("config", "/etc/toggle-monitor/config.yaml", "path to the YAML config")
	cmd.Flags().String("listen", ":8080", "HTTP listen address")
	cmd.Flags().String("kubeconfig", "",
		"path to a kubeconfig file for auto-discovery; "+
			"empty defaults to in-cluster ServiceAccount (use $KUBECONFIG for the host-local file)")
	return cmd
}

// runServeCLI is the entrypoint for `toggle-monitor serve`. It reads
// the config file, resolves the DB password from the configured env
// var, and hands off to lifecycle.RunServe.
func runServeCLI(ctx context.Context, cfgPath, listenAddr, kubeconfigPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	dbCfg := db.Config{
		Host:     cfg.Database.Host,
		Port:     cfg.Database.Port,
		User:     cfg.Database.User,
		Password: pw,
		Name:     cfg.Database.Name,
		SSLMode:  cfg.Database.SSLMode,
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{}))

	// Fall back to $KUBECONFIG when --kubeconfig wasn't passed
	// explicitly — matches what kubectl does out of the box.
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("KUBECONFIG")
	}

	return lifecycle.RunServe(ctx, lifecycle.ServeOptions{
		Config:         cfg,
		DBConfig:       dbCfg,
		ListenAddr:     listenAddr,
		KubeconfigPath: kubeconfigPath,
		Logger:         logger,
	})
}
