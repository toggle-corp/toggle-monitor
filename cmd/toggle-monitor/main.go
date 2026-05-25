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

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=v0.2.3"
//
// It's stamped onto every Sentry event as the release.
var version = "dev"

func main() {
	if err := cli.NewRootCmd(cli.BuildInfo{Version: version}).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
