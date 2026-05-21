// Command toggle-monitor is the single-binary entrypoint for the
// monitoring tool. Default invocation (no subcommand) starts the
// service; subcommands provide CLI utilities (validate, config show,
// migrate).
package main

import (
	"fmt"
	"os"

	"github.com/toggle-corp/toggle-monitor/cmd/toggle-monitor/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
