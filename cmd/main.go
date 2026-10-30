package main

import (
	"os"

	"github.com/nivekalara237/localrectl/config"
	"github.com/nivekalara237/localrectl/internal/cli"
	"github.com/nivekalara237/localrectl/internal/logging"
	"github.com/spf13/cobra"
)

var rootCmd *cobra.Command

func init() {
	cobra.EnableTraverseRunHooks = true
	rootCmd = cli.NewRootCommand()
}

func main() {
	cmd, err := rootCmd.ExecuteC()
	if err == nil {
		return
	}
	logging.From(cmd.Context()).Error(err)
	if config.IsUsageError(err) {
		os.Exit(2)
	}
	os.Exit(1)
}
