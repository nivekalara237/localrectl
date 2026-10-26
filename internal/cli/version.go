package cli

import (
	"fmt"

	"github.com/nivekalara237/localrectl/internal/build"
	"github.com/spf13/cobra"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version",
		Aliases: []string{
			"v",
		},
		Short: "Get Version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(build.GetBuildInfo().ToString())
		},
	}
}
