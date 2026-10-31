package main

import (
	"os"

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
	if err != nil {
		logging.From(cmd.Context()).Error(err)
	}
	os.Exit(cli.ExitCode(cmd, err))
}
